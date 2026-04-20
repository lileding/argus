//! Feishu WebSocket wire protocol (pbbp2 protobuf format).
//!
//! All WebSocket messages are binary frames containing a protobuf-encoded [`Frame`].
//! The protocol uses proto2 syntax with 9 fields, though only fields 1-5 and 8 are
//! used in practice.

/// Protobuf header (key-value pair) inside a [`Frame`].
#[derive(Clone, PartialEq, prost::Message)]
pub struct Header {
    #[prost(string, required, tag = "1")]
    pub key: String,
    #[prost(string, required, tag = "2")]
    pub value: String,
}

/// Protobuf frame — the unit of communication over the Feishu WebSocket.
///
/// `method` discriminates control frames (ping/pong) from data frames (events).
/// Headers carry metadata like message type, fragment info, and trace IDs.
#[derive(Clone, PartialEq, prost::Message)]
pub struct Frame {
    #[prost(uint64, required, tag = "1")]
    pub seq_id: u64,
    #[prost(uint64, required, tag = "2")]
    pub log_id: u64,
    #[prost(int32, required, tag = "3")]
    pub service: i32,
    #[prost(int32, required, tag = "4")]
    pub method: i32,
    #[prost(message, repeated, tag = "5")]
    pub headers: Vec<Header>,
    #[prost(string, optional, tag = "6")]
    pub payload_encoding: Option<String>,
    #[prost(string, optional, tag = "7")]
    pub payload_type: Option<String>,
    #[prost(bytes = "vec", optional, tag = "8")]
    pub payload: Option<Vec<u8>>,
    #[prost(string, optional, tag = "9")]
    pub log_id_new: Option<String>,
}

/// Frame method discriminator.
pub const METHOD_CONTROL: i32 = 0;
pub const METHOD_DATA: i32 = 1;

/// Well-known header keys.
pub const HEADER_TYPE: &str = "type";
pub const HEADER_MESSAGE_ID: &str = "message_id";
pub const HEADER_SUM: &str = "sum";
pub const HEADER_SEQ: &str = "seq";
pub const HEADER_TRACE_ID: &str = "trace_id";
pub const HEADER_BIZ_RT: &str = "biz_rt";

/// Well-known header type values.
pub const TYPE_PING: &str = "ping";
pub const TYPE_PONG: &str = "pong";
pub const TYPE_EVENT: &str = "event";
pub const TYPE_CARD: &str = "card";

impl Frame {
    /// Get a header value by key.
    pub fn header(&self, key: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|h| h.key == key)
            .map(|h| h.value.as_str())
    }

    /// Get the frame type from headers (ping/pong/event/card).
    pub fn frame_type(&self) -> Option<&str> {
        self.header(HEADER_TYPE)
    }

    /// Build a ping frame for the given service ID.
    pub fn ping(service_id: i32) -> Self {
        Self {
            seq_id: 0,
            log_id: 0,
            service: service_id,
            method: METHOD_CONTROL,
            headers: vec![Header {
                key: HEADER_TYPE.into(),
                value: TYPE_PING.into(),
            }],
            payload_encoding: None,
            payload_type: None,
            payload: None,
            log_id_new: None,
        }
    }

    /// Build an ACK response frame from a received data frame.
    pub fn ack(original: &Frame, biz_rt_ms: u64) -> Self {
        let mut headers = original.headers.clone();
        headers.push(Header {
            key: HEADER_BIZ_RT.into(),
            value: biz_rt_ms.to_string(),
        });
        Self {
            seq_id: original.seq_id,
            log_id: original.log_id,
            service: original.service,
            method: METHOD_DATA,
            headers,
            payload_encoding: None,
            payload_type: None,
            payload: Some(br#"{"code":200,"headers":{},"data":[]}"#.to_vec()),
            log_id_new: original.log_id_new.clone(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use prost::Message;

    #[test]
    fn roundtrip_ping_frame() {
        let ping = Frame::ping(42);
        let mut buf = Vec::new();
        ping.encode(&mut buf).unwrap();
        let decoded = Frame::decode(&buf[..]).unwrap();
        assert_eq!(decoded.method, METHOD_CONTROL);
        assert_eq!(decoded.service, 42);
        assert_eq!(decoded.frame_type(), Some(TYPE_PING));
    }

    #[test]
    fn roundtrip_data_frame_with_payload() {
        let frame = Frame {
            seq_id: 1,
            log_id: 100,
            service: 7,
            method: METHOD_DATA,
            headers: vec![
                Header {
                    key: HEADER_TYPE.into(),
                    value: TYPE_EVENT.into(),
                },
                Header {
                    key: HEADER_MESSAGE_ID.into(),
                    value: "msg-123".into(),
                },
                Header {
                    key: HEADER_SUM.into(),
                    value: "1".into(),
                },
                Header {
                    key: HEADER_SEQ.into(),
                    value: "0".into(),
                },
            ],
            payload_encoding: None,
            payload_type: None,
            payload: Some(br#"{"event":"test"}"#.to_vec()),
            log_id_new: None,
        };
        let mut buf = Vec::new();
        frame.encode(&mut buf).unwrap();
        let decoded = Frame::decode(&buf[..]).unwrap();
        assert_eq!(decoded.seq_id, 1);
        assert_eq!(decoded.frame_type(), Some(TYPE_EVENT));
        assert_eq!(decoded.header(HEADER_MESSAGE_ID), Some("msg-123"));
        assert_eq!(decoded.payload, Some(br#"{"event":"test"}"#.to_vec()));
    }

    #[test]
    fn ack_preserves_original_headers_and_adds_biz_rt() {
        let original = Frame {
            seq_id: 42,
            log_id: 99,
            service: 7,
            method: METHOD_DATA,
            headers: vec![
                Header {
                    key: HEADER_TYPE.into(),
                    value: TYPE_EVENT.into(),
                },
                Header {
                    key: HEADER_TRACE_ID.into(),
                    value: "trace-abc".into(),
                },
            ],
            payload_encoding: None,
            payload_type: None,
            payload: Some(b"original".to_vec()),
            log_id_new: Some("log-new".into()),
        };

        let ack = Frame::ack(&original, 150);
        assert_eq!(ack.seq_id, 42);
        assert_eq!(ack.log_id, 99);
        assert_eq!(ack.service, 7);
        assert_eq!(ack.method, METHOD_DATA);
        assert_eq!(ack.log_id_new.as_deref(), Some("log-new"));
        // Original headers preserved.
        assert_eq!(ack.header(HEADER_TYPE), Some(TYPE_EVENT));
        assert_eq!(ack.header(HEADER_TRACE_ID), Some("trace-abc"));
        // biz_rt added.
        assert_eq!(ack.header(HEADER_BIZ_RT), Some("150"));
        // Payload is the ACK JSON.
        let payload = std::str::from_utf8(ack.payload.as_ref().unwrap()).unwrap();
        assert!(payload.contains("\"code\":200"));
    }

    #[test]
    fn header_lookup_missing_key_returns_none() {
        let frame = Frame::ping(1);
        assert_eq!(frame.header("nonexistent"), None);
    }
}
