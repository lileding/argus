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

const geminiBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// GeminiClient implements Client using Google's Gemini REST API directly.
type GeminiClient struct {
	apiKey    string
	modelName string
	maxTokens int
	client    *http.Client
}

func NewGeminiClient(_ context.Context, apiKey, modelName string, maxTokens int) (*GeminiClient, error) {
	return &GeminiClient{
		apiKey:    apiKey,
		modelName: modelName,
		maxTokens: maxTokens,
		client:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

// --- Gemini API types ---

type geminiRequest struct {
	Contents          []*geminiContent        `json:"contents"`
	Tools             []*geminiTool           `json:"tools,omitempty"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []*geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string              `json:"text,omitempty"`
	InlineData       *geminiBlob         `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResp `json:"functionResponse,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64 encoded
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type geminiFunctionResp struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiTool struct {
	FunctionDeclarations []*geminiFuncDecl `json:"functionDeclarations,omitempty"`
}

type geminiFuncDecl struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Parameters  *geminiSchema `json:"parameters,omitempty"`
}

type geminiSchema struct {
	Type        string                   `json:"type"`
	Description string                   `json:"description,omitempty"`
	Properties  map[string]*geminiSchema `json:"properties,omitempty"`
	Required    []string                 `json:"required,omitempty"`
	Items       *geminiSchema            `json:"items,omitempty"`
	Enum        []string                 `json:"enum,omitempty"`
}

type geminiGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type geminiResponse struct {
	Candidates    []*geminiCandidate   `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata,omitempty"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content"`
	FinishReason string         `json:"finishReason,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// --- Client interface ---

func (c *GeminiClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	req := c.buildRequest(messages, tools)
	data, _ := json.Marshal(req)

	body, err := c.doRequest(ctx, "generateContent", data)
	if err != nil {
		return nil, err
	}

	var resp geminiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("gemini parse: %w", err)
	}
	return convertGeminiResponse(&resp), nil
}

func (c *GeminiClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	req := c.buildRequest(messages, tools)
	data, _ := json.Marshal(req)

	httpResp, err := c.doStreamRequest(ctx, data)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		defer httpResp.Body.Close()

		var usage Usage
		reader := bufio.NewReader(httpResp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					ch <- StreamChunk{Done: true, Err: fmt.Errorf("gemini stream: %w", err), Usage: usage}
					return
				}
				break
			}
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := line[6:]
			if payload == "" {
				continue
			}

			var resp geminiResponse
			if err := json.Unmarshal([]byte(payload), &resp); err != nil {
				continue
			}

			if resp.UsageMetadata != nil {
				usage.PromptTokens = resp.UsageMetadata.PromptTokenCount
				usage.CompletionTokens = resp.UsageMetadata.CandidatesTokenCount
			}

			if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
				for _, part := range resp.Candidates[0].Content.Parts {
					if part.Text != "" {
						ch <- StreamChunk{Delta: part.Text}
					}
				}
			}
		}
		ch <- StreamChunk{Done: true, Usage: usage}
	}()
	return ch, nil
}

func (c *GeminiClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	req := c.buildRequest(messages, tools)
	data, _ := json.Marshal(req)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.streamURL(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("gemini error: status=%d body=%s", httpResp.StatusCode, body)
	}

	var content strings.Builder
	var toolCalls []ToolCall
	var usage Usage

	reader := bufio.NewReader(httpResp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				if content.Len() == 0 && len(toolCalls) == 0 {
					return nil, fmt.Errorf("gemini stream: %w", err)
				}
			}
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var resp geminiResponse
		if err := json.Unmarshal([]byte(line[6:]), &resp); err != nil {
			continue
		}

		if resp.UsageMetadata != nil {
			usage.PromptTokens = resp.UsageMetadata.PromptTokenCount
			usage.CompletionTokens = resp.UsageMetadata.CandidatesTokenCount
		}

		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			for _, part := range resp.Candidates[0].Content.Parts {
				if part.Text != "" {
					content.WriteString(part.Text)
				}
				if part.FunctionCall != nil {
					argsJSON, _ := json.Marshal(part.FunctionCall.Args)
					toolCalls = append(toolCalls, ToolCall{
						ID:   fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, len(toolCalls)),
						Type: "function",
						Function: FunctionCall{
							Name:      part.FunctionCall.Name,
							Arguments: string(argsJSON),
						},
					})
				}
			}
		}

		if len(toolCalls) == 0 && content.Len()/4 > maxTextTokens {
			slog.Info("gemini early abort", "tokens_estimate", content.Len()/4, "max", maxTextTokens)
			cancel()
			return &Response{Content: content.String(), FinishReason: "early_abort", Usage: usage}, nil
		}
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}
	return &Response{Content: content.String(), ToolCalls: toolCalls, FinishReason: finishReason, Usage: usage}, nil
}

func (c *GeminiClient) Transcribe(_ context.Context, _ []byte, _ string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("gemini does not support transcription (use openai upstream with Whisper)")
}

// --- Internal ---

func (c *GeminiClient) url() string {
	return fmt.Sprintf("%s/%s:generateContent?key=%s", geminiBaseURL, c.modelName, c.apiKey)
}

func (c *GeminiClient) streamURL() string {
	return fmt.Sprintf("%s/%s:streamGenerateContent?alt=sse&key=%s", geminiBaseURL, c.modelName, c.apiKey)
}

func (c *GeminiClient) doRequest(ctx context.Context, _ string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.url(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini error: status=%d body=%s", resp.StatusCode, respBody)
	}
	return respBody, nil
}

func (c *GeminiClient) doStreamRequest(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", c.streamURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("gemini error: status=%d body=%s", resp.StatusCode, b)
	}
	return resp, nil
}

func (c *GeminiClient) buildRequest(messages []Message, tools []ToolDef) geminiRequest {
	req := geminiRequest{
		GenerationConfig: &geminiGenerationConfig{MaxOutputTokens: c.maxTokens},
	}

	if len(tools) > 0 {
		var decls []*geminiFuncDecl
		for _, t := range tools {
			decls = append(decls, &geminiFuncDecl{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  convertToGeminiSchema(t.Function.Parameters),
			})
		}
		req.Tools = []*geminiTool{{FunctionDeclarations: decls}}
	}

	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			req.SystemInstruction = &geminiContent{
				Parts: []*geminiPart{{Text: msg.TextContent()}},
			}

		case RoleUser:
			if parts, ok := msg.Content.([]ContentPart); ok && hasImages(parts) {
				var gParts []*geminiPart
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						gParts = append(gParts, &geminiPart{Text: p.Text})
					} else if p.Type == "image_url" && p.ImageURL != nil {
						mime, data := parseDataURL(p.ImageURL.URL)
						if data != "" {
							gParts = append(gParts, &geminiPart{
								InlineData: &geminiBlob{MimeType: mime, Data: data},
							})
						}
					}
				}
				req.Contents = append(req.Contents, &geminiContent{Role: "user", Parts: gParts})
			} else {
				text := msg.TextContent()
				if text == "" {
					text = " "
				}
				req.Contents = append(req.Contents, &geminiContent{
					Role:  "user",
					Parts: []*geminiPart{{Text: text}},
				})
			}

		case RoleAssistant:
			var parts []*geminiPart
			if text := msg.TextContent(); text != "" {
				parts = append(parts, &geminiPart{Text: text})
			}
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
				parts = append(parts, &geminiPart{
					FunctionCall: &geminiFunctionCall{Name: tc.Function.Name, Args: args},
				})
			}
			if len(parts) > 0 {
				req.Contents = append(req.Contents, &geminiContent{Role: "model", Parts: parts})
			}

		case RoleTool:
			text := msg.TextContent()
			if text == "" {
				continue
			}
			var respData map[string]any
			json.Unmarshal([]byte(text), &respData)
			if respData == nil {
				respData = map[string]any{"result": text}
			}
			name := msg.ToolName
			if name == "" {
				name = msg.ToolCallID // fallback to call ID
			}
			if name == "" {
				name = "tool"
			}
			req.Contents = append(req.Contents, &geminiContent{
				Role: "user",
				Parts: []*geminiPart{{
					FunctionResponse: &geminiFunctionResp{Name: name, Response: respData},
				}},
			})
		}
	}

	return req
}

func convertGeminiResponse(resp *geminiResponse) *Response {
	result := &Response{FinishReason: "stop"}
	if resp.UsageMetadata != nil {
		result.Usage = Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
		}
	}
	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return result
	}

	var textParts []string
	for _, part := range resp.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			argsJSON, _ := json.Marshal(part.FunctionCall.Args)
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:       fmt.Sprintf("call_%s_%d", part.FunctionCall.Name, len(result.ToolCalls)),
				Type:     "function",
				Function: FunctionCall{Name: part.FunctionCall.Name, Arguments: string(argsJSON)},
			})
		}
	}
	result.Content = strings.Join(textParts, "")
	if len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	return result
}

func convertToGeminiSchema(raw json.RawMessage) *geminiSchema {
	if raw == nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return mapToGeminiSchema(m)
}

func mapToGeminiSchema(m map[string]any) *geminiSchema {
	s := &geminiSchema{}
	if t, ok := m["type"].(string); ok {
		s.Type = strings.ToUpper(t)
	}
	if d, ok := m["description"].(string); ok {
		s.Description = d
	}
	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*geminiSchema)
		for k, v := range props {
			if vm, ok := v.(map[string]any); ok {
				s.Properties[k] = mapToGeminiSchema(vm)
			}
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		s.Items = mapToGeminiSchema(items)
	}
	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			if es, ok := e.(string); ok {
				s.Enum = append(s.Enum, es)
			}
		}
	}
	return s
}
