package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"cloud.google.com/go/vertexai/genai"
	"google.golang.org/api/iterator"
)

// vertexClient implements Client for all models on Vertex AI (Gemini, Claude, etc.)
// using the unified GenerateContent API via the Google genai SDK.
type vertexClient struct {
	client    *genai.Client
	modelName string
	maxTokens int
}

func newVertexClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	client, err := genai.NewClient(ctx, project, location)
	if err != nil {
		return nil, fmt.Errorf("create vertex client: %w", err)
	}
	return &vertexClient{
		client:    client,
		modelName: modelName,
		maxTokens: maxTokens,
	}, nil
}

func (c *vertexClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	model, history, userParts := c.prepare(messages, tools)
	cs := model.StartChat()
	cs.History = history

	resp, err := cs.SendMessage(ctx, userParts...)
	if err != nil {
		return nil, fmt.Errorf("vertex chat: %w", err)
	}
	return c.convertResponse(resp), nil
}

func (c *vertexClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	model, history, userParts := c.prepare(messages, tools)
	cs := model.StartChat()
	cs.History = history

	iter := cs.SendMessageStream(ctx, userParts...)
	ch := make(chan StreamChunk, 32)

	go func() {
		defer close(ch)
		for {
			resp, err := iter.Next()
			if err == iterator.Done {
				merged := iter.MergedResponse()
				usage := Usage{}
				if merged != nil && merged.UsageMetadata != nil {
					usage.PromptTokens = int(merged.UsageMetadata.PromptTokenCount)
					usage.CompletionTokens = int(merged.UsageMetadata.CandidatesTokenCount)
				}
				ch <- StreamChunk{Done: true, Usage: usage}
				return
			}
			if err != nil {
				ch <- StreamChunk{Done: true, Err: fmt.Errorf("vertex stream: %w", err)}
				return
			}
			if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
				for _, part := range resp.Candidates[0].Content.Parts {
					if t, ok := part.(genai.Text); ok {
						ch <- StreamChunk{Delta: string(t)}
					}
				}
			}
		}
	}()
	return ch, nil
}

func (c *vertexClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	model, history, userParts := c.prepare(messages, tools)
	cs := model.StartChat()
	cs.History = history

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	iter := cs.SendMessageStream(ctx, userParts...)

	var content strings.Builder
	var functionCalls []ToolCall
	var usage Usage

	for {
		resp, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			if content.Len() > 0 || len(functionCalls) > 0 {
				break // use what we have
			}
			return nil, fmt.Errorf("vertex stream: %w", err)
		}

		if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
			for _, part := range resp.Candidates[0].Content.Parts {
				switch p := part.(type) {
				case genai.Text:
					content.WriteString(string(p))
				case genai.FunctionCall:
					argsJSON, _ := json.Marshal(p.Args)
					functionCalls = append(functionCalls, ToolCall{
						ID:   fmt.Sprintf("call_%s_%d", p.Name, len(functionCalls)),
						Type: "function",
						Function: FunctionCall{
							Name:      p.Name,
							Arguments: string(argsJSON),
						},
					})
				}
			}
		}

		if resp.UsageMetadata != nil {
			usage.PromptTokens = int(resp.UsageMetadata.PromptTokenCount)
			usage.CompletionTokens = int(resp.UsageMetadata.CandidatesTokenCount)
		}

		// Early abort: too much text, no tool calls.
		if len(functionCalls) == 0 && content.Len()/4 > maxTextTokens {
			slog.Info("vertex early abort", "tokens_estimate", content.Len()/4, "max", maxTextTokens)
			cancel()
			return &Response{
				Content:      content.String(),
				FinishReason: "early_abort",
				Usage:        usage,
			}, nil
		}
	}

	// Get final usage from merged response.
	merged := iter.MergedResponse()
	if merged != nil && merged.UsageMetadata != nil {
		usage.PromptTokens = int(merged.UsageMetadata.PromptTokenCount)
		usage.CompletionTokens = int(merged.UsageMetadata.CandidatesTokenCount)
	}

	finishReason := "stop"
	if len(functionCalls) > 0 {
		finishReason = "tool_calls"
	}

	return &Response{
		Content:      content.String(),
		ToolCalls:    functionCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

func (c *vertexClient) Transcribe(_ context.Context, _ []byte, _ string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("vertex_ai does not support transcription (use openai upstream with Whisper)")
}

// --- Internal helpers ---

// prepare builds the genai model, history, and user message parts from our
// internal Message/ToolDef types. Returns (model, history, userParts).
func (c *vertexClient) prepare(messages []Message, tools []ToolDef) (*genai.GenerativeModel, []*genai.Content, []genai.Part) {
	model := c.client.GenerativeModel(c.modelName)
	model.GenerationConfig.SetMaxOutputTokens(int32(c.maxTokens))

	// Convert tools.
	if len(tools) > 0 {
		var decls []*genai.FunctionDeclaration
		for _, t := range tools {
			decls = append(decls, convertToolDef(t))
		}
		model.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	// Split messages into system, history, and final user turn.
	var history []*genai.Content
	var userParts []genai.Part

	for i, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			model.SystemInstruction = &genai.Content{
				Parts: []genai.Part{genai.Text(msg.TextContent())},
			}
		case RoleUser:
			if i == len(messages)-1 {
				// Last user message → sent via SendMessage.
				userParts = append(userParts, genai.Text(msg.TextContent()))
			} else {
				history = append(history, genai.NewUserContent(genai.Text(msg.TextContent())))
			}
		case RoleAssistant:
			parts := []genai.Part{}
			if msg.Content != nil {
				if text := msg.TextContent(); text != "" {
					parts = append(parts, genai.Text(text))
				}
			}
			for _, tc := range msg.ToolCalls {
				var args map[string]any
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
				parts = append(parts, genai.FunctionCall{
					Name: tc.Function.Name,
					Args: args,
				})
			}
			if len(parts) > 0 {
				history = append(history, &genai.Content{Role: "model", Parts: parts})
			}
		case RoleTool:
			// Tool results go as FunctionResponse in a user content.
			var respData map[string]any
			json.Unmarshal([]byte(msg.TextContent()), &respData)
			if respData == nil {
				respData = map[string]any{"result": msg.TextContent()}
			}
			toolName := ""
			if msg.ToolCallID != "" {
				// Extract tool name from the call ID if available.
				// Our IDs are "call_<name>_<seq>", but original OpenAI IDs
				// don't have the name. Fall back to checking history.
				toolName = msg.ToolCallID
			}
			history = append(history, genai.NewUserContent(
				genai.FunctionResponse{Name: toolName, Response: respData},
			))
		}
	}

	// Ensure we have at least one user part.
	if len(userParts) == 0 {
		userParts = []genai.Part{genai.Text("")}
	}

	return model, history, userParts
}

func (c *vertexClient) convertResponse(resp *genai.GenerateContentResponse) *Response {
	result := &Response{FinishReason: "stop"}

	if resp.UsageMetadata != nil {
		result.Usage = Usage{
			PromptTokens:     int(resp.UsageMetadata.PromptTokenCount),
			CompletionTokens: int(resp.UsageMetadata.CandidatesTokenCount),
		}
	}

	if len(resp.Candidates) == 0 {
		return result
	}
	cand := resp.Candidates[0]
	if cand.Content == nil {
		return result
	}

	var textParts []string
	for _, part := range cand.Content.Parts {
		switch p := part.(type) {
		case genai.Text:
			textParts = append(textParts, string(p))
		case genai.FunctionCall:
			argsJSON, _ := json.Marshal(p.Args)
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:   fmt.Sprintf("call_%s_%d", p.Name, len(result.ToolCalls)),
				Type: "function",
				Function: FunctionCall{
					Name:      p.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	result.Content = strings.Join(textParts, "")
	if len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	return result
}

// convertToolDef converts our OpenAI-format ToolDef to genai.FunctionDeclaration.
func convertToolDef(t ToolDef) *genai.FunctionDeclaration {
	fd := &genai.FunctionDeclaration{
		Name:        t.Function.Name,
		Description: t.Function.Description,
	}
	if t.Function.Parameters != nil {
		fd.Parameters = convertJSONSchema(t.Function.Parameters)
	}
	return fd
}

// convertJSONSchema recursively converts a JSON Schema (as map[string]any)
// to a genai.Schema.
func convertJSONSchema(raw json.RawMessage) *genai.Schema {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	return convertSchemaMap(schema)
}

func convertSchemaMap(m map[string]any) *genai.Schema {
	s := &genai.Schema{}

	if t, ok := m["type"].(string); ok {
		switch t {
		case "object":
			s.Type = genai.TypeObject
		case "array":
			s.Type = genai.TypeArray
		case "string":
			s.Type = genai.TypeString
		case "number":
			s.Type = genai.TypeNumber
		case "integer":
			s.Type = genai.TypeInteger
		case "boolean":
			s.Type = genai.TypeBoolean
		}
	}

	if desc, ok := m["description"].(string); ok {
		s.Description = desc
	}

	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*genai.Schema)
		for k, v := range props {
			if vm, ok := v.(map[string]any); ok {
				s.Properties[k] = convertSchemaMap(vm)
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
		s.Items = convertSchemaMap(items)
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
