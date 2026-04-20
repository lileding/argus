//! Feishu WebSocket event stream: connect, receive events, auto-reconnect.
//!
//! The WebSocket is receive-only (events from Feishu). Sending messages
//! back to users is done via the REST API ([`crate::api::Api`]).

use std::collections::HashMap;
use std::time::{Duration, Instant};

use futures_util::{SinkExt, StreamExt};
use prost::Message as ProstMessage;
use tokio::net::TcpStream;
use tokio_tungstenite::tungstenite::Message as WsMessage;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};
use tracing;

use crate::auth::Auth;
use crate::pbbp2::{self, Frame};
use crate::types::{ClientConfig, EndpointResponse, Error, EventEnvelope, FeishuEvent, Result};

type WsStream = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// Parsed connection parameters from the endpoint URL.
struct ConnParams {
    url: String,
    service_id: i32,
    config: ClientConfig,
}

/// Fragment cache entry.
struct FragmentEntry {
    parts: HashMap<usize, Vec<u8>>,
    total: usize,
    created: Instant,
}

const FRAGMENT_TTL: Duration = Duration::from_secs(60);

/// Fragment reassembler — extracted for testability without a WS connection.
struct Reassembler {
    fragments: HashMap<String, FragmentEntry>,
}

impl Reassembler {
    fn new() -> Self {
        Self {
            fragments: HashMap::new(),
        }
    }

    /// Process a frame and return the complete payload when all fragments are collected.
    fn process(&mut self, frame: &Frame) -> Result<Option<Vec<u8>>> {
        // GC stale fragments.
        self.fragments
            .retain(|_, entry| entry.created.elapsed() < FRAGMENT_TTL);

        let sum: usize = frame
            .header(pbbp2::HEADER_SUM)
            .and_then(|v| v.parse().ok())
            .unwrap_or(1);
        let seq: usize = frame
            .header(pbbp2::HEADER_SEQ)
            .and_then(|v| v.parse().ok())
            .unwrap_or(0);

        let payload = frame.payload.clone().unwrap_or_default();

        if sum <= 1 {
            return Ok(Some(payload));
        }

        let msg_id = frame
            .header(pbbp2::HEADER_MESSAGE_ID)
            .unwrap_or("")
            .to_string();

        let entry = self
            .fragments
            .entry(msg_id.clone())
            .or_insert_with(|| FragmentEntry {
                parts: HashMap::new(),
                total: sum,
                created: Instant::now(),
            });

        entry.parts.insert(seq, payload);

        if entry.parts.len() == entry.total {
            // Safe: we just accessed the entry above via `self.fragments.entry()`.
            let Some(entry) = self.fragments.remove(&msg_id) else {
                return Ok(None);
            };
            let mut full = Vec::new();
            for i in 0..entry.total {
                if let Some(part) = entry.parts.get(&i) {
                    full.extend_from_slice(part);
                }
            }
            Ok(Some(full))
        } else {
            tracing::trace!(
                message_id = %msg_id,
                got = entry.parts.len(),
                total = entry.total,
                "fragment cached, waiting for more"
            );
            Ok(None)
        }
    }
}

/// A WebSocket event stream from Feishu.
///
/// Created via [`EventStream::connect`]. Use [`EventStream::next_event`] to
/// receive events. Handles ping/pong, fragment reassembly, and ACK internally.
/// Reconnection is the caller's responsibility (see [`EventStream::connect`] errors).
pub struct EventStream {
    ws: WsStream,
    service_id: i32,
    config: ClientConfig,
    reassembler: Reassembler,
    last_ping: Instant,
}

impl EventStream {
    /// Connect to Feishu WebSocket. This does NOT auto-reconnect — the caller
    /// should loop on connect + next_event, handling reconnection themselves.
    pub async fn connect(auth: &Auth) -> Result<Self> {
        let params = get_conn_params(auth).await?;

        tracing::info!(url = %params.url, service_id = params.service_id, "connecting to Feishu WS");

        let (ws, _) = tokio_tungstenite::connect_async(&params.url).await?;

        tracing::info!("Feishu WS connected");

        Ok(Self {
            ws,
            service_id: params.service_id,
            config: params.config,
            reassembler: Reassembler::new(),
            last_ping: Instant::now(),
        })
    }

    /// Get a reference to the current client config (server-negotiated).
    pub fn config(&self) -> &ClientConfig {
        &self.config
    }

    /// Receive the next event. This blocks until an event arrives, sending
    /// pings as needed. Returns `None` when the connection is closed.
    ///
    /// Handles internally: ping/pong, fragment reassembly, ACK.
    pub async fn next_event(&mut self) -> Option<FeishuEvent> {
        loop {
            let ping_deadline = self.last_ping + Duration::from_secs(self.config.ping_interval);
            let now = Instant::now();
            let sleep_dur = if now >= ping_deadline {
                Duration::ZERO
            } else {
                ping_deadline - now
            };

            tokio::select! {
                _ = tokio::time::sleep(sleep_dur) => {
                    if let Err(e) = self.send_ping().await {
                        tracing::warn!("ping failed: {e}");
                        return None;
                    }
                }
                msg = self.ws.next() => {
                    match msg {
                        Some(Ok(WsMessage::Binary(data))) => {
                            match self.handle_binary(&data).await {
                                Ok(Some(event)) => return Some(event),
                                Ok(None) => continue, // control frame or incomplete fragment
                                Err(e) => {
                                    tracing::warn!("frame handling error: {e}");
                                    continue;
                                }
                            }
                        }
                        Some(Ok(WsMessage::Close(_))) | None => {
                            tracing::info!("Feishu WS connection closed");
                            return None;
                        }
                        Some(Ok(_)) => continue, // text, ping, pong — ignore
                        Some(Err(e)) => {
                            tracing::warn!("WS read error: {e}");
                            return None;
                        }
                    }
                }
            }
        }
    }

    async fn send_ping(&mut self) -> Result<()> {
        let frame = Frame::ping(self.service_id);
        let mut buf = Vec::new();
        frame.encode(&mut buf)?;
        self.ws.send(WsMessage::Binary(buf.into())).await?;
        self.last_ping = Instant::now();
        tracing::trace!("ping sent");
        Ok(())
    }

    async fn send_ack(&mut self, original: &Frame, biz_rt_ms: u64) -> Result<()> {
        let ack = Frame::ack(original, biz_rt_ms);
        let mut buf = Vec::new();
        ack.encode(&mut buf)?;
        self.ws.send(WsMessage::Binary(buf.into())).await?;
        tracing::trace!("ack sent");
        Ok(())
    }

    async fn handle_binary(&mut self, data: &[u8]) -> Result<Option<FeishuEvent>> {
        let frame = Frame::decode(data)?;

        match frame.method {
            pbbp2::METHOD_CONTROL => {
                self.handle_control(&frame);
                Ok(None)
            }
            pbbp2::METHOD_DATA => self.handle_data(frame).await,
            other => {
                tracing::debug!(method = other, "unknown frame method, ignoring");
                Ok(None)
            }
        }
    }

    fn handle_control(&mut self, frame: &Frame) {
        match frame.frame_type() {
            Some(pbbp2::TYPE_PONG) => {
                if let Some(payload) = &frame.payload
                    && let Ok(config) = serde_json::from_slice::<ClientConfig>(payload)
                {
                    tracing::debug!(?config, "client config updated from pong");
                    self.config = config;
                }
            }
            Some(other) => tracing::trace!(frame_type = other, "control frame"),
            None => {}
        }
    }

    async fn handle_data(&mut self, frame: Frame) -> Result<Option<FeishuEvent>> {
        let start = Instant::now();

        // Fragment reassembly.
        let payload = self.reassembler.process(&frame)?;
        let payload = match payload {
            Some(p) => p,
            None => return Ok(None), // waiting for more fragments
        };

        // ACK before processing (must be within 3 seconds).
        let biz_rt = start.elapsed().as_millis() as u64;
        if let Err(e) = self.send_ack(&frame, biz_rt).await {
            tracing::warn!("ack failed: {e}");
        }

        // Parse event.
        let frame_type = frame.frame_type().unwrap_or("event");
        let envelope: EventEnvelope = serde_json::from_slice(&payload)?;

        let event = match frame_type {
            pbbp2::TYPE_CARD => FeishuEvent::Card(envelope),
            _ => FeishuEvent::Message(envelope),
        };

        Ok(Some(event))
    }
}

/// Get WebSocket connection parameters from the endpoint API.
async fn get_conn_params(auth: &Auth) -> Result<ConnParams> {
    let url = format!("{}/callback/ws/endpoint", auth.base_url());
    let body = serde_json::json!({
        "AppID": auth.app_id(),
        "AppSecret": auth.app_secret(),
    });

    let resp: EndpointResponse = auth
        .http()
        .post(&url)
        .json(&body)
        .send()
        .await?
        .json()
        .await?;

    match resp.code {
        0 => {}
        403 | 1000040350 => {
            return Err(Error::Fatal(format!(
                "endpoint rejected (code {}): {}",
                resp.code, resp.msg
            )));
        }
        _ => {
            return Err(Error::Connection(format!(
                "endpoint error (code {}): {}",
                resp.code, resp.msg
            )));
        }
    }

    let data = resp
        .data
        .ok_or_else(|| Error::Connection("endpoint response missing data".into()))?;

    // Parse service_id from URL query parameters.
    let service_id = extract_query_param(&data.url, "service_id")
        .and_then(|v| v.parse().ok())
        .unwrap_or(0);

    Ok(ConnParams {
        url: data.url,
        service_id,
        config: data.client_config,
    })
}

fn extract_query_param<'a>(url: &'a str, key: &str) -> Option<&'a str> {
    let query = url.split('?').nth(1)?;
    for pair in query.split('&') {
        let mut kv = pair.splitn(2, '=');
        if kv.next() == Some(key) {
            return kv.next();
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extract_query_param_works() {
        let url = "wss://open.feishu.cn/ws/abc?device_id=d1&service_id=42&foo=bar";
        assert_eq!(extract_query_param(url, "service_id"), Some("42"));
        assert_eq!(extract_query_param(url, "device_id"), Some("d1"));
        assert_eq!(extract_query_param(url, "foo"), Some("bar"));
        assert_eq!(extract_query_param(url, "missing"), None);
    }

    #[test]
    fn extract_query_param_no_query() {
        assert_eq!(extract_query_param("wss://foo.com/ws", "key"), None);
    }

    #[test]
    fn fragment_single_frame() {
        let mut r = Reassembler::new();
        let frame = Frame {
            seq_id: 1,
            log_id: 0,
            service: 1,
            method: pbbp2::METHOD_DATA,
            headers: vec![pbbp2::Header {
                key: pbbp2::HEADER_SUM.into(),
                value: "1".into(),
            }],
            payload_encoding: None,
            payload_type: None,
            payload: Some(b"hello".to_vec()),
            log_id_new: None,
        };
        assert_eq!(r.process(&frame).unwrap(), Some(b"hello".to_vec()));
    }

    #[test]
    fn fragment_multi_in_order() {
        let mut r = Reassembler::new();
        assert_eq!(r.process(&frag("m1", 3, 0, b"aaa")).unwrap(), None);
        assert_eq!(r.process(&frag("m1", 3, 1, b"bbb")).unwrap(), None);
        assert_eq!(
            r.process(&frag("m1", 3, 2, b"ccc")).unwrap(),
            Some(b"aaabbbccc".to_vec())
        );
        assert!(r.fragments.is_empty());
    }

    #[test]
    fn fragment_out_of_order() {
        let mut r = Reassembler::new();
        assert_eq!(r.process(&frag("m2", 3, 2, b"ccc")).unwrap(), None);
        assert_eq!(r.process(&frag("m2", 3, 0, b"aaa")).unwrap(), None);
        assert_eq!(
            r.process(&frag("m2", 3, 1, b"bbb")).unwrap(),
            Some(b"aaabbbccc".to_vec())
        );
    }

    #[test]
    fn fragment_interleaved_messages() {
        let mut r = Reassembler::new();
        assert_eq!(r.process(&frag("a", 2, 0, b"A0")).unwrap(), None);
        assert_eq!(r.process(&frag("b", 2, 0, b"B0")).unwrap(), None);
        assert_eq!(
            r.process(&frag("a", 2, 1, b"A1")).unwrap(),
            Some(b"A0A1".to_vec())
        );
        assert_eq!(
            r.process(&frag("b", 2, 1, b"B1")).unwrap(),
            Some(b"B0B1".to_vec())
        );
    }

    fn frag(msg_id: &str, sum: usize, seq: usize, payload: &[u8]) -> Frame {
        Frame {
            seq_id: seq as u64,
            log_id: 0,
            service: 1,
            method: pbbp2::METHOD_DATA,
            headers: vec![
                pbbp2::Header {
                    key: pbbp2::HEADER_MESSAGE_ID.into(),
                    value: msg_id.into(),
                },
                pbbp2::Header {
                    key: pbbp2::HEADER_SUM.into(),
                    value: sum.to_string(),
                },
                pbbp2::Header {
                    key: pbbp2::HEADER_SEQ.into(),
                    value: seq.to_string(),
                },
                pbbp2::Header {
                    key: pbbp2::HEADER_TYPE.into(),
                    value: pbbp2::TYPE_EVENT.into(),
                },
            ],
            payload_encoding: None,
            payload_type: None,
            payload: Some(payload.to_vec()),
            log_id_new: None,
        }
    }
}
