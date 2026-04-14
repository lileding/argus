package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/config"
	"argus/internal/model"
)

// MessageHandler is called to process an incoming message.
type MessageHandler func(chatID string, msg model.Message, messageID string)

// Transcriber can transcribe audio files to text.
type Transcriber interface {
	Transcribe(ctx context.Context, audioData []byte, filename string) (string, error)
}

// Handler handles Feishu webhook events.
type Handler struct {
	client       *Client
	transcriber  Transcriber
	dedup        *Dedup
	cfg          config.FeishuConfig
	workspaceDir string
	onMsg        MessageHandler
}

func NewHandler(client *Client, cfg config.FeishuConfig, workspaceDir string, transcriber Transcriber, onMsg MessageHandler) *Handler {
	filesDir := filepath.Join(workspaceDir, ".files")
	os.MkdirAll(filesDir, 0755)

	return &Handler{
		client:       client,
		transcriber:  transcriber,
		dedup:        NewDedup(5 * time.Minute),
		cfg:          cfg,
		workspaceDir: workspaceDir,
		onMsg:        onMsg,
	}
}

func (h *Handler) filesDir() string {
	return filepath.Join(h.workspaceDir, ".files")
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
	case "file":
		return h.buildFileMessage(event)
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

	filePath, dataURL, err := h.downloadAndSaveMedia(event.Message.MessageID, content.ImageKey, "image", ".png")
	if err != nil {
		slog.Warn("image download failed", "err", err)
		return model.NewTextMessage(model.RoleUser, "[User sent an image that could not be downloaded]"), nil
	}

	slog.Info("media saved", "type", "image", "path", filePath)
	return model.NewMultimodalMessage(model.RoleUser,
		fmt.Sprintf("The user sent an image (saved at %s). Describe or analyze it as needed.", filePath),
		dataURL,
	), nil
}

func (h *Handler) buildPostMessage(event MessageEvent) (model.Message, error) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(event.Message.Content), &raw); err != nil {
		return model.Message{}, fmt.Errorf("parse post content: %w", err)
	}

	text, imageKeys := extractPostContent(raw)

	if len(imageKeys) == 0 {
		if text == "" {
			text = "[Empty post message]"
		}
		return model.NewTextMessage(model.RoleUser, text), nil
	}

	var dataURLs []string
	var savedPaths []string
	for _, key := range imageKeys {
		filePath, dataURL, err := h.downloadAndSaveMedia(event.Message.MessageID, key, "image", ".png")
		if err != nil {
			slog.Warn("post image download failed", "image_key", key, "err", err)
			continue
		}
		dataURLs = append(dataURLs, dataURL)
		savedPaths = append(savedPaths, filePath)
	}

	if text == "" {
		text = "The user sent images. Describe or analyze them as needed."
	}
	if len(savedPaths) > 0 {
		text += fmt.Sprintf(" (images saved at: %s)", strings.Join(savedPaths, ", "))
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

	filePath, _, err := h.downloadAndSaveMedia(event.Message.MessageID, content.FileKey, "file", ".opus")
	if err != nil {
		slog.Warn("audio download failed", "err", err)
		return model.NewTextMessage(model.RoleUser, "[User sent a voice message that could not be downloaded]"), nil
	}

	slog.Info("media saved", "type", "audio", "path", filePath, "duration_ms", content.Duration)

	// Read the saved file for transcription.
	absPath := filepath.Join(h.workspaceDir, filePath)
	audioData, err2 := os.ReadFile(absPath)
	if err2 != nil {
		return model.NewTextMessage(model.RoleUser, "[Voice message saved but could not be read for transcription]"), nil
	}

	// Transcribe using the model's /v1/audio/transcriptions endpoint.
	transcript, err2 := h.transcriber.Transcribe(context.Background(), audioData, filepath.Base(absPath))
	if err2 != nil {
		slog.Warn("transcription failed", "err", err2)
		return model.NewTextMessage(model.RoleUser,
			fmt.Sprintf("[User sent a %d-second voice message (saved at %s) but transcription failed: %v]",
				content.Duration/1000, filePath, err2)), nil
	}

	slog.Info("audio transcribed", "text", transcript)

	return model.NewTextMessage(model.RoleUser,
		fmt.Sprintf("[Voice message, %ds, saved at %s]\n%s", content.Duration/1000, filePath, transcript)), nil
}

// downloadAndSaveMedia downloads a media file from Feishu, saves it to workspace/.files/,
// and returns the file path and base64 data URL.
func (h *Handler) buildFileMessage(event MessageEvent) (model.Message, error) {
	var content struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(event.Message.Content), &content); err != nil {
		return model.Message{}, fmt.Errorf("parse file content: %w", err)
	}

	// Download file.
	ext := filepath.Ext(content.FileName)
	if ext == "" {
		ext = ".bin"
	}
	filePath, _, err := h.downloadAndSaveMedia(event.Message.MessageID, content.FileKey, "file", ext)
	if err != nil {
		slog.Warn("file download failed", "err", err)
		return model.NewTextMessage(model.RoleUser,
			fmt.Sprintf("[User sent a file '%s' that could not be downloaded]", content.FileName)), nil
	}

	slog.Info("media saved", "type", "file", "path", filePath, "name", content.FileName)

	absPath := filepath.Join(h.workspaceDir, filePath)
	return model.NewTextMessage(model.RoleUser,
		fmt.Sprintf("The user sent a file '%s' (saved at %s, absolute path: %s). Read and process it as needed. For PDFs use `pdftotext '%s' -` via the cli tool.",
			content.FileName, filePath, absPath, absPath)), nil
}

func (h *Handler) downloadAndSaveMedia(messageID, fileKey, resourceType, ext string) (filePath, dataURL string, err error) {
	data, err := h.client.DownloadMessageResource(messageID, fileKey, resourceType)
	if err != nil {
		return "", "", err
	}

	if len(data) == 0 {
		return "", "", fmt.Errorf("empty response")
	}

	// Check for error JSON response.
	if len(data) < 1000 && data[0] == '{' {
		return "", "", fmt.Errorf("API error: %s", string(data))
	}

	// Save to workspace/.files/
	filename := fileKey + ext
	filePath = filepath.Join(h.filesDir(), filename)
	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", "", fmt.Errorf("save file: %w", err)
	}

	// Build data URL.
	contentType := http.DetectContentType(data)
	dataURL = fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data))

	slog.Info("media downloaded", "size", len(data), "path", filePath)

	// Return path relative to workspace for the model to reference.
	relPath, _ := filepath.Rel(h.workspaceDir, filePath)
	return relPath, dataURL, nil
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
