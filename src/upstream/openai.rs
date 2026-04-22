//! OpenAI-compatible chat completions client.
//! Works with api.openai.com and local servers (vLLM, Ollama, etc.).

use std::time::Duration;

use futures::{Stream, StreamExt};
use reqwest_eventsource::{Event as SseEvent, EventSource};
use serde::{Deserialize, Serialize};
use tracing::debug;

use super::Client;
use super::types::*;
use crate::config::{RoleConfig, UpstreamConfig};

/// Detect api.openai.com: new models require max_completion_tokens instead of max_tokens.
fn is_openai_api(base_url: &str) -> bool {
    base_url.contains("api.openai.com")
}

pub(super) struct OpenAiClient {
    http: reqwest::Client,
    base_url: String,
    api_key: String,
    model: String,
    max_tokens: usize,
    use_new_token_field: bool,
}

impl OpenAiClient {
    pub(super) fn new(upstream: &UpstreamConfig, role: &RoleConfig) -> Self {
        let base_url = upstream.effective_base_url().to_string();
        let http = reqwest::Client::builder()
            .timeout(Duration::from_secs(upstream.timeout_secs))
            .build()
            .expect("failed to build HTTP client");

        Self {
            use_new_token_field: is_openai_api(&base_url),
            http,
            base_url,
            api_key: upstream.api_key.clone(),
            model: role.model_name.clone(),
            max_tokens: role.max_tokens,
        }
    }

    fn chat_url(&self) -> String {
        format!("{}/chat/completions", self.base_url)
    }

    fn build_request(&self, messages: &[Message], tools: &[ToolDef], stream: bool) -> ChatRequest {
        let oai_messages: Vec<OaiMessage> = messages.iter().map(|m| m.into()).collect();
        let oai_tools: Vec<OaiTool> = tools.iter().map(|t| t.into()).collect();

        ChatRequest {
            model: self.model.clone(),
            messages: oai_messages,
            tools: if oai_tools.is_empty() {
                None
            } else {
                Some(oai_tools)
            },
            stream,
            stream_options: if stream {
                Some(StreamOptions {
                    include_usage: true,
                })
            } else {
                None
            },
            max_tokens: if self.use_new_token_field {
                None
            } else {
                Some(self.max_tokens)
            },
            max_completion_tokens: if self.use_new_token_field {
                Some(self.max_tokens)
            } else {
                None
            },
        }
    }

    fn auth_request(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        if self.api_key.is_empty() {
            req
        } else {
            req.bearer_auth(&self.api_key)
        }
    }

    /// Parse a non-streaming response.
    fn parse_response(body: &str) -> ClientResult<Response> {
        let resp: ChatResponse = serde_json::from_str(body)?;
        if let Some(err) = resp.error {
            return Err(ClientError::Api {
                status: 0,
                message: err.message,
            });
        }
        let choice = resp
            .choices
            .first()
            .ok_or_else(|| ClientError::Other("empty choices in response".into()))?;

        let tool_calls = choice
            .message
            .tool_calls
            .as_ref()
            .map(|tcs| {
                tcs.iter()
                    .map(|tc| ToolCall {
                        id: tc.id.clone(),
                        name: tc.function.name.clone(),
                        arguments: tc.function.arguments.clone(),
                    })
                    .collect()
            })
            .unwrap_or_default();

        let finish_reason = match choice.finish_reason.as_deref() {
            Some("stop") => FinishReason::Stop,
            Some("tool_calls") => FinishReason::ToolCalls,
            Some("length") => FinishReason::Length,
            Some(other) => FinishReason::Other(other.to_string()),
            None => FinishReason::Stop,
        };

        Ok(Response {
            content: choice.message.content.clone().unwrap_or_default(),
            tool_calls,
            finish_reason,
            usage: Usage {
                prompt_tokens: resp.usage.prompt_tokens,
                completion_tokens: resp.usage.completion_tokens,
                total_tokens: resp.usage.total_tokens,
            },
        })
    }
}

#[async_trait::async_trait]
impl Client for OpenAiClient {
    async fn chat(&self, messages: &[Message], tools: &[ToolDef]) -> ClientResult<Response> {
        let body = self.build_request(messages, tools, false);
        let resp = self
            .auth_request(self.http.post(self.chat_url()))
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
            .auth_request(self.http.post(self.chat_url()))
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
        let mut finish_reason = FinishReason::Stop;

        // Accumulate tool call arguments by index.
        // Tool calls arrive incrementally: first delta has id+name, subsequent
        // deltas append to arguments.
        let mut pending_tools: Vec<PendingToolCall> = Vec::new();

        while let Some(chunk) = stream.next().await {
            if let Some(err) = &chunk.error {
                return Err(ClientError::Sse(err.clone()));
            }

            content.push_str(&chunk.delta);

            // Accumulate tool call deltas.
            for tcd in &chunk.tool_call_deltas {
                // Grow pending_tools to accommodate this index.
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

            // Early abort: if text grows too long and no tool calls detected.
            if pending_tools.is_empty() && content.len() / 4 > max_text_tokens {
                debug!(
                    text_len = content.len(),
                    max_text_tokens, "early abort: text exceeds threshold"
                );
                finish_reason = FinishReason::EarlyAbort;
                break;
            }
        }

        // Finalize pending tool calls.
        let tool_calls: Vec<ToolCall> = pending_tools
            .into_iter()
            .filter(|pt| !pt.name.is_empty())
            .map(|pt| ToolCall {
                id: pt.id,
                name: pt.name,
                arguments: pt.arguments,
            })
            .collect();

        if !tool_calls.is_empty() {
            finish_reason = FinishReason::ToolCalls;
        }

        Ok(Response {
            content,
            tool_calls,
            finish_reason,
            usage,
        })
    }
}

// --- SSE stream conversion ---

/// Convert an EventSource into an async Stream of StreamChunks.
fn sse_to_stream(mut es: EventSource) -> impl Stream<Item = StreamChunk> {
    async_stream::stream! {
        loop {
            match es.next().await {
                Some(Ok(SseEvent::Message(msg))) => {
                    let data = msg.data.trim();
                    if data == "[DONE]" {
                        yield StreamChunk { delta: String::new(), tool_call_deltas: vec![], done: true, usage: None, error: None };
                        break;
                    }
                    if let Some(chunk) = parse_sse_data(data) {
                        yield chunk;
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
                    yield StreamChunk { delta: String::new(), tool_call_deltas: vec![], done: true, usage: None, error: None };
                    break;
                }
            }
        }
    }
}

fn parse_sse_data(data: &str) -> Option<StreamChunk> {
    let resp: SseResponse = serde_json::from_str(data).ok()?;
    let choice = resp.choices.first()?;
    let delta = &choice.delta;

    let text = delta.content.as_deref().unwrap_or("");

    // Parse tool call deltas.
    let tool_call_deltas = delta
        .tool_calls
        .as_ref()
        .map(|tcs| {
            tcs.iter()
                .map(|tc| ToolCallDelta {
                    index: tc.index,
                    id: tc.id.clone(),
                    name: tc.function.as_ref().and_then(|f| f.name.clone()),
                    arguments_delta: tc
                        .function
                        .as_ref()
                        .and_then(|f| f.arguments.clone())
                        .unwrap_or_default(),
                })
                .collect()
        })
        .unwrap_or_default();

    let usage = resp.usage.map(|u| Usage {
        prompt_tokens: u.prompt_tokens,
        completion_tokens: u.completion_tokens,
        total_tokens: u.total_tokens,
    });

    let done = choice.finish_reason.is_some();

    Some(StreamChunk {
        delta: text.to_string(),
        tool_call_deltas,
        done,
        usage,
        error: None,
    })
}

// --- Tracking tool calls during streaming ---

struct PendingToolCall {
    id: String,
    name: String,
    arguments: String,
}

// --- OpenAI API types (serialization) ---

#[derive(Serialize)]
struct ChatRequest {
    model: String,
    messages: Vec<OaiMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tools: Option<Vec<OaiTool>>,
    stream: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    stream_options: Option<StreamOptions>,
    #[serde(skip_serializing_if = "Option::is_none")]
    max_tokens: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    max_completion_tokens: Option<usize>,
}

#[derive(Serialize)]
struct StreamOptions {
    include_usage: bool,
}

#[derive(Serialize)]
struct OaiMessage {
    role: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    content: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tool_calls: Option<Vec<OaiToolCall>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tool_call_id: Option<String>,
}

impl From<&Message> for OaiMessage {
    fn from(msg: &Message) -> Self {
        let content = if msg.parts.is_empty() {
            Some(serde_json::Value::String(msg.content.clone()))
        } else {
            // Multimodal: array of content parts.
            let parts: Vec<serde_json::Value> = msg
                .parts
                .iter()
                .map(|p| match p {
                    ContentPart::Text { text } => serde_json::json!({"type": "text", "text": text}),
                    ContentPart::ImageUrl { url } => {
                        serde_json::json!({"type": "image_url", "image_url": {"url": url}})
                    }
                })
                .collect();
            Some(serde_json::Value::Array(parts))
        };

        let tool_calls = if msg.tool_calls.is_empty() {
            None
        } else {
            Some(
                msg.tool_calls
                    .iter()
                    .map(|tc| OaiToolCall {
                        id: tc.id.clone(),
                        r#type: "function".into(),
                        function: OaiFunctionCall {
                            name: tc.name.clone(),
                            arguments: tc.arguments.clone(),
                        },
                    })
                    .collect(),
            )
        };

        OaiMessage {
            role: match msg.role {
                Role::System => "system".into(),
                Role::User => "user".into(),
                Role::Assistant => "assistant".into(),
                Role::Tool => "tool".into(),
            },
            content,
            tool_calls,
            tool_call_id: msg.tool_call_id.clone(),
        }
    }
}

#[derive(Serialize)]
struct OaiTool {
    r#type: String,
    function: OaiFunction,
}

impl From<&ToolDef> for OaiTool {
    fn from(td: &ToolDef) -> Self {
        OaiTool {
            r#type: "function".into(),
            function: OaiFunction {
                name: td.name.clone(),
                description: td.description.clone(),
                parameters: td.parameters.clone(),
            },
        }
    }
}

#[derive(Serialize)]
struct OaiFunction {
    name: String,
    description: String,
    parameters: serde_json::Value,
}

#[derive(Serialize, Deserialize)]
struct OaiToolCall {
    id: String,
    r#type: String,
    function: OaiFunctionCall,
}

#[derive(Serialize, Deserialize)]
struct OaiFunctionCall {
    name: String,
    arguments: String,
}

// --- OpenAI API types (deserialization) ---

#[derive(Deserialize)]
struct ChatResponse {
    #[serde(default)]
    choices: Vec<ChatChoice>,
    #[serde(default)]
    usage: OaiUsage,
    #[serde(default)]
    error: Option<ApiError>,
}

#[derive(Deserialize)]
struct ChatChoice {
    message: ChatChoiceMessage,
    #[serde(default)]
    finish_reason: Option<String>,
}

#[derive(Deserialize)]
struct ChatChoiceMessage {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    tool_calls: Option<Vec<OaiToolCall>>,
}

#[derive(Deserialize, Default)]
struct OaiUsage {
    #[serde(default)]
    prompt_tokens: u32,
    #[serde(default)]
    completion_tokens: u32,
    #[serde(default)]
    total_tokens: u32,
}

#[derive(Deserialize)]
struct ApiError {
    message: String,
}

// --- SSE response types ---

#[derive(Deserialize)]
struct SseResponse {
    #[serde(default)]
    choices: Vec<SseChoice>,
    #[serde(default)]
    usage: Option<OaiUsage>,
}

#[derive(Deserialize)]
struct SseChoice {
    delta: SseDelta,
    #[serde(default)]
    finish_reason: Option<String>,
}

#[derive(Deserialize)]
struct SseDelta {
    #[serde(default)]
    content: Option<String>,
    #[serde(default)]
    tool_calls: Option<Vec<SseToolCallDelta>>,
}

#[derive(Deserialize)]
struct SseToolCallDelta {
    #[serde(default)]
    index: usize,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    function: Option<SseFunctionDelta>,
}

#[derive(Deserialize)]
struct SseFunctionDelta {
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    arguments: Option<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn message_to_oai_text() {
        let msg = Message::user("hello");
        let oai: OaiMessage = (&msg).into();
        assert_eq!(oai.role, "user");
        assert_eq!(oai.content, Some(serde_json::json!("hello")));
        assert!(oai.tool_calls.is_none());
    }

    #[test]
    fn message_to_oai_system() {
        let msg = Message::system("be helpful");
        let oai: OaiMessage = (&msg).into();
        assert_eq!(oai.role, "system");
    }

    #[test]
    fn message_to_oai_tool_result() {
        let msg = Message::tool_result("call_1", "search", "found it");
        let oai: OaiMessage = (&msg).into();
        assert_eq!(oai.role, "tool");
        assert_eq!(oai.tool_call_id, Some("call_1".into()));
    }

    #[test]
    fn tool_def_to_oai() {
        let td = ToolDef {
            name: "search".into(),
            description: "Web search".into(),
            parameters: serde_json::json!({"type": "object"}),
        };
        let oai: OaiTool = (&td).into();
        assert_eq!(oai.r#type, "function");
        assert_eq!(oai.function.name, "search");
    }

    #[test]
    fn parse_chat_response() {
        let json = r#"{
            "choices": [{
                "message": {"role": "assistant", "content": "hello"},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
        }"#;
        let resp = OpenAiClient::parse_response(json).unwrap();
        assert_eq!(resp.content, "hello");
        assert_eq!(resp.finish_reason, FinishReason::Stop);
        assert_eq!(resp.usage.prompt_tokens, 10);
        assert!(resp.tool_calls.is_empty());
    }

    #[test]
    fn parse_chat_response_with_tools() {
        let json = r#"{
            "choices": [{
                "message": {
                    "role": "assistant",
                    "content": null,
                    "tool_calls": [{
                        "id": "call_1",
                        "type": "function",
                        "function": {"name": "search", "arguments": "{\"query\":\"rust\"}"}
                    }]
                },
                "finish_reason": "tool_calls"
            }],
            "usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
        }"#;
        let resp = OpenAiClient::parse_response(json).unwrap();
        assert_eq!(resp.finish_reason, FinishReason::ToolCalls);
        assert_eq!(resp.tool_calls.len(), 1);
        assert_eq!(resp.tool_calls[0].name, "search");
        assert_eq!(resp.tool_calls[0].arguments, r#"{"query":"rust"}"#);
    }

    #[test]
    fn parse_sse_text_chunk() {
        let data = r#"{"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}"#;
        let chunk = parse_sse_data(data).unwrap();
        assert_eq!(chunk.delta, "hello");
        assert!(!chunk.done);
    }

    #[test]
    fn parse_sse_final_chunk() {
        let data = r#"{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}"#;
        let chunk = parse_sse_data(data).unwrap();
        assert!(chunk.done);
        let usage = chunk.usage.unwrap();
        assert_eq!(usage.prompt_tokens, 10);
    }

    #[test]
    fn is_openai_api_detection() {
        assert!(is_openai_api("https://api.openai.com/v1"));
        assert!(!is_openai_api("http://localhost:8000/v1"));
    }

    #[test]
    fn new_token_field_for_openai() {
        let upstream = UpstreamConfig {
            provider_type: "openai".into(),
            base_url: "https://api.openai.com/v1".into(),
            api_key: "sk-test".into(),
            timeout_secs: 120,
        };
        let role = RoleConfig {
            upstream: "openai".into(),
            model_name: "gpt-5.4".into(),
            max_tokens: 4096,
        };
        let client = OpenAiClient::new(&upstream, &role);
        assert!(client.use_new_token_field);

        let req = client.build_request(&[], &[], false);
        assert!(req.max_tokens.is_none());
        assert_eq!(req.max_completion_tokens, Some(4096));
    }

    #[test]
    fn legacy_token_field_for_local() {
        let upstream = UpstreamConfig {
            provider_type: "openai".into(),
            base_url: "http://localhost:8000/v1".into(),
            api_key: String::new(),
            timeout_secs: 120,
        };
        let role = RoleConfig {
            upstream: "local".into(),
            model_name: "qwen".into(),
            max_tokens: 4096,
        };
        let client = OpenAiClient::new(&upstream, &role);
        assert!(!client.use_new_token_field);

        let req = client.build_request(&[], &[], false);
        assert_eq!(req.max_tokens, Some(4096));
        assert!(req.max_completion_tokens.is_none());
    }
}
