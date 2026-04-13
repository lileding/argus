package feishu

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"argus/internal/config"
)

// MessageHandler is called to process an incoming message.
// chatID is the derived chat identifier, text is the message content, messageID is used for replies.
type MessageHandler func(chatID, text, messageID string)

// Handler handles Feishu webhook events.
type Handler struct {
	client  *Client
	dedup   *Dedup
	cfg     config.FeishuConfig
	onMsg   MessageHandler
}

func NewHandler(client *Client, cfg config.FeishuConfig, onMsg MessageHandler) *Handler {
	return &Handler{
		client: client,
		dedup:  NewDedup(5 * time.Minute),
		cfg:    cfg,
		onMsg:  onMsg,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("read body", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var envelope EventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		slog.Error("parse envelope", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// URL verification challenge.
	if envelope.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}

	// Dedup by event_id.
	eventID := envelope.Header.EventID
	if eventID != "" && h.dedup.IsDuplicate(eventID) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Respond 200 immediately — Feishu has a 3-second timeout.
	w.WriteHeader(http.StatusOK)

	// Process asynchronously.
	go h.processEvent(envelope)
}

func (h *Handler) processEvent(envelope EventEnvelope) {
	if envelope.Header.EventType != "im.message.receive_v1" {
		return
	}

	var msgEvent MessageEvent
	if err := json.Unmarshal(envelope.Event, &msgEvent); err != nil {
		slog.Error("parse message event", "err", err)
		return
	}

	// Only handle text messages for now.
	if msgEvent.Message.MessageType != "text" {
		return
	}

	var content TextContent
	if err := json.Unmarshal([]byte(msgEvent.Message.Content), &content); err != nil {
		slog.Error("parse message content", "err", err)
		return
	}

	// Derive chat_id.
	chatID := deriveChatID(msgEvent.Message.ChatType, msgEvent.Sender.SenderID.OpenID, msgEvent.Message.ChatID)

	// In group chats, only respond to @mentions (check if mentions exist).
	if msgEvent.Message.ChatType == "group" && len(msgEvent.Message.Mentions) == 0 {
		return
	}

	slog.Info("message received",
		"chat_id", chatID,
		"chat_type", msgEvent.Message.ChatType,
		"text", content.Text,
	)

	h.onMsg(chatID, content.Text, msgEvent.Message.MessageID)
}

func deriveChatID(chatType, userOpenID, feishuChatID string) string {
	if chatType == "p2p" {
		return fmt.Sprintf("p2p:%s", userOpenID)
	}
	return fmt.Sprintf("group:%s", feishuChatID)
}
