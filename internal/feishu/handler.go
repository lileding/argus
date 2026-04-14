package feishu

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"argus/internal/config"
	"argus/internal/model"
)

// MessageHandler is called to process an incoming message.
// chatID identifies the conversation, msg is the user message (may be multimodal),
// messageID is used for replies.
type MessageHandler func(chatID string, msg model.Message, messageID string)

// Handler handles Feishu webhook events.
type Handler struct {
	client *Client
	dedup  *Dedup
	cfg    config.FeishuConfig
	onMsg  MessageHandler
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

	slog.Debug("webhook received", "body", string(body))

	var envelope EventEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		slog.Error("parse envelope", "err", err, "body", string(body))
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

	// Derive chat_id.
	chatID := deriveChatID(msgEvent.Message.ChatType, msgEvent.Sender.SenderID.OpenID, msgEvent.Message.ChatID)

	// In group chats, only respond to @mentions.
	if msgEvent.Message.ChatType == "group" && len(msgEvent.Message.Mentions) == 0 {
		return
	}

	// Build model message based on message type.
	msg, err := h.buildMessage(msgEvent)
	if err != nil {
		slog.Warn("unsupported message", "type", msgEvent.Message.MessageType, "err", err)
		return
	}

	slog.Info("message received",
		"chat_id", chatID,
		"chat_type", msgEvent.Message.ChatType,
		"msg_type", msgEvent.Message.MessageType,
	)

	h.onMsg(chatID, msg, msgEvent.Message.MessageID)
}

// buildMessage converts a Feishu message event into a model.Message.
func (h *Handler) buildMessage(event MessageEvent) (model.Message, error) {
	switch event.Message.MessageType {
	case "text":
		return h.buildTextMessage(event)
	case "image":
		return h.buildImageMessage(event)
	case "audio":
		return h.buildAudioMessage(event)
	default:
		return model.Message{}, fmt.Errorf("unsupported message type: %s", event.Message.MessageType)
	}
}

func (h *Handler) buildTextMessage(event MessageEvent) (model.Message, error) {
	var content TextContent
	if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
		return model.Message{}, fmt.Errorf("parse text content: %w", err)
	}
	return model.NewTextMessage(model.RoleUser, content.Text), nil
}

func (h *Handler) buildImageMessage(event MessageEvent) (model.Message, error) {
	var content struct {
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
		return model.Message{}, fmt.Errorf("parse image content: %w", err)
	}

	// Download image from Feishu.
	imageData, err := h.client.DownloadImage(content.ImageKey)
	if err != nil {
		return model.Message{}, fmt.Errorf("download image: %w", err)
	}

	// Convert to data URL for OpenAI vision format.
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData)

	return model.NewMultimodalMessage(model.RoleUser, "The user sent this image. Describe or analyze it as needed.", dataURL), nil
}

func (h *Handler) buildAudioMessage(event MessageEvent) (model.Message, error) {
	var content struct {
		FileKey  string `json:"file_key"`
		Duration int    `json:"duration"` // milliseconds
	}
	if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
		return model.Message{}, fmt.Errorf("parse audio content: %w", err)
	}

	// Download audio from Feishu.
	audioData, err := h.client.DownloadMessageResource(event.Message.MessageID, content.FileKey, "audio")
	if err != nil {
		return model.Message{}, fmt.Errorf("download audio: %w", err)
	}

	// Convert to base64 data URL. Feishu audio is typically opus format.
	dataURL := "data:audio/opus;base64," + base64.StdEncoding.EncodeToString(audioData)

	// Build multimodal message with audio.
	parts := []model.ContentPart{
		{Type: "text", Text: "The user sent a voice message. Transcribe and respond to it."},
		{Type: "input_audio", Text: dataURL},
	}

	return model.Message{Role: model.RoleUser, Content: parts}, nil
}

func deriveChatID(chatType, userOpenID, feishuChatID string) string {
	if chatType == "p2p" {
		return fmt.Sprintf("p2p:%s", userOpenID)
	}
	return fmt.Sprintf("group:%s", feishuChatID)
}
