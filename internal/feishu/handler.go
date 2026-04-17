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

	"argus/internal/config"
	"argus/internal/model"
	"argus/internal/store"
)

// FilterNotifier is used by Handler to notify the Filter that a new message
// is available for processing.
type FilterNotifier interface {
	Notify(chatID string)
}

// filterChanNotifier wraps a channel as a FilterNotifier.
type filterChanNotifier struct {
	ch chan<- string
}

func NewFilterChanNotifier(ch chan<- string) FilterNotifier {
	return &filterChanNotifier{ch: ch}
}

func (n *filterChanNotifier) Notify(chatID string) {
	select {
	case n.ch <- chatID:
	default:
	}
}

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

// Handler handles Feishu webhook events. Inbound: parse + store to DB + notify
// Filter. All media processing is done by the Filter, not the Handler.
type Handler struct {
	client       *Client
	store        store.QueueStore
	dedup        *Dedup
	cfg          config.FeishuConfig
	workspaceDir string
	filterNotify FilterNotifier
}

func NewHandler(client *Client, cfg config.FeishuConfig, workspaceDir string, st store.QueueStore, filterNotify FilterNotifier) *Handler {
	filesDir := filepath.Join(workspaceDir, ".files")
	os.MkdirAll(filesDir, 0755)

	return &Handler{
		client:       client,
		store:        st,
		dedup:        NewDedup(5 * time.Minute),
		cfg:          cfg,
		workspaceDir: workspaceDir,
		filterNotify: filterNotify,
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

// processEvent is the inbound path: parse envelope → store raw message → notify Filter.
// Fully reentrant — no per-chat locking, no media download, no agent call.
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

	// Store raw message immediately (QoS=1: persist before anything else).
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
		"msg_id", msg.ID,
		"chat_id", chatID,
		"chat_type", msgEvent.Message.ChatType,
		"msg_type", msgEvent.Message.MessageType,
	)

	// Notify Filter that there's work.
	h.filterNotify.Notify(chatID)
}

// The build*Message, downloadAndSaveMedia, and correctTranscription methods
// have moved to filter.go (FeishuFilter). Handler no longer does media
// processing — it stores the raw message and lets Filter handle the rest.

// extractPostContent and deriveChatID are shared helpers used by both
// handler and filter.

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
