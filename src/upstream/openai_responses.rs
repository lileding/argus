//! OpenAI Responses API client (`/v1/responses`).
//!
//! Used for GPT models with reasoning + tools, which is not supported
//! by the Chat Completions API. Registered as provider type "openai-response".

use std::time::Duration;

use futures::{Stream, StreamExt};
use reqwest_eventsource::{Event as SseEvent, EventSource};
use serde::{Deserialize, Serialize};
use tracing::debug;

use super::Client;
use super::types::*;
use crate::config::{RoleConfig, UpstreamConfig};

pub(super) struct OpenAiResponsesClient {
    http: reqwest::Client,
    base_url: String,
    api_key: String,
    model: String,
    max_tokens: usize,
}

impl OpenAiResponsesClient {
    pub(super) fn new(upstream: &UpstreamConfig, role: &RoleConfig) -> Self {
        let base_url = upstream.effective_base_url().to_string();
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

    fn responses_url(&self) -> String {
        format!("{}/responses", self.base_url)
    }

    fn build_request(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
        stream: bool,
        options: &ChatOptions,
    ) -> ResponsesRequest {
        // Extract system message as instructions; rest as input items.
        let mut instructions: Option<String> = None;
        let mut input: Vec<serde_json::Value> = Vec::new();

        for msg in messages {
            match msg.role {
                Role::System => {
                    instructions = Some(msg.content.clone());
                }
                Role::User => {
                    if msg.parts.is_empty() {
                        input.push(serde_json::json!({
                            "role": "user",
                            "content": msg.content
                        }));
                    } else {
                        let parts: Vec<serde_json::Value> = msg
                            .parts
                            .iter()
                            .map(|p| match p {
                                ContentPart::Text { text } => {
                                    serde_json::json!({"type": "input_text", "text": text})
                                }
                                ContentPart::Image { media_type, data } => {
                                    let url = format!("data:{media_type};base64,{data}");
                                    serde_json::json!({"type": "input_image", "image_url": url})
                                }
                            })
                            .collect();
                        input.push(serde_json::json!({
                            "role": "user",
                            "content": parts
                        }));
                    }
                }
                Role::Assistant => {
                    // Assistant message with tool calls → output items.
                    if msg.tool_calls.is_empty() {
                        input.push(serde_json::json!({
                            "role": "assistant",
                            "content": msg.content
                        }));
                    } else {
                        // Add text if present.
                        if !msg.content.is_empty() {
                            input.push(serde_json::json!({
                                "type": "message",
                                "role": "assistant",
                                "content": [{"type": "output_text", "text": msg.content}]
                            }));
                        }
                        // Add function calls.
                        for tc in &msg.tool_calls {
                            input.push(serde_json::json!({
                                "type": "function_call",
                                "call_id": tc.id,
                                "name": tc.name,
                                "arguments": tc.arguments
                            }));
                        }
                    }
                }
                Role::Tool => {
                    // Tool result → function_call_output.
                    input.push(serde_json::json!({
                        "type": "function_call_output",
                        "call_id": msg.tool_call_id,
                        "output": msg.content
                    }));
                }
            }
        }

        let api_tools: Vec<serde_json::Value> = tools
            .iter()
            .map(|t| {
                serde_json::json!({
                    "type": "function",
                    "name": t.name,
                    "description": t.description,
                    "parameters": t.parameters
                })
            })
            .collect();

        let reasoning = if options.thinking_budget > 0 {
            Some(serde_json::json!({"effort": "medium"}))
        } else {
            None
        };

        ResponsesRequest {
            model: self.model.clone(),
            instructions,
            input,
            tools: if api_tools.is_empty() {
                None
            } else {
                Some(api_tools)
            },
            reasoning,
            max_output_tokens: Some(self.max_tokens),
            stream,
        }
    }

    fn auth_request(&self, req: reqwest::RequestBuilder) -> reqwest::RequestBuilder {
        if self.api_key.is_empty() {
            req
        } else {
            req.bearer_auth(&self.api_key)
        }
    }

    fn parse_response(body: &str) -> ClientResult<Response> {
        let resp: ResponsesResponse = serde_json::from_str(body)?;
        if let Some(err) = resp.error {
            return Err(ClientError::Api {
                status: 0,
                message: err.message,
            });
        }

        let mut content = String::new();
        let mut tool_calls = Vec::new();

        for item in &resp.output {
            match item.item_type.as_str() {
                "message" => {
                    if let Some(content_parts) = &item.content {
                        for part in content_parts {
                            if part.part_type == "output_text"
                                && let Some(text) = &part.text
                            {
                                content.push_str(text);
                            }
                        }
                    }
                }
                "function_call" => {
                    tool_calls.push(ToolCall {
                        id: item.id.clone().unwrap_or_default(),
                        name: item.name.clone().unwrap_or_default(),
                        arguments: item.arguments.clone().unwrap_or_default(),
                    });
                }
                _ => {}
            }
        }

        let finish_reason = match resp.status.as_deref() {
            Some("completed") => {
                if tool_calls.is_empty() {
                    FinishReason::Stop
                } else {
                    FinishReason::ToolCalls
                }
            }
            Some("incomplete") => FinishReason::Length,
            Some(other) => FinishReason::Other(other.to_string()),
            None => FinishReason::Stop,
        };

        Ok(Response {
            reasoning_content: None,
            content,
            tool_calls,
            finish_reason,
            usage: Usage {
                prompt_tokens: resp.usage.input_tokens,
                completion_tokens: resp.usage.output_tokens,
                total_tokens: resp.usage.total_tokens,
            },
        })
    }
}

#[async_trait::async_trait]
impl Client for OpenAiResponsesClient {
    async fn chat(
        &self,
        messages: &[Message],
        tools: &[ToolDef],
        options: &ChatOptions,
    ) -> ClientResult<Response> {
        let body = self.build_request(messages, tools, false, options);
        let resp = self
            .auth_request(self.http.post(self.responses_url()))
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
        options: &ChatOptions,
    ) -> ClientResult<ChunkStream> {
        let body = self.build_request(messages, tools, true, options);
        let req = self
            .auth_request(self.http.post(self.responses_url()))
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
        options: &ChatOptions,
    ) -> ClientResult<Response> {
        let mut stream = self.chat_stream(messages, tools, options).await?;

        let mut content = String::new();
        let mut usage = Usage::default();
        let mut finish_reason = FinishReason::Stop;
        let mut pending_tools: Vec<PendingToolCall> = Vec::new();

        while let Some(chunk) = stream.next().await {
            if let Some(err) = &chunk.error {
                return Err(ClientError::Sse(err.clone()));
            }

            content.push_str(&chunk.delta);

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

            // Early abort: text too long, no tool calls.
            if pending_tools.is_empty() && content.len() / 4 > max_text_tokens {
                debug!(
                    text_len = content.len(),
                    max_text_tokens, "early abort: text exceeds threshold"
                );
                finish_reason = FinishReason::EarlyAbort;
                break;
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

        if !tool_calls.is_empty() {
            finish_reason = FinishReason::ToolCalls;
        }

        Ok(Response {
            content,
            reasoning_content: None,
            tool_calls,
            finish_reason,
            usage,
        })
    }
}

// --- SSE stream conversion for Responses API ---

/// Responses API SSE events:
/// - response.output_text.delta: text content
/// - response.function_call_arguments.delta: tool call arguments
/// - response.output_item.added: new output item (function_call with id+name)
/// - response.completed: done
fn sse_to_stream(mut es: EventSource) -> impl Stream<Item = StreamChunk> {
    let mut tool_index: usize = 0;

    async_stream::stream! {
        loop {
            match es.next().await {
                Some(Ok(SseEvent::Message(msg))) => {
                    let event_type = msg.event.as_str();
                    let data = msg.data.trim();

                    match event_type {
                        "response.output_text.delta" => {
                            if let Ok(v) = serde_json::from_str::<serde_json::Value>(data) {
                                let delta = v.get("delta").and_then(|d| d.as_str()).unwrap_or("");
                                if !delta.is_empty() {
                                    yield StreamChunk {
                                        delta: delta.to_string(),
                                        reasoning_delta: String::new(),
                                        tool_call_deltas: vec![],
                                        done: false,
                                        usage: None,
                                        error: None,
                                    };
                                }
                            }
                        }
                        "response.output_item.added" => {
                            if let Ok(v) = serde_json::from_str::<serde_json::Value>(data)
                                && let Some(item) = v.get("item")
                                && item.get("type").and_then(|t| t.as_str()) == Some("function_call")
                            {
                                let id = item.get("id").and_then(|i| i.as_str()).unwrap_or("").to_string();
                                let name = item.get("name").and_then(|n| n.as_str()).unwrap_or("").to_string();
                                yield StreamChunk {
                                    delta: String::new(), reasoning_delta: String::new(),
                                    tool_call_deltas: vec![ToolCallDelta {
                                        index: tool_index,
                                        id: Some(id),
                                        name: Some(name),
                                        arguments_delta: String::new(),
                                    }],
                                    done: false,
                                    usage: None,
                                    error: None,
                                };
                            }
                        }
                        "response.function_call_arguments.delta" => {
                            if let Ok(v) = serde_json::from_str::<serde_json::Value>(data) {
                                let delta = v.get("delta").and_then(|d| d.as_str()).unwrap_or("");
                                if !delta.is_empty() {
                                    yield StreamChunk {
                                        delta: String::new(), reasoning_delta: String::new(),
                                        tool_call_deltas: vec![ToolCallDelta {
                                            index: tool_index,
                                            id: None,
                                            name: None,
                                            arguments_delta: delta.to_string(),
                                        }],
                                        done: false,
                                        usage: None,
                                        error: None,
                                    };
                                }
                            }
                        }
                        "response.function_call_arguments.done" => {
                            // Tool call complete, increment index for next one.
                            tool_index += 1;
                        }
                        "response.completed" => {
                            // Extract usage from the completed event.
                            let usage = if let Ok(v) = serde_json::from_str::<serde_json::Value>(data) {
                                v.get("response").and_then(|r| r.get("usage")).map(|u| Usage {
                                    prompt_tokens: u.get("input_tokens").and_then(|t| t.as_u64()).unwrap_or(0) as u32,
                                    completion_tokens: u.get("output_tokens").and_then(|t| t.as_u64()).unwrap_or(0) as u32,
                                    total_tokens: u.get("total_tokens").and_then(|t| t.as_u64()).unwrap_or(0) as u32,
                                })
                            } else {
                                None
                            };
                            yield StreamChunk {
                                delta: String::new(), reasoning_delta: String::new(),
                                tool_call_deltas: vec![],
                                done: true,
                                usage,
                                error: None,
                            };
                            break;
                        }
                        _ => {} // Ignore other events
                    }
                }
                Some(Ok(SseEvent::Open)) => {}
                Some(Err(e)) => {
                    yield StreamChunk {
                        delta: String::new(), reasoning_delta: String::new(),
                        tool_call_deltas: vec![],
                        done: true,
                        usage: None,
                        error: Some(e.to_string()),
                    };
                    break;
                }
                None => {
                    yield StreamChunk {
                        delta: String::new(), reasoning_delta: String::new(),
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

// --- Responses API types ---

#[derive(Serialize)]
struct ResponsesRequest {
    model: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    instructions: Option<String>,
    input: Vec<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    tools: Option<Vec<serde_json::Value>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    reasoning: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    max_output_tokens: Option<usize>,
    stream: bool,
}

#[derive(Deserialize)]
struct ResponsesResponse {
    #[serde(default)]
    output: Vec<OutputItem>,
    #[serde(default)]
    status: Option<String>,
    #[serde(default)]
    usage: ResponsesUsage,
    #[serde(default)]
    error: Option<ApiError>,
}

#[derive(Deserialize)]
struct OutputItem {
    #[serde(rename = "type")]
    item_type: String,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    name: Option<String>,
    #[serde(default)]
    arguments: Option<String>,
    #[serde(default)]
    content: Option<Vec<OutputContentPart>>,
}

#[derive(Deserialize)]
struct OutputContentPart {
    #[serde(rename = "type")]
    part_type: String,
    #[serde(default)]
    text: Option<String>,
}

#[derive(Deserialize, Default)]
struct ResponsesUsage {
    #[serde(default)]
    input_tokens: u32,
    #[serde(default)]
    output_tokens: u32,
    #[serde(default)]
    total_tokens: u32,
}

#[derive(Deserialize)]
struct ApiError {
    message: String,
}
