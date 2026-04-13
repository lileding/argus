package feishu

import "encoding/json"

// EventEnvelope is the top-level structure for Feishu v2 event callbacks.
type EventEnvelope struct {
	Schema    string          `json:"schema"`
	Header    EventHeader     `json:"header"`
	Event     json.RawMessage `json:"event"`
	Challenge string          `json:"challenge"`
	Type      string          `json:"type"`
}

type EventHeader struct {
	EventID    string `json:"event_id"`
	Token      string `json:"token"`
	CreateTime string `json:"create_time"`
	EventType  string `json:"event_type"`
	TenantKey  string `json:"tenant_key"`
	AppID      string `json:"app_id"`
}

type MessageEvent struct {
	Sender  Sender  `json:"sender"`
	Message MsgBody `json:"message"`
}

type Sender struct {
	SenderID   SenderID `json:"sender_id"`
	SenderType string   `json:"sender_type"`
	TenantKey  string   `json:"tenant_key"`
}

type SenderID struct {
	OpenID  string `json:"open_id"`
	UserID  string `json:"user_id"`
	UnionID string `json:"union_id"`
}

type MsgBody struct {
	MessageID   string    `json:"message_id"`
	ChatID      string    `json:"chat_id"`
	ChatType    string    `json:"chat_type"` // "p2p" or "group"
	Content     string    `json:"content"`   // JSON string, e.g. {"text":"hello"}
	MessageType string    `json:"message_type"`
	Mentions    []Mention `json:"mentions"`
}

type Mention struct {
	Key    string   `json:"key"`
	ID     SenderID `json:"id"`
	Name   string   `json:"name"`
	OpenID string   `json:"open_id,omitempty"`
}

// TextContent is the parsed content for text messages.
type TextContent struct {
	Text string `json:"text"`
}
