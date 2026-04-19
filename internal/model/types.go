package model

import (
	"encoding/json"
	"time"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message represents a chat message. Content can be:
//   - string: plain text message
//   - []ContentPart: multimodal message (text + images, OpenAI vision format)
type Message struct {
	Role       Role          `json:"role"`
	Content    interface{}   `json:"content,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Meta       *MessageMeta  `json:"-"` // not serialized to LLM, used for storage metadata
}

// MessageMeta carries source metadata for persistence.
type MessageMeta struct {
	SourceIM  string     // "feishu", "cli", "cron"
	Channel   string     // specific chat/group
	SourceTS  *time.Time // timestamp from origin platform
	MsgType   string     // "text", "image", "audio", "file", "post"
	FilePaths []string   // paths to saved media files
	SenderID  string     // user identity from source IM
}

// ContentPart is a part of a multimodal message (OpenAI vision API format).
type ContentPart struct {
	Type     string    `json:"type"`               // "text" or "image_url"
	Text     string    `json:"text,omitempty"`      // for type "text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // for type "image_url"
}

type ImageURL struct {
	URL string `json:"url"` // can be a URL or "data:image/png;base64,..."
}

// TextContent returns the text content of a message, regardless of content type.
func (m Message) TextContent() string {
	switch v := m.Content.(type) {
	case string:
		return v
	case []ContentPart:
		for _, p := range v {
			if p.Type == "text" {
				return p.Text
			}
		}
	case []interface{}:
		for _, item := range v {
			if mp, ok := item.(map[string]interface{}); ok {
				if mp["type"] == "text" {
					if text, ok := mp["text"].(string); ok {
						return text
					}
				}
			}
		}
	}
	return ""
}

// NewTextMessage creates a simple text message.
func NewTextMessage(role Role, text string) Message {
	return Message{Role: role, Content: text}
}

// NewMultimodalMessage creates a message with text and images.
func NewMultimodalMessage(role Role, text string, imageDataURLs ...string) Message {
	parts := []ContentPart{{Type: "text", Text: text}}
	for _, url := range imageDataURLs {
		parts = append(parts, ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: url},
		})
	}
	return Message{Role: role, Content: parts}
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// MarshalJSON ensures Type is always "function" (never empty string).
// Some providers (GPT-5.x) reject empty type values.
func (tc ToolCall) MarshalJSON() ([]byte, error) {
	type alias ToolCall
	t := alias(tc)
	if t.Type == "" {
		t.Type = "function"
	}
	return json.Marshal(t)
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolDef struct {
	Type     string       `json:"type"`
	Function FunctionDefn `json:"function"`
}

type FunctionDefn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Response struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string // "stop" or "tool_calls"
	Usage        Usage
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
