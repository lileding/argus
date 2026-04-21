//! Anthropic Messages API client (Claude models).
//!
//! Key differences from OpenAI:
//! - Auth: `x-api-key` header (not Bearer)
//! - System message: top-level `system` field, not in messages array
//! - Tool args: parsed JSON object (not string)
//! - Tool result: user message with `tool_result` content block
//! - SSE: typed events (message_start, content_block_start/delta/stop, message_delta)

use std::time::Duration;

use futures::{Stream, StreamExt};
use reqwest_eventsource::{Event as SseEvent, EventSource};
use serde::{Deserialize, Serialize};
use tracing::debug;

use super::Client;
use super::types::*;
use crate::config::{RoleConfig, UpstreamConfig};

const DEFAULT_BASE_URL: &str = "https://api.anthropic.com";
const API_VERSION: &str = "2023-06-01";

pub(super) struct AnthropicClient {
    http: reqwest::Client,
    base_url: String,
    api_key: String,
    model: String,
    max_tokens: usize,
}

impl AnthropicClient {
    pub fn new(upstream: &UpstreamConfig, role: &RoleConfig) -> Self {
        let base_url = if upstream.base_url.is_empty() {
            DEFAULT_BASE_URL.to_string()
        } else {
            upstream.base_url.clone()
        };
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(upstream.timeout_secs))
            .build()
            .expect("failed to build HTTP client");

        Self {
            http,
            base_url,
            api_key: upstream.api_key.clone(),
            model: role.model_name.clone(),
            max_tokens: role.max_tokens,
        }
    }

    fn messages_url(&self) -> String {
        format!("{}/v1/messages", self.base_url)
    }

    fn build_request(&self, messages: &[Message], tools: &[ToolDef], stream: bool) -> ApiRequest {
        let mut system: Option<serde_json::Value> = None;
        let mut api_messages: Vec<ApiMessage> = Vec::new();

        for msg in messages {
            match msg.role {
                Role::System => {
                    // Top-level system with cache_control for prompt caching.
                    system = Some(serde_json::json!([{
                        "type": "text",
                        "text": msg.content,
                        "cache_control": {"type": "ephemeral"}
                    }]));
                }
                Role::User => {
                    if msg.parts.is_empty() {
                        api_messages.push(ApiMessage {
                            role: "user".into(),
                            content: serde_json::json!(msg.content),
                        });
                    } else {
                        // Multimodal: convert parts to Anthropic content blocks.
                        let blocks: Vec<serde_json::Value> = msg
                            .parts
                            .iter()
                            .map(|p| match p {
                                ContentPart::Text { text } => {
                                    serde_json::json!({"type": "text", "text": text})
                                }
                                ContentPart::ImageUrl { url } => {
                                    // Anthropic wants base64 source, not data: URL.
                                    // Parse "data:image/png;base64,<data>" format.
                                    if let Some((media_type, data)) = parse_data_url(url) {
                                        serde_json::json!({
                                            "type": "image",
                                            "source": {
                                                "type": "base64",
                                                "media_type": media_type,
                                                "data": data
                                            }
                                        })
                                    } else {
                                        serde_json::json!({
                                            "type": "image",
                                            "source": {"type": "url", "url": url}
                                        })
                                    }
                                }
                            })
                            .collect();
                        api_messages.push(ApiMessage {
                            role: "user".into(),
                            content: serde_json::Value::Array(blocks),
                        });
                    }
                }
                Role::Assistant => {
                    if msg.tool_calls.is_empty() {
                        api_messages.push(ApiMessage {
                            role: "assistant".into(),
                            content: serde_json::json!(msg.content),
                        });
                    } else {
                        // Assistant with tool calls → content blocks.
                        let mut blocks: Vec<serde_json::Value> = Vec::new();
                        if !msg.content.is_empty() {
                            blocks.push(serde_json::json!({"type": "text", "text": msg.content}));
                        }
                        for tc in &msg.tool_calls {
                            let input: serde_json::Value =
                                serde_json::from_str(&tc.arguments).unwrap_or_default();
                            blocks.push(serde_json::json!({
                                "type": "tool_use",
                                "id": tc.id,
                                "name": tc.name,
                                "input": input
                            }));
                        }
                        api_messages.push(ApiMessage {
                            role: "assistant".into(),
                            content: serde_json::Value::Array(blocks),
                        });
                    }
                }
                Role::Tool => {
                    // Tool result → user message with tool_result block.
                    api_messages.push(ApiMessage {
                        role: "user".into(),
                        content: serde_json::json!([{
                            "type": "tool_result",
                            "tool_use_id": msg.tool_call_id,
                            "content": msg.content
                        }]),
                    });
                }
            }
        }

        let api_tools: Vec<ApiTool> = tools
            .iter()
            .map(|t| ApiTool {
                name: t.name.clone(),
                description: t.description.clone(),
                input_schema: t.parameters.clone(),
            })
            .collect();

        ApiRequest {
            model: self.model.clone(),
            max_tokens: self.max_tokens,
            system,
            messages: api_messages,
            tools: if api_tools.is_empty() {
                None
            } else {
                Some(api_tools)
            },
            stream,
        }
    }

    fn auth_request(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        req.header("x-api-key", &self.api_key)
            .header("anthropic-version", API_VERSION)
    }

    fn parse_response(body: &str) -> ClientResult<Response> {
        let resp: ApiResponse = serde_json::from_str(body)?;
        if let Some(err) = resp.error {
            return Err(ClientError::Api {
                status: 0,
                message: format!("{}: {}", err.error_type, err.message),
            });
        }

        let mut content = String::new();
        let mut tool_calls = Vec::new();

        for block in &resp.content {
            match block.block_type.as_str() {
                "text" => {
                    if let Some(text) = &block.text {
                        content.push_str(text);
                    }
                }
                "tool_use" => {
                    if let (Some(id), Some(name), Some(input)) =
                        (&block.id, &block.name, &block.input)
                    {
                        tool_calls.push(ToolCall {
                            id: id.clone(),
                            name: name.clone(),
                            arguments: serde_json::to_string(input).unwrap_or_default(),
                        });
                    }
                }
                _ => {}
            }
        }

        let finish_reason = match resp.stop_reason.as_deref() {
            Some("end_turn") | Some("stop") => FinishReason::Stop,
            Some("tool_use") => FinishReason::ToolCalls,
            Some("max_tokens") => FinishReason::Length,
            Some(other) => FinishReason::Other(other.to_string()),
            None => FinishReason::Stop,
        };

        Ok(Response {
            content,
            tool_calls,
            finish_reason,
            usage: Usage {
                prompt_tokens: resp.usage.input_tokens,
                completion_tokens: resp.usage.output_tokens,
                total_tokens: resp.usage.input_tokens + resp.usage.output_tokens,
            },
        })
    }
}

#[async_trait::async_trait]
impl Client for AnthropicClient {
    async fn chat(&self, messages: &[Message], tools: &[ToolDef]) -> ClientResult<Response> {
        let body = self.build_request(messages, tools, false);
        let resp = self
            .auth_request(self.http.post(self.messages_url()))
            .json(&body)
            .send()
            .await?;

        let status = resp.status();
        if status.as_u16() == 429 {
            return Err(ClientError::RateLimited);
        }

        let text = resp.text().await?;
        if !status.is_success() {
            return Err(ClientError::Api {
                status: status.as_u16(),
                message: text,
            });
        }

        Self::parse_response(&text)
    }

    async fn chat_stream(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
    ) -> ClientResult<ChunkStream> {
        let body = self.build_request(messages, tools, true);
        let req = self
            .auth_request(self.http.post(self.messages_url()))
            .json(&body);

        let es = EventSource::new(req).map_err(|e| ClientError::Sse(e.to_string()))?;
        let stream = sse_to_stream(es);
        Ok(Box::pin(stream))
    }

    async fn chat_with_early_abort(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
        max_text_tokens: usize,
    ) -> ClientResult<Response> {
        let mut stream = self.chat_stream(messages, tools).await?;

        let mut content = String::new();
        let mut usage = Usage::default();
        let mut pending_tools: Vec<PendingToolCall> = Vec::new();
        let mut current_tool_active = false;

        while let Some(chunk) = stream.next().await {
            if let Some(err) = &chunk.error {
                return Err(ClientError::Sse(err.clone()));
            }

            content.push_str(&chunk.delta);

            // Accumulate tool call deltas.
            for tcd in &chunk.tool_call_deltas {
                while pending_tools.len() <= tcd.index {
                    pending_tools.push(PendingToolCall {
                        id: String::new(),
                        name: String::new(),
                        arguments: String::new(),
                    });
                }
                let pt = &mut pending_tools[tcd.index];
                if let Some(id) = &tcd.id {
                    pt.id.clone_from(id);
                    current_tool_active = true;
                }
                if let Some(name) = &tcd.name {
                    pt.name.clone_from(name);
                }
                pt.arguments.push_str(&tcd.arguments_delta);
            }

            if let Some(u) = chunk.usage {
                usage = u;
            }

            if chunk.done {
                break;
            }

            // Early abort: text too long, no tool calls active or pending.
            if pending_tools.is_empty()
                && !current_tool_active
                && content.len() / 4 > max_text_tokens
            {
                debug!(
                    text_len = content.len(),
                    max_text_tokens, "early abort: text exceeds threshold"
                );
                return Ok(Response {
                    content,
                    tool_calls: vec![],
                    finish_reason: FinishReason::EarlyAbort,
                    usage,
                });
            }
        }

        let tool_calls: Vec<ToolCall> = pending_tools
            .into_iter()
            .filter(|pt| !pt.name.is_empty())
            .map(|pt| ToolCall {
                id: pt.id,
                name: pt.name,
                arguments: pt.arguments,
            })
            .collect();

        let finish_reason = if !tool_calls.is_empty() {
            FinishReason::ToolCalls
        } else {
            FinishReason::Stop
        };

        Ok(Response {
            content,
            tool_calls,
            finish_reason,
            usage,
        })
    }
}

// --- SSE parsing ---

/// Anthropic SSE events have typed event names:
/// message_start, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
fn sse_to_stream(mut es: EventSource) -> impl Stream<Item = StreamChunk> {
    let mut tool_index: usize = 0;
    let mut current_block_is_tool = false;

    async_stream::stream! {
        loop {
            match es.next().await {
                Some(Ok(SseEvent::Message(msg))) => {
                    let data = msg.data.trim();
                    let Ok(event) = serde_json::from_str::<SseEventData>(data) else { continue };

                    match event.event_type.as_str() {
                        "message_start" => {
                            // Extract prompt token count.
                            if let Some(message) = &event.message {
                                yield StreamChunk {
                                    delta: String::new(),
                                    tool_call_deltas: vec![],
                                    done: false,
                                    usage: Some(Usage {
                                        prompt_tokens: message.usage.input_tokens,
                                        completion_tokens: 0,
                                        total_tokens: message.usage.input_tokens,
                                    }),
                                    error: None,
                                };
                            }
                        }
                        "content_block_start" => {
                            current_block_is_tool = event
                                .content_block
                                .as_ref()
                                .is_some_and(|cb| cb.block_type == "tool_use");
                            if let Some(cb) = &event.content_block
                                && cb.block_type == "tool_use"
                            {
                                yield StreamChunk {
                                    delta: String::new(),
                                    tool_call_deltas: vec![ToolCallDelta {
                                        index: tool_index,
                                        id: Some(cb.id.clone().unwrap_or_default()),
                                        name: Some(cb.name.clone().unwrap_or_default()),
                                        arguments_delta: String::new(),
                                    }],
                                    done: false,
                                    usage: None,
                                    error: None,
                                };
                            }
                        }
                        "content_block_delta" => {
                            if let Some(delta) = &event.delta {
                                match delta.delta_type.as_str() {
                                    "text_delta" => {
                                        yield StreamChunk {
                                            delta: delta.text.clone().unwrap_or_default(),
                                            tool_call_deltas: vec![],
                                            done: false,
                                            usage: None,
                                            error: None,
                                        };
                                    }
                                    "input_json_delta" => {
                                        yield StreamChunk {
                                            delta: String::new(),
                                            tool_call_deltas: vec![ToolCallDelta {
                                                index: tool_index,
                                                id: None,
                                                name: None,
                                                arguments_delta: delta.partial_json.clone().unwrap_or_default(),
                                            }],
                                            done: false,
                                            usage: None,
                                            error: None,
                                        };
                                    }
                                    _ => {}
                                }
                            }
                        }
                        "content_block_stop" if current_block_is_tool => {
                            tool_index += 1;
                            current_block_is_tool = false;
                        }
                        "content_block_stop" => {}
                        "message_delta" => {
                            let usage = event.usage.map(|u| Usage {
                                prompt_tokens: 0,
                                completion_tokens: u.output_tokens,
                                total_tokens: u.output_tokens,
                            });
                            yield StreamChunk {
                                delta: String::new(),
                                tool_call_deltas: vec![],
                                done: false,
                                usage,
                                error: None,
                            };
                        }
                        "message_stop" => {
                            yield StreamChunk {
                                delta: String::new(),
                                tool_call_deltas: vec![],
                                done: true,
                                usage: None,
                                error: None,
                            };
                            break;
                        }
                        _ => {}
                    }
                }
                Some(Ok(SseEvent::Open)) => {}
                Some(Err(e)) => {
                    yield StreamChunk {
                        delta: String::new(),
                        tool_call_deltas: vec![],
                        done: true,
                        usage: None,
                        error: Some(e.to_string()),
                    };
                    break;
                }
                None => {
                    yield StreamChunk {
                        delta: String::new(),
                        tool_call_deltas: vec![],
                        done: true,
                        usage: None,
                        error: None,
                    };
                    break;
                }
            }
        }
    }
}

struct PendingToolCall {
    id: String,
    name: String,
    arguments: String,
}

/// Parse a data: URL like "data:image/png;base64,<data>" into (media_type, data).
fn parse_data_url(url: &str) -> Option<(String, String)> {
    let rest = url.strip_prefix("data:")?;
    let (meta, data) = rest.split_once(',')?;
    let media_type = meta.strip_suffix(";base64")?;
    Some((media_type.to_string(), data.to_string()))
}

// --- Anthropic API types ---

#[derive(Serialize)]
struct ApiRequest {
    model: String,
    max_tokens: usize,
    #[serde(skip_serializing_if = "Option::is_none")]
    system: Option<serde_json::Value>,
    messages: Vec<ApiMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tools: Option<Vec<ApiTool>>,
    stream: bool,
}

#[derive(Serialize)]
struct ApiMessage {
    role: String,
    content: serde_json::Value,
}

#[derive(Serialize)]
struct ApiTool {
    name: String,
    description: String,
    input_schema: serde_json::Value,
}

#[derive(Deserialize)]
struct ApiResponse {
    #[serde(default)]
    content: Vec<ContentBlock>,
    #[serde(default)]
    stop_reason: Option<String>,
    #[serde(default)]
    usage: ApiUsage,
    #[serde(default)]
    error: Option<ApiError>,
}

#[derive(Deserialize)]
struct ContentBlock {
    #[serde(rename = "type")]
    block_type: String,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    input: Option<serde_json::Value>,
}

#[derive(Deserialize, Default)]
struct ApiUsage {
    #[serde(default)]
    input_tokens: u32,
    #[serde(default)]
    output_tokens: u32,
}

#[derive(Deserialize)]
struct ApiError {
    #[serde(rename = "type")]
    error_type: String,
    message: String,
}

// --- SSE event types ---

#[derive(Deserialize)]
struct SseEventData {
    #[serde(rename = "type")]
    event_type: String,
    #[serde(default)]
    message: Option<SseMessage>,
    #[serde(default)]
    content_block: Option<SseContentBlock>,
    #[serde(default)]
    delta: Option<SseDelta>,
    #[serde(default)]
    usage: Option<SseUsage>,
}

#[derive(Deserialize)]
struct SseMessage {
    usage: SseMessageUsage,
}

#[derive(Deserialize)]
struct SseMessageUsage {
    #[serde(default)]
    input_tokens: u32,
    #[serde(default)]
    output_tokens: u32,
}

#[derive(Deserialize)]
struct SseContentBlock {
    #[serde(rename = "type")]
    block_type: String,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    name: Option<String>,
}

#[derive(Deserialize)]
struct SseDelta {
    #[serde(rename = "type")]
    delta_type: String,
    #[serde(default)]
    text: Option<String>,
    #[serde(default)]
    partial_json: Option<String>,
}

#[derive(Deserialize)]
struct SseUsage {
    #[serde(default)]
    output_tokens: u32,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_non_streaming_response() {
        let json = r#"{
            "content": [
                {"type": "text", "text": "Hello!"}
            ],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 10, "output_tokens": 5}
        }"#;
        let resp = AnthropicClient::parse_response(json).unwrap();
        assert_eq!(resp.content, "Hello!");
        assert_eq!(resp.finish_reason, FinishReason::Stop);
        assert_eq!(resp.usage.prompt_tokens, 10);
        assert_eq!(resp.usage.completion_tokens, 5);
    }

    #[test]
    fn parse_tool_use_response() {
        let json = r#"{
            "content": [
                {"type": "text", "text": "Let me search."},
                {"type": "tool_use", "id": "toolu_1", "name": "search", "input": {"query": "rust"}}
            ],
            "stop_reason": "tool_use",
            "usage": {"input_tokens": 20, "output_tokens": 15}
        }"#;
        let resp = AnthropicClient::parse_response(json).unwrap();
        assert_eq!(resp.content, "Let me search.");
        assert_eq!(resp.finish_reason, FinishReason::ToolCalls);
        assert_eq!(resp.tool_calls.len(), 1);
        assert_eq!(resp.tool_calls[0].name, "search");
        assert!(resp.tool_calls[0].arguments.contains("rust"));
    }

    #[test]
    fn parse_error_response() {
        let json = r#"{
            "content": [],
            "stop_reason": null,
            "usage": {"input_tokens": 0, "output_tokens": 0},
            "error": {"type": "invalid_request_error", "message": "bad request"}
        }"#;
        let err = AnthropicClient::parse_response(json).unwrap_err();
        assert!(err.to_string().contains("bad request"));
    }

    #[test]
    fn parse_data_url_valid() {
        let (media, data) = parse_data_url("data:image/png;base64,abc123").unwrap();
        assert_eq!(media, "image/png");
        assert_eq!(data, "abc123");
    }

    #[test]
    fn parse_data_url_invalid() {
        assert!(parse_data_url("https://example.com/image.png").is_none());
        assert!(parse_data_url("data:image/png,nobase64").is_none());
    }

    #[test]
    fn build_request_system_message() {
        let upstream = UpstreamConfig {
            provider_type: "anthropic".into(),
            base_url: String::new(),
            api_key: "test".into(),
            timeout_secs: 60,
        };
        let role = RoleConfig {
            upstream: "anthropic".into(),
            model_name: "claude-haiku-4-5".into(),
            max_tokens: 4096,
        };
        let client = AnthropicClient::new(&upstream, &role);

        let messages = vec![Message::system("be helpful"), Message::user("hi")];
        let req = client.build_request(&messages, &[], false);

        // System should be top-level, not in messages.
        assert!(req.system.is_some());
        assert_eq!(req.messages.len(), 1); // only user, no system
        assert_eq!(req.messages[0].role, "user");
    }

    #[test]
    fn build_request_tool_result() {
        let upstream = UpstreamConfig {
            provider_type: "anthropic".into(),
            base_url: String::new(),
            api_key: "test".into(),
            timeout_secs: 60,
        };
        let role = RoleConfig {
            upstream: "anthropic".into(),
            model_name: "claude-haiku-4-5".into(),
            max_tokens: 4096,
        };
        let client = AnthropicClient::new(&upstream, &role);

        let messages = vec![Message::tool_result("toolu_1", "search", "found it")];
        let req = client.build_request(&messages, &[], false);

        // Tool result → user message with tool_result content block.
        assert_eq!(req.messages.len(), 1);
        assert_eq!(req.messages[0].role, "user");
        let content = &req.messages[0].content;
        assert!(content.is_array());
        assert_eq!(content[0]["type"], "tool_result");
        assert_eq!(content[0]["tool_use_id"], "toolu_1");
    }
}
