//! Shared types for the model client layer.

use std::pin::Pin;

use futures::Stream;
use serde::{Deserialize, Serialize};

// --- Roles ---

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub(crate) enum Role {
    System,
    User,
    Assistant,
    Tool,
}

// --- Messages ---

/// A chat message. Content is either plain text or multimodal (text + images).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct Message {
    pub(crate) role: Role,
    /// Plain text content. For multimodal messages, this is the text part.
    #[serde(default)]
    pub(crate) content: String,
    /// Multimodal content parts (text + images). Empty for text-only messages.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub(crate) parts: Vec<ContentPart>,
    /// Tool calls made by the assistant.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub(crate) tool_calls: Vec<ToolCall>,
    /// For tool result messages: links back to the tool call ID.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) tool_call_id: Option<String>,
    /// For tool result messages: the function name (required by Gemini).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub(crate) tool_name: Option<String>,
}

impl Message {
    pub(crate) fn system(content: impl Into<String>) -> Self {
        Self {
            role: Role::System,
            content: content.into(),
            parts: vec![],
            tool_calls: vec![],
            tool_call_id: None,
            tool_name: None,
        }
    }

    pub(crate) fn user(content: impl Into<String>) -> Self {
        Self {
            role: Role::User,
            content: content.into(),
            parts: vec![],
            tool_calls: vec![],
            tool_call_id: None,
            tool_name: None,
        }
    }

    pub(crate) fn assistant(content: impl Into<String>) -> Self {
        Self {
            role: Role::Assistant,
            content: content.into(),
            parts: vec![],
            tool_calls: vec![],
            tool_call_id: None,
            tool_name: None,
        }
    }

    pub(crate) fn tool_result(
        call_id: impl Into<String>,
        name: impl Into<String>,
        content: impl Into<String>,
    ) -> Self {
        Self {
            role: Role::Tool,
            content: content.into(),
            parts: vec![],
            tool_calls: vec![],
            tool_call_id: Some(call_id.into()),
            tool_name: Some(name.into()),
        }
    }
}

/// A part of a multimodal message.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type")]
pub(crate) enum ContentPart {
    #[serde(rename = "text")]
    Text { text: String },
    #[serde(rename = "image_url")]
    ImageUrl { url: String },
}

// --- Tool Definitions ---

/// Tool definition passed to the model.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct ToolDef {
    pub(crate) name: String,
    pub(crate) description: String,
    /// JSON Schema for the function parameters (passed as-is to providers).
    pub(crate) parameters: serde_json::Value,
}

// --- Tool Calls ---

/// A tool call requested by the model.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub(crate) struct ToolCall {
    pub(crate) id: String,
    pub(crate) name: String,
    /// Raw JSON string of arguments (as returned by the model).
    pub(crate) arguments: String,
}

// --- Responses ---

/// Response from a non-streaming chat call.
#[derive(Debug, Clone)]
pub(crate) struct Response {
    pub(crate) content: String,
    pub(crate) tool_calls: Vec<ToolCall>,
    pub(crate) finish_reason: FinishReason,
    pub(crate) usage: Usage,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum FinishReason {
    Stop,
    ToolCalls,
    Length,
    EarlyAbort,
    Other(String),
}

#[derive(Debug, Clone, Default)]
pub(crate) struct Usage {
    pub(crate) prompt_tokens: u32,
    pub(crate) completion_tokens: u32,
    pub(crate) total_tokens: u32,
}

// --- Streaming ---

/// Incremental tool call delta from a streaming response.
#[derive(Debug, Clone)]
pub(crate) struct ToolCallDelta {
    /// Index of this tool call in the array (for parallel tool calls).
    pub(crate) index: usize,
    /// Tool call ID (only present on the first delta for this index).
    pub(crate) id: Option<String>,
    /// Function name (only present on the first delta for this index).
    pub(crate) name: Option<String>,
    /// Incremental JSON arguments fragment.
    pub(crate) arguments_delta: String,
}

/// A chunk from a streaming chat response.
#[derive(Debug, Clone)]
pub(crate) struct StreamChunk {
    /// Incremental text delta (appended since previous chunk).
    pub(crate) delta: String,
    /// Tool call deltas in this chunk (if any).
    pub(crate) tool_call_deltas: Vec<ToolCallDelta>,
    /// True on the final chunk.
    pub(crate) done: bool,
    /// Token usage (populated on final chunk if provider reports it).
    pub(crate) usage: Option<Usage>,
    /// Error (populated on final chunk if streaming errored).
    pub(crate) error: Option<String>,
}

/// Type alias for a boxed async stream of chunks.
pub(crate) type ChunkStream = Pin<Box<dyn Stream<Item = StreamChunk> + Send>>;

// --- Client trait ---

/// Model client interface. Each provider implements this.
#[async_trait::async_trait]
pub(crate) trait Client: Send + Sync {
    async fn chat(&self, messages: &[Message], tools: &[ToolDef]) -> ClientResult<Response>;
    async fn chat_stream(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
    ) -> ClientResult<ChunkStream>;
    async fn chat_with_early_abort(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
        max_text_tokens: usize,
    ) -> ClientResult<Response>;
}

// --- Errors ---

#[derive(Debug, thiserror::Error)]
pub(crate) enum ClientError {
    #[error("HTTP error: {0}")]
    Http(#[from] reqwest::Error),
    #[error("API error (status {status}): {message}")]
    Api { status: u16, message: String },
    #[error("Rate limited (429)")]
    RateLimited,
    #[error("JSON error: {0}")]
    Json(#[from] serde_json::Error),
    #[error("SSE error: {0}")]
    Sse(String),
    #[error("{0}")]
    Other(String),
}

impl ClientError {
    pub(crate) fn is_rate_limited(&self) -> bool {
        matches!(self, Self::RateLimited)
    }
}

pub(crate) type ClientResult<T> = std::result::Result<T, ClientError>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn message_constructors() {
        let sys = Message::system("you are helpful");
        assert_eq!(sys.role, Role::System);
        assert_eq!(sys.content, "you are helpful");

        let usr = Message::user("hello");
        assert_eq!(usr.role, Role::User);

        let asst = Message::assistant("hi there");
        assert_eq!(asst.role, Role::Assistant);
        assert!(asst.tool_calls.is_empty());

        let tool = Message::tool_result("call_1", "search", "results...");
        assert_eq!(tool.role, Role::Tool);
        assert_eq!(tool.tool_call_id.as_deref(), Some("call_1"));
        assert_eq!(tool.tool_name.as_deref(), Some("search"));
    }

    #[test]
    fn role_serializes_lowercase() {
        assert_eq!(serde_json::to_string(&Role::System).unwrap(), "\"system\"");
        assert_eq!(serde_json::to_string(&Role::User).unwrap(), "\"user\"");
        assert_eq!(
            serde_json::to_string(&Role::Assistant).unwrap(),
            "\"assistant\""
        );
        assert_eq!(serde_json::to_string(&Role::Tool).unwrap(), "\"tool\"");
    }

    #[test]
    fn role_deserializes() {
        let r: Role = serde_json::from_str(r#""assistant""#).unwrap();
        assert_eq!(r, Role::Assistant);
    }

    #[test]
    fn tool_def_roundtrip() {
        let td = ToolDef {
            name: "search".into(),
            description: "Web search".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string"}
                },
                "required": ["query"]
            }),
        };
        let json = serde_json::to_string(&td).unwrap();
        let parsed: ToolDef = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed.name, "search");
        assert_eq!(parsed.parameters["properties"]["query"]["type"], "string");
    }

    #[test]
    fn finish_reason_equality() {
        assert_eq!(FinishReason::Stop, FinishReason::Stop);
        assert_ne!(FinishReason::Stop, FinishReason::ToolCalls);
        assert_eq!(
            FinishReason::Other("foo".into()),
            FinishReason::Other("foo".into())
        );
    }

    #[test]
    fn client_error_rate_limited() {
        assert!(ClientError::RateLimited.is_rate_limited());
        assert!(!ClientError::Other("nope".into()).is_rate_limited());
    }

    #[test]
    fn content_part_serializes() {
        let text = ContentPart::Text {
            text: "hello".into(),
        };
        let json = serde_json::to_string(&text).unwrap();
        assert!(json.contains(r#""type":"text""#));

        let img = ContentPart::ImageUrl {
            url: "data:image/png;base64,abc".into(),
        };
        let json = serde_json::to_string(&img).unwrap();
        assert!(json.contains(r#""type":"image_url""#));
    }
}
