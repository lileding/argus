package feishu

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"argus/internal/config"
	"argus/internal/model"
)

// MessageHandler is called to process an incoming message.
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

	if envelope.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": envelope.Challenge})
		return
	}

	eventID := envelope.Header.EventID
	if eventID != "" && h.dedup.IsDuplicate(eventID) {
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
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

	chatID := deriveChatID(msgEvent.Message.ChatType, msgEvent.Sender.SenderID.OpenID, msgEvent.Message.ChatID)

	if msgEvent.Message.ChatType == "group" && len(msgEvent.Message.Mentions) == 0 {
		return
	}

	msg, err := h.buildMessage(msgEvent)
	if err != nil {
		slog.Warn("unsupported message", "type", msgEvent.Message.MessageType, "err", err)
		return
	}

	slog.Info("message received",
		"chat_id", chatID,
		"chat_type", msgEvent.Message.ChatType,
		"msg_type", msgEvent.Message.MessageType,
		"text", msg.TextContent(),
	)

	h.onMsg(chatID, msg, msgEvent.Message.MessageID)
}

func (h *Handler) buildMessage(event MessageEvent) (model.Message, error) {
	switch event.Message.MessageType {
	case "text":
		return h.buildTextMessage(event)
	case "image":
		return h.buildImageMessage(event)
	case "post":
		return h.buildPostMessage(event)
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

	dataURL, err := h.downloadImageAsDataURL(event.Message.MessageID, content.ImageKey)
	if err != nil {
		slog.Warn("image download failed, sending as text", "err", err)
		return model.NewTextMessage(model.RoleUser, "[User sent an image that could not be downloaded]"), nil
	}

	return model.NewMultimodalMessage(model.RoleUser, "The user sent this image. Describe or analyze it as needed.", dataURL), nil
}

// buildPostMessage handles Feishu rich text "post" messages (text + images combined).
func (h *Handler) buildPostMessage(event MessageEvent) (model.Message, error) {
	// Feishu post content: {"title":"...", "content":[[{"tag":"text","text":"..."},{"tag":"img","image_key":"..."}]]}
	// Or with language key: {"zh_cn": {"title":"...", "content":[...]}}
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(event.Message.Content), &raw); err != nil {
		return model.Message{}, fmt.Errorf("parse post content: %w", err)
	}

	text, imageKeys := extractPostContent(raw)

	if len(imageKeys) == 0 {
		// Text-only post.
		if text == "" {
			text = "[Empty post message]"
		}
		return model.NewTextMessage(model.RoleUser, text), nil
	}

	// Download images and build multimodal message.
	var dataURLs []string
	for _, key := range imageKeys {
		dataURL, err := h.downloadImageAsDataURL(event.Message.MessageID, key)
		if err != nil {
			slog.Warn("post image download failed", "image_key", key, "err", err)
			continue
		}
		dataURLs = append(dataURLs, dataURL)
	}

	if text == "" {
		text = "The user sent this image. Describe or analyze it as needed."
	}

	if len(dataURLs) == 0 {
		return model.NewTextMessage(model.RoleUser, text), nil
	}

	return model.NewMultimodalMessage(model.RoleUser, text, dataURLs...), nil
}

func (h *Handler) buildAudioMessage(event MessageEvent) (model.Message, error) {
	var content struct {
		FileKey  string `json:"file_key"`
		Duration int    `json:"duration"`
	}
	if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
		return model.Message{}, fmt.Errorf("parse audio content: %w", err)
	}

	audioData, err := h.client.DownloadMessageResource(event.Message.MessageID, content.FileKey, "audio")
	if err != nil {
		return model.NewTextMessage(model.RoleUser, "[User sent a voice message that could not be downloaded]"), nil
	}

	slog.Info("audio downloaded", "size", len(audioData))
	dataURL := "data:audio/opus;base64," + base64.StdEncoding.EncodeToString(audioData)

	parts := []model.ContentPart{
		{Type: "text", Text: "The user sent a voice message. Transcribe and respond to it."},
		{Type: "input_audio", Text: dataURL},
	}

	return model.Message{Role: model.RoleUser, Content: parts}, nil
}

// downloadImageAsDataURL downloads an image from Feishu and returns a base64 data URL.
// Uses the message resource API which requires message_id + image_key.
func (h *Handler) downloadImageAsDataURL(messageID, imageKey string) (string, error) {
	imageData, err := h.client.DownloadMessageResource(messageID, imageKey, "image")
	if err != nil {
		return "", err
	}

	if len(imageData) == 0 {
		return "", fmt.Errorf("empty image data")
	}

	// Check if the response is an error JSON instead of image bytes.
	if len(imageData) < 1000 && imageData[0] == '{' {
		return "", fmt.Errorf("image download returned error: %s", string(imageData))
	}

	slog.Info("image downloaded", "size", len(imageData), "image_key", imageKey)

	// Detect content type from magic bytes.
	contentType := http.DetectContentType(imageData)
	dataURL := fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(imageData))

	return dataURL, nil
}

// extractPostContent extracts text and image keys from a Feishu post message.
func extractPostContent(raw json.RawMessage) (text string, imageKeys []string) {
	// Try direct format: {"title":"...", "content":[...]}
	var direct struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil && direct.Content != nil {
		t, imgs := parsePostContentArray(direct.Content)
		return joinText(direct.Title, t), imgs
	}

	// Try language-keyed format: {"zh_cn": {"title":"...", "content":[...]}}
	var langKeyed map[string]struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &langKeyed); err == nil {
		for _, body := range langKeyed {
			t, imgs := parsePostContentArray(body.Content)
			return joinText(body.Title, t), imgs
		}
	}

	return "", nil
}

// parsePostContentArray parses [[{"tag":"text","text":"..."},{"tag":"img","image_key":"..."}]]
func parsePostContentArray(raw json.RawMessage) (text string, imageKeys []string) {
	var lines [][]struct {
		Tag      string `json:"tag"`
		Text     string `json:"text"`
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return "", nil
	}

	var texts []string
	for _, line := range lines {
		var lineTexts []string
		for _, elem := range line {
			switch elem.Tag {
			case "text":
				if elem.Text != "" {
					lineTexts = append(lineTexts, elem.Text)
				}
			case "img":
				if elem.ImageKey != "" {
					imageKeys = append(imageKeys, elem.ImageKey)
				}
			}
		}
		if len(lineTexts) > 0 {
			texts = append(texts, strings.Join(lineTexts, ""))
		}
	}

	return strings.Join(texts, "\n"), imageKeys
}

func joinText(title, body string) string {
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title != "" && body != "" {
		return title + "\n" + body
	}
	if title != "" {
		return title
	}
	return body
}

func deriveChatID(chatType, userOpenID, feishuChatID string) string {
	if chatType == "p2p" {
		return fmt.Sprintf("p2p:%s", userOpenID)
	}
	return fmt.Sprintf("group:%s", feishuChatID)
}
