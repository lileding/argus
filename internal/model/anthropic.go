package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const anthropicAPI = "https://api.anthropic.com/v1/messages"
const anthropicVersion = "2023-06-01"

type AnthropicClient struct {
	apiKey    string
	modelName string
	maxTokens int
	client    *http.Client
}

func NewAnthropicClient(apiKey, modelName string, maxTokens int, timeout time.Duration) *AnthropicClient {
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	return &AnthropicClient{
		apiKey:    apiKey,
		modelName: modelName,
		maxTokens: maxTokens,
		client:    &http.Client{Timeout: timeout},
	}
}

// --- Anthropic API types ---

type anthropicRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    string            `json:"system,omitempty"`
	Messages  []anthropicMsg    `json:"messages"`
	Tools     []anthropicTool   `json:"tools,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []anthropicContentBlock
}

type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string         `json:"type"`
		Text  string         `json:"text,omitempty"`
		ID    string         `json:"id,omitempty"`
		Name  string         `json:"name,omitempty"`
		Input map[string]any `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"` // end_turn, tool_use, max_tokens
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// --- Client interface implementation ---

func (c *AnthropicClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	reqBody := c.buildRequest(messages, tools, false)
	data, _ := json.Marshal(reqBody)

	resp, err := c.doRequest(ctx, data)
	if err != nil {
		return nil, err
	}

	return c.convertResponse(resp), nil
}

func (c *AnthropicClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	reqBody := c.buildRequest(messages, tools, true)
	data, _ := json.Marshal(reqBody)

	httpResp, err := c.doStreamRequest(ctx, data)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()
		c.consumeStream(httpResp.Body, ch, false, 0)
	}()
	return ch, nil
}

func (c *AnthropicClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	reqBody := c.buildRequest(messages, tools, true)
	data, _ := json.Marshal(reqBody)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPI, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(req)

	httpResp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("anthropic error: status=%d body=%s", httpResp.StatusCode, body)
	}

	// Consume stream with early abort.
	var content strings.Builder
	var toolCalls []ToolCall
	var usage Usage
	var currentToolID, currentToolName string
	var currentToolArgs strings.Builder

	reader := bufio.NewReader(httpResp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			if content.Len() > 0 || len(toolCalls) > 0 {
				break
			}
			return nil, fmt.Errorf("read stream: %w", err)
		}

		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]
		if payload == "[DONE]" {
			break
		}

		var event struct {
			Type         string `json:"type"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block,omitempty"`
			Delta *struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			} `json:"delta,omitempty"`
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage,omitempty"`
			Message *struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage.PromptTokens = event.Message.Usage.InputTokens
			}
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				currentToolID = event.ContentBlock.ID
				currentToolName = event.ContentBlock.Name
				currentToolArgs.Reset()
			}
		case "content_block_delta":
			if event.Delta != nil {
				switch event.Delta.Type {
				case "text_delta":
					content.WriteString(event.Delta.Text)
				case "input_json_delta":
					currentToolArgs.WriteString(event.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if currentToolName != "" {
				toolCalls = append(toolCalls, ToolCall{
					ID:   currentToolID,
					Type: "function",
					Function: FunctionCall{
						Name:      currentToolName,
						Arguments: currentToolArgs.String(),
					},
				})
				currentToolName = ""
			}
		case "message_delta":
			if event.Usage != nil {
				usage.CompletionTokens = event.Usage.OutputTokens
			}
		}

		// Early abort: too much text, no tool calls.
		if len(toolCalls) == 0 && currentToolName == "" && content.Len()/4 > maxTextTokens {
			slog.Info("anthropic early abort", "tokens_estimate", content.Len()/4, "max", maxTextTokens)
			cancel()
			return &Response{
				Content:      content.String(),
				FinishReason: "early_abort",
				Usage:        usage,
			}, nil
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	return &Response{
		Content:      content.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func (c *AnthropicClient) Transcribe(_ context.Context, _ []byte, _ string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("anthropic does not support transcription (use openai upstream with Whisper)")
}

// --- Internal helpers ---

func (c *AnthropicClient) buildRequest(messages []Message, tools []ToolDef, stream bool) anthropicRequest {
	req := anthropicRequest{
		Model:     c.modelName,
		MaxTokens: c.maxTokens,
		Stream:    stream,
	}

	// Convert tools.
	for _, t := range tools {
		req.Tools = append(req.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}

	// Convert messages: system goes to top-level, rest to messages array.
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			req.System = msg.TextContent()

		case RoleUser:
			req.Messages = append(req.Messages, anthropicMsg{
				Role:    "user",
				Content: msg.TextContent(),
			})

		case RoleAssistant:
			if len(msg.ToolCalls) > 0 {
				// Assistant with tool calls → content blocks.
				var blocks []anthropicContentBlock
				if text := msg.TextContent(); text != "" {
					blocks = append(blocks, anthropicContentBlock{Type: "text", Text: text})
				}
				for _, tc := range msg.ToolCalls {
					var input map[string]any
					json.Unmarshal([]byte(tc.Function.Arguments), &input)
					blocks = append(blocks, anthropicContentBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: input,
					})
				}
				req.Messages = append(req.Messages, anthropicMsg{Role: "assistant", Content: blocks})
			} else {
				req.Messages = append(req.Messages, anthropicMsg{
					Role:    "assistant",
					Content: msg.TextContent(),
				})
			}

		case RoleTool:
			// Tool result → user message with tool_result block.
			req.Messages = append(req.Messages, anthropicMsg{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type:      "tool_result",
					ToolUseID: msg.ToolCallID,
					Content:   msg.TextContent(),
				}},
			})
		}
	}

	return req
}

func (c *AnthropicClient) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func (c *AnthropicClient) doRequest(ctx context.Context, body []byte) (*anthropicResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPI, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic error: status=%d body=%s", resp.StatusCode, respBody)
	}

	var result anthropicResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if result.Error != nil {
		return nil, fmt.Errorf("anthropic error: %s", result.Error.Message)
	}

	return &result, nil
}

func (c *AnthropicClient) doStreamRequest(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPI, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setHeaders(req)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic error: status=%d body=%s", resp.StatusCode, body)
	}

	return resp, nil
}

func (c *AnthropicClient) convertResponse(resp *anthropicResponse) *Response {
	result := &Response{
		FinishReason: "stop",
		Usage: Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
		},
	}

	var textParts []string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			argsJSON, _ := json.Marshal(block.Input)
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: FunctionCall{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	result.Content = strings.Join(textParts, "")
	if resp.StopReason == "tool_use" || len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	return result
}

func (c *AnthropicClient) consumeStream(body io.Reader, ch chan<- StreamChunk, earlyAbort bool, maxTextTokens int) {
	reader := bufio.NewReader(body)
	var usage Usage

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			ch <- StreamChunk{Done: true, Usage: usage}
			if err != io.EOF {
				ch <- StreamChunk{Done: true, Err: fmt.Errorf("read stream: %w", err), Usage: usage}
			}
			return
		}

		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := line[6:]

		var event struct {
			Type  string `json:"type"`
			Delta *struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
			} `json:"delta,omitempty"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage,omitempty"`
			Message *struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message,omitempty"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage.PromptTokens = event.Message.Usage.InputTokens
			}
		case "content_block_delta":
			if event.Delta != nil && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				ch <- StreamChunk{Delta: event.Delta.Text}
			}
		case "message_delta":
			if event.Usage != nil {
				usage.CompletionTokens = event.Usage.OutputTokens
			}
		case "message_stop":
			ch <- StreamChunk{Done: true, Usage: usage}
			return
		}
	}
}
