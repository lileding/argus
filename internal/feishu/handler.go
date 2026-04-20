package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/agent"
	"argus/internal/config"
	"argus/internal/model"
	"argus/internal/store"
)

// Transcriber can transcribe audio files to text.
type Transcriber interface {
	Transcribe(ctx context.Context, audioData []byte, filename string) (*model.TranscriptionResult, error)
}

// Corrector can correct transcription errors using an LLM.
type Corrector interface {
	Chat(ctx context.Context, messages []model.Message, tools []model.ToolDef) (*model.Response, error)
}

// DocRegisterer can register documents for RAG indexing.
type DocRegisterer interface {
	SaveDocument(ctx context.Context, doc *store.Document) error
}

// Handler handles Feishu webhook events.
//
// Inbound: parse → INSERT raw message → construct Task → agent.SubmitTask
// → spawn async media processing goroutine.
//
// The media goroutine downloads/transcribes, updates DB, and sends the
// processed Payload through the task's ReadyCh. The Agent scheduler
// opens the thinking card (via Frontend.SubmitMessage) immediately on
// task pop, then blocks on ReadyCh until content is ready.
type Handler struct {
	client       *Client
	store        store.QueueStore
	ag           *agent.Agent
	frontend     *FeishuFrontend
	transcriber  Transcriber
	corrector    Corrector
	docStore     DocRegisterer
	dedup        *Dedup
	cfg          config.FeishuConfig
	workspaceDir string
}

func NewHandler(
	client *Client,
	cfg config.FeishuConfig,
	workspaceDir string,
	st store.QueueStore,
	ag *agent.Agent,
	frontend *FeishuFrontend,
	transcriber Transcriber,
	corrector Corrector,
	docStore DocRegisterer,
) *Handler {
	filesDir := filepath.Join(workspaceDir, ".files")
	os.MkdirAll(filesDir, 0755)

	return &Handler{
		client:       client,
		store:        st,
		ag:           ag,
		frontend:     frontend,
		transcriber:  transcriber,
		corrector:    corrector,
		docStore:     docStore,
		dedup:        NewDedup(5 * time.Minute),
		cfg:          cfg,
		workspaceDir: workspaceDir,
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

// processEvent: parse → store raw → push to chat channel → spawn media goroutine.
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

	// 1. Store raw message immediately (notReady).
	msg := &store.StoredMessage{
		ChatID:       chatID,
		Role:         "user",
		Content:      msgEvent.Message.Content, // raw Feishu JSON
		SourceIM:     "feishu",
		Channel:      chatID,
		MsgType:      msgEvent.Message.MessageType,
		SenderID:     msgEvent.Sender.SenderID.OpenID,
		TriggerMsgID: msgEvent.Message.MessageID,
	}
	if err := h.store.SaveMessageQueued(context.Background(), msg); err != nil {
		slog.Error("handler: save message", "err", err)
		return
	}

	slog.Info("message queued",
		"msg_id", msg.ID, "chat_id", chatID,
		"msg_type", msgEvent.Message.MessageType,
	)

	// 2. Construct Task and submit to Agent.
	readyCh := make(chan agent.Payload, 1)
	h.ag.SubmitTask(agent.Task{
		ChatID:       chatID,
		MsgID:        msg.ID,
		TriggerMsgID: msgEvent.Message.MessageID,
		Lang:         quickDetectLang(msgEvent.Message.Content),
		Frontend:     h.frontend,
		ReadyCh:      readyCh,
	})

	// 3. Spawn async media processing goroutine.
	go h.ProcessMedia(msg, readyCh)
}

// ProcessMedia downloads media, transcribes audio, updates DB content to
// the processed form, and sends the Payload through readyCh.
// DB updates are retained for crash recovery / replay.
func (h *Handler) ProcessMedia(msg *store.StoredMessage, readyCh chan agent.Payload) {
	ctx := context.Background()

	processedText, filePaths, err := h.buildProcessedContent(ctx, msg)
	if err != nil {
		slog.Warn("processMedia: build content failed", "msg_id", msg.ID, "err", err)
		processedText = fmt.Sprintf("[Message processing failed: %v]", err)
	}

	// Persist to DB for replay.
	if err := h.store.UpdateMessageContent(ctx, msg.ID, processedText); err != nil {
		slog.Error("processMedia: update content", "msg_id", msg.ID, "err", err)
	}
	if len(filePaths) > 0 {
		if err := h.store.UpdateMessageFilePaths(ctx, msg.ID, filePaths); err != nil {
			slog.Error("processMedia: update file_paths", "msg_id", msg.ID, "err", err)
		}
	}
	if err := h.store.SetReplyStatus(ctx, msg.ID, "ready"); err != nil {
		slog.Error("processMedia: set ready", "msg_id", msg.ID, "err", err)
	}

	// Deliver payload to Agent — no DB round-trip needed.
	readyCh <- agent.Payload{Content: processedText, FilePaths: filePaths}

	slog.Info("processMedia: ready", "msg_id", msg.ID, "msg_type", msg.MsgType, "files", len(filePaths))
}

// buildProcessedContent converts raw Feishu content JSON into agent-ready text
// and returns any image file paths for multimodal message construction.
func (h *Handler) buildProcessedContent(ctx context.Context, msg *store.StoredMessage) (string, []string, error) {
	raw := msg.Content

	switch msg.MsgType {
	case "text":
		var c TextContent
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return raw, nil, nil
		}
		return c.Text, nil, nil

	case "image":
		var c struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return "[User sent an image]", nil, nil
		}
		filePath, err := h.downloadMedia(msg.TriggerMsgID, c.ImageKey, "image", ".png")
		if err != nil {
			return "[User sent an image that could not be downloaded]", nil, nil
		}
		return "The user sent an image.", []string{filePath}, nil

	case "audio":
		var c struct {
			FileKey  string `json:"file_key"`
			Duration int    `json:"duration"`
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return "[User sent a voice message]", nil, nil
		}
		filePath, err := h.downloadMedia(msg.TriggerMsgID, c.FileKey, "file", ".opus")
		if err != nil {
			return "[Voice message could not be downloaded]", nil, nil
		}
		slog.Info("media saved", "type", "audio", "path", filePath, "duration_ms", c.Duration)

		absPath := filepath.Join(h.workspaceDir, filePath)
		audioData, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Sprintf("[Voice message saved at %s but could not be read]", filePath), nil, nil
		}

		result, err := h.transcriber.Transcribe(ctx, audioData, filepath.Base(absPath))
		if err != nil {
			return fmt.Sprintf("[Voice message, %ds, transcription failed: %v]", c.Duration/1000, err), nil, nil
		}
		transcript := result.Text
		slog.Info("audio transcribed", "text", transcript, "confidence", result.Confidence)

		if h.corrector != nil && result.Confidence < -0.15 {
			if corrected := h.correctTranscription(ctx, transcript); corrected != "" {
				slog.Info("transcription corrected", "original", transcript, "corrected", corrected)
				transcript = corrected
			}
		} else if h.corrector != nil {
			slog.Info("transcription confident, skipping LLM correction",
				"confidence", result.Confidence)
		}
		return fmt.Sprintf("[Voice message, %ds, saved at %s]\n%s", c.Duration/1000, filePath, transcript), nil, nil

	case "post":
		var rawMsg json.RawMessage
		if err := json.Unmarshal([]byte(raw), &rawMsg); err != nil {
			return raw, nil, nil
		}
		text, imageKeys := extractPostContent(rawMsg)
		var savedPaths []string
		for _, key := range imageKeys {
			if fp, err := h.downloadMedia(msg.TriggerMsgID, key, "image", ".png"); err == nil {
				savedPaths = append(savedPaths, fp)
			}
		}
		if text == "" {
			text = "The user sent images."
		}
		return text, savedPaths, nil

	case "file":
		var c struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return "[User sent a file]", nil, nil
		}
		ext := filepath.Ext(c.FileName)
		if ext == "" {
			ext = ".bin"
		}
		filePath, err := h.downloadMedia(msg.TriggerMsgID, c.FileKey, "file", ext)
		if err != nil {
			return fmt.Sprintf("[File '%s' could not be downloaded]", c.FileName), nil, nil
		}
		absPath := filepath.Join(h.workspaceDir, filePath)
		if h.docStore != nil {
			h.docStore.SaveDocument(ctx, &store.Document{
				Filename: c.FileName, FilePath: absPath, Status: "pending",
			})
		}
		return fmt.Sprintf("The user sent a file '%s' (saved at %s, absolute path: %s).",
			c.FileName, filePath, absPath), nil, nil

	default:
		return raw, nil, nil
	}
}

func (h *Handler) downloadMedia(messageID, fileKey, resourceType, ext string) (string, error) {
	data, err := h.client.DownloadMessageResource(messageID, fileKey, resourceType)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("empty response")
	}
	if len(data) < 1000 && data[0] == '{' {
		return "", fmt.Errorf("API error: %s", string(data))
	}
	filename := fileKey + ext
	fullPath := filepath.Join(h.workspaceDir, ".files", filename)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", fmt.Errorf("save file: %w", err)
	}
	slog.Info("media downloaded", "size", len(data), "path", fullPath)
	relPath, _ := filepath.Rel(h.workspaceDir, fullPath)
	return relPath, nil
}

func (h *Handler) correctTranscription(ctx context.Context, raw string) string {
	messages := []model.Message{
		{Role: model.RoleSystem, Content: "You are a transcription post-processor. Your task:\n" +
			"1. Add proper punctuation\n2. Fix misheard words, especially technical terms and proper nouns\n" +
			"3. The speaker uses mixed Chinese and English\n" +
			"4. Return ONLY the corrected text. No explanations."},
		{Role: model.RoleUser, Content: raw},
	}
	resp, err := h.corrector.Chat(ctx, messages, nil)
	if err != nil {
		return ""
	}
	return resp.Content
}

// --- Shared helpers ---

func extractPostContent(raw json.RawMessage) (text string, imageKeys []string) {
	var direct struct {
		Title   string          `json:"title"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil && direct.Content != nil {
		t, imgs := parsePostContentArray(direct.Content)
		return joinText(direct.Title, t), imgs
	}
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

func parsePostContentArray(raw json.RawMessage) (string, []string) {
	var lines [][]struct {
		Tag      string `json:"tag"`
		Text     string `json:"text"`
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return "", nil
	}
	var texts []string
	var imageKeys []string
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

// detectLang checks if the text is primarily Chinese.
func detectLang(text string) string {
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	return "en"
}

// quickDetectLang does a fast language detection on raw Feishu message
// content JSON. Falls back to "zh" if undetermined.
func quickDetectLang(rawContentJSON string) string {
	for _, r := range rawContentJSON {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	return "zh"
}

func deriveChatID(chatType, userOpenID, feishuChatID string) string {
	if chatType == "p2p" {
		return fmt.Sprintf("p2p:%s", userOpenID)
	}
	return fmt.Sprintf("group:%s", feishuChatID)
}
