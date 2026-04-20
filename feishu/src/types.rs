//! Shared types for Feishu events, messages, and API responses.

use serde::{Deserialize, Serialize};

/// Error type for the feishu crate.
///
/// Classifies errors by recoverability:
/// - [`Error::Fatal`]: permanent failure, do not retry (auth banned, conn limit)
/// - [`Error::Auth`], [`Error::Api`]: server-reported errors with code
/// - Others: transient or local, may be retried
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("HTTP request failed: {0}")]
    Http(#[from] reqwest::Error),
    #[error("WebSocket error: {0}")]
    WebSocket(Box<tokio_tungstenite::tungstenite::Error>),
    #[error("Protobuf decode error: {0}")]
    ProtobufDecode(#[from] prost::DecodeError),
    #[error("Protobuf encode error: {0}")]
    ProtobufEncode(#[from] prost::EncodeError),
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("Auth failed (code {code}): {msg}")]
    Auth { code: i64, msg: String },
    #[error("API error (code {code}): {msg}")]
    Api { code: i64, msg: String },
    #[error("Connection error: {0}")]
    Connection(String),
    #[error("Fatal: {0}")]
    Fatal(String),
}

impl From<tokio_tungstenite::tungstenite::Error> for Error {
    fn from(e: tokio_tungstenite::tungstenite::Error) -> Self {
        Self::WebSocket(Box::new(e))
    }
}

impl Error {
    /// Whether this error is fatal (should not retry).
    pub fn is_fatal(&self) -> bool {
        matches!(self, Self::Fatal(_))
    }
}

pub type Result<T> = std::result::Result<T, Error>;

/// Response envelope for the WS endpoint discovery.
#[derive(Debug, Deserialize)]
pub struct EndpointResponse {
    pub code: i64,
    pub msg: String,
    pub data: Option<EndpointData>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct EndpointData {
    #[serde(rename = "URL")]
    pub url: String,
    pub client_config: ClientConfig,
}

/// Server-negotiated client configuration. Sent in the initial endpoint
/// response and may be updated in pong frames.
#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct ClientConfig {
    /// Max reconnect attempts (-1 = infinite).
    #[serde(default = "default_reconnect_count")]
    pub reconnect_count: i32,
    /// Seconds between reconnect attempts.
    #[serde(default = "default_reconnect_interval")]
    pub reconnect_interval: u64,
    /// Max random jitter (seconds) before first reconnect.
    #[serde(default = "default_reconnect_nonce")]
    pub reconnect_nonce: u64,
    /// Seconds between ping frames.
    #[serde(default = "default_ping_interval")]
    pub ping_interval: u64,
}

fn default_reconnect_count() -> i32 {
    -1
}
fn default_reconnect_interval() -> u64 {
    120
}
fn default_reconnect_nonce() -> u64 {
    30
}
fn default_ping_interval() -> u64 {
    120
}

impl Default for ClientConfig {
    fn default() -> Self {
        Self {
            reconnect_count: default_reconnect_count(),
            reconnect_interval: default_reconnect_interval(),
            reconnect_nonce: default_reconnect_nonce(),
            ping_interval: default_ping_interval(),
        }
    }
}

/// Tenant access token response.
#[derive(Debug, Deserialize)]
pub struct TokenResponse {
    pub code: i64,
    pub msg: String,
    pub tenant_access_token: Option<String>,
    pub expire: Option<u64>,
}

/// Standard Feishu API response envelope.
#[derive(Debug, Deserialize)]
pub struct ApiResponse<T = serde_json::Value> {
    pub code: i64,
    pub msg: String,
    pub data: Option<T>,
}

/// A received Feishu event (after WS frame decoding + fragment reassembly).
#[derive(Debug, Clone)]
pub enum FeishuEvent {
    /// IM message event (im.message.receive_v1).
    Message(EventEnvelope),
    /// Card action callback.
    Card(EventEnvelope),
}

/// The raw JSON event envelope from Feishu (v2 format).
/// Fields are parsed lazily — the consumer extracts what they need.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct EventEnvelope {
    pub schema: Option<String>,
    pub header: Option<EventHeader>,
    pub event: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct EventHeader {
    pub event_id: Option<String>,
    pub event_type: Option<String>,
    pub create_time: Option<String>,
    pub token: Option<String>,
    pub app_id: Option<String>,
    pub tenant_key: Option<String>,
}

/// Data returned from send/reply message APIs.
#[derive(Debug, Deserialize)]
pub struct SendMessageData {
    pub message_id: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deserialize_endpoint_response() {
        let json = r#"{
            "code": 0,
            "msg": "success",
            "data": {
                "URL": "wss://open.feishu.cn/ws/abc?device_id=d1&service_id=7",
                "ClientConfig": {
                    "ReconnectCount": 120,
                    "ReconnectInterval": 60,
                    "ReconnectNonce": 15,
                    "PingInterval": 90
                }
            }
        }"#;
        let resp: EndpointResponse = serde_json::from_str(json).unwrap();
        assert_eq!(resp.code, 0);
        let data = resp.data.unwrap();
        assert!(data.url.starts_with("wss://"));
        assert_eq!(data.client_config.ping_interval, 90);
        assert_eq!(data.client_config.reconnect_nonce, 15);
    }

    #[test]
    fn deserialize_client_config_defaults() {
        let json = r#"{}"#;
        let cfg: ClientConfig = serde_json::from_str(json).unwrap();
        assert_eq!(cfg.reconnect_count, -1);
        assert_eq!(cfg.reconnect_interval, 120);
        assert_eq!(cfg.ping_interval, 120);
    }

    #[test]
    fn deserialize_token_response() {
        let json = r#"{
            "code": 0,
            "msg": "ok",
            "tenant_access_token": "t-abc123",
            "expire": 7200
        }"#;
        let resp: TokenResponse = serde_json::from_str(json).unwrap();
        assert_eq!(resp.tenant_access_token.as_deref(), Some("t-abc123"));
        assert_eq!(resp.expire, Some(7200));
    }

    #[test]
    fn deserialize_event_envelope() {
        let json = r#"{
            "schema": "2.0",
            "header": {
                "event_id": "evt-123",
                "event_type": "im.message.receive_v1",
                "create_time": "1234567890",
                "token": "tok",
                "app_id": "cli_abc",
                "tenant_key": "tenant_abc"
            },
            "event": {"message": {"content": "{\"text\":\"hello\"}"}}
        }"#;
        let env: EventEnvelope = serde_json::from_str(json).unwrap();
        assert_eq!(env.schema.as_deref(), Some("2.0"));
        let header = env.header.unwrap();
        assert_eq!(header.event_type.as_deref(), Some("im.message.receive_v1"));
        assert!(env.event.is_some());
    }

    #[test]
    fn api_response_with_send_message_data() {
        let json = r#"{"code": 0, "msg": "success", "data": {"message_id": "om_abc123"}}"#;
        let resp: ApiResponse<SendMessageData> = serde_json::from_str(json).unwrap();
        assert_eq!(resp.code, 0);
        assert_eq!(resp.data.unwrap().message_id.as_deref(), Some("om_abc123"));
    }
}
