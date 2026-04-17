package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// MessageFilter processes raw messages (status=received) into agent-ready
// form (status=ready). This is the middle stage of the pipeline:
// Handler (inbound) → Filter → Dispatcher (agent).
type MessageFilter interface {
	// Process takes a received message, downloads media, transcribes audio,
	// updates the message content, sends the thinking card (ACK), and
	// transitions the message to 'ready'. emit is called to push IM events
	// (e.g. thinking card) back through the Handler's outbound path.
	Process(ctx context.Context, msg *store.StoredMessage) error
}

// FeishuFilter implements MessageFilter for Feishu messages.
type FeishuFilter struct {
	client       *Client
	transcriber  Transcriber
	corrector    Corrector
	docStore     DocRegisterer
	store        store.QueueStore
	workspaceDir string

	dispatchNotify chan<- string // notify Dispatcher when a message is ready
	quit           chan struct{}
	wg             sync.WaitGroup
}

func NewFeishuFilter(
	client *Client,
	transcriber Transcriber,
	corrector Corrector,
	docStore DocRegisterer,
	st store.QueueStore,
	workspaceDir string,
	dispatchNotify chan<- string,
) *FeishuFilter {
	filesDir := filepath.Join(workspaceDir, ".files")
	os.MkdirAll(filesDir, 0755)
	return &FeishuFilter{
		client:         client,
		transcriber:    transcriber,
		corrector:      corrector,
		docStore:       docStore,
		store:          st,
		workspaceDir:   workspaceDir,
		dispatchNotify: dispatchNotify,
		quit:           make(chan struct{}),
	}
}

// StartWorker launches a goroutine that listens for filter notifications
// and periodically scans for received messages.
func (f *FeishuFilter) StartWorker(notify <-chan string) {
	f.wg.Add(1)
	go f.run(notify)
	slog.Info("filter worker started")
}

func (f *FeishuFilter) Stop() {
	close(f.quit)
	f.wg.Wait()
	slog.Info("filter worker stopped")
}

func (f *FeishuFilter) run(notify <-chan string) {
	defer f.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-notify:
			f.processAll()
		case <-ticker.C:
			f.processAll()
		case <-f.quit:
			return
		}
	}
}

// processAll scans for all 'received' messages and processes them.
// Uses RecoverQueue which also returns received rows as a side effect.
func (f *FeishuFilter) processAll() {
	ctx := context.Background()
	_, unacked, err := f.store.RecoverQueue(ctx)
	if err != nil {
		slog.Warn("filter: query received", "err", err)
		return
	}
	for i := range unacked {
		if err := f.Process(ctx, &unacked[i]); err != nil {
			slog.Error("filter: process failed", "msg_id", unacked[i].ID, "err", err)
		}
	}
}

func (f *FeishuFilter) Process(ctx context.Context, msg *store.StoredMessage) error {
	slog.Info("filter: processing",
		"msg_id", msg.ID, "chat_id", msg.ChatID,
		"msg_type", msg.MsgType, "content_len", len(msg.Content),
	)

	// Mark as filtering.
	if err := f.store.SetReplyStatus(ctx, msg.ID, "filtering"); err != nil {
		return fmt.Errorf("set filtering: %w", err)
	}

	// Build the processed text content from the raw Feishu content JSON.
	processedText, err := f.buildProcessedContent(ctx, msg)
	if err != nil {
		slog.Warn("filter: build content failed", "msg_id", msg.ID, "err", err)
		processedText = fmt.Sprintf("[Message processing failed: %v]", err)
	}

	// Update content in the database.
	if err := f.store.UpdateMessageContent(ctx, msg.ID, processedText); err != nil {
		return fmt.Errorf("update content: %w", err)
	}

	// ACK: send thinking card (visual confirmation to user).
	lang := quickDetectLang(processedText)
	replyChannelID := ""
	if msg.TriggerMsgID != "" {
		cardJSON := ThinkingCard(lang)
		if id, err := f.client.ReplyRichWithID(msg.TriggerMsgID, "interactive", cardJSON); err != nil {
			slog.Warn("filter: send thinking card", "msg_id", msg.ID, "err", err)
		} else {
			replyChannelID = id
		}
	}

	// Transition to ready + store reply channel ID.
	if err := f.store.AckReply(ctx, msg.ID, replyChannelID); err != nil {
		return fmt.Errorf("ack reply: %w", err)
	}

	slog.Info("filter: message ready",
		"msg_id", msg.ID, "chat_id", msg.ChatID,
		"reply_channel_id", replyChannelID,
	)

	// Notify Dispatcher.
	select {
	case f.dispatchNotify <- msg.ChatID:
	default:
	}

	return nil
}

// buildProcessedContent converts raw Feishu content JSON into agent-ready text.
// This is the logic formerly in handler.buildMessage, but now operates on
// stored raw content rather than the live event.
func (f *FeishuFilter) buildProcessedContent(ctx context.Context, msg *store.StoredMessage) (string, error) {
	rawContent := msg.Content

	switch msg.MsgType {
	case "text":
		var content TextContent
		if err := json.Unmarshal([]byte(rawContent), &content); err != nil {
			return rawContent, nil // fallback: use raw content as-is
		}
		return content.Text, nil

	case "image":
		var content struct {
			ImageKey string `json:"image_key"`
		}
		if err := json.Unmarshal([]byte(rawContent), &content); err != nil {
			return "[User sent an image]", nil
		}
		filePath, err := f.downloadMedia(msg.TriggerMsgID, content.ImageKey, "image", ".png")
		if err != nil {
			return "[User sent an image that could not be downloaded]", nil
		}
		return fmt.Sprintf("The user sent an image (saved at %s). Describe or analyze it as needed.", filePath), nil

	case "audio":
		var content struct {
			FileKey  string `json:"file_key"`
			Duration int    `json:"duration"`
		}
		if err := json.Unmarshal([]byte(rawContent), &content); err != nil {
			return "[User sent a voice message]", nil
		}
		filePath, err := f.downloadMedia(msg.TriggerMsgID, content.FileKey, "file", ".opus")
		if err != nil {
			return "[Voice message could not be downloaded]", nil
		}

		slog.Info("media saved", "type", "audio", "path", filePath, "duration_ms", content.Duration)

		// Transcribe.
		absPath := filepath.Join(f.workspaceDir, filePath)
		audioData, err := os.ReadFile(absPath)
		if err != nil {
			return fmt.Sprintf("[Voice message saved at %s but could not be read]", filePath), nil
		}

		result, err := f.transcriber.Transcribe(ctx, audioData, filepath.Base(absPath))
		if err != nil {
			return fmt.Sprintf("[Voice message, %ds, saved at %s, transcription failed: %v]",
				content.Duration/1000, filePath, err), nil
		}

		transcript := result.Text
		slog.Info("audio transcribed", "text", transcript, "confidence", result.Confidence)

		// LLM correction.
		if f.corrector != nil {
			corrected := f.correctTranscription(ctx, transcript)
			if corrected != "" {
				slog.Info("transcription corrected", "original", transcript, "corrected", corrected)
				transcript = corrected
			}
		}

		return fmt.Sprintf("[Voice message, %ds, saved at %s]\n%s",
			content.Duration/1000, filePath, transcript), nil

	case "post":
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(rawContent), &raw); err != nil {
			return rawContent, nil
		}
		text, imageKeys := extractPostContent(raw)

		var savedPaths []string
		for _, key := range imageKeys {
			filePath, err := f.downloadMedia(msg.TriggerMsgID, key, "image", ".png")
			if err != nil {
				continue
			}
			savedPaths = append(savedPaths, filePath)
		}

		if text == "" {
			text = "The user sent images."
		}
		if len(savedPaths) > 0 {
			text += fmt.Sprintf(" (images saved at: %s)", strings.Join(savedPaths, ", "))
		}
		return text, nil

	case "file":
		var content struct {
			FileKey  string `json:"file_key"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(rawContent), &content); err != nil {
			return "[User sent a file]", nil
		}
		ext := filepath.Ext(content.FileName)
		if ext == "" {
			ext = ".bin"
		}
		filePath, err := f.downloadMedia(msg.TriggerMsgID, content.FileKey, "file", ext)
		if err != nil {
			return fmt.Sprintf("[File '%s' could not be downloaded]", content.FileName), nil
		}

		absPath := filepath.Join(f.workspaceDir, filePath)

		// Register for RAG indexing.
		if f.docStore != nil {
			f.docStore.SaveDocument(ctx, &store.Document{
				Filename: content.FileName,
				FilePath: absPath,
				Status:   "pending",
			})
		}

		return fmt.Sprintf("The user sent a file '%s' (saved at %s, absolute path: %s). "+
			"Read and process it as needed.", content.FileName, filePath, absPath), nil

	default:
		return rawContent, nil
	}
}

func (f *FeishuFilter) downloadMedia(messageID, fileKey, resourceType, ext string) (string, error) {
	data, err := f.client.DownloadMessageResource(messageID, fileKey, resourceType)
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
	fullPath := filepath.Join(f.workspaceDir, ".files", filename)
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", fmt.Errorf("save file: %w", err)
	}

	slog.Info("media downloaded", "size", len(data), "path", fullPath)

	relPath, _ := filepath.Rel(f.workspaceDir, fullPath)
	return relPath, nil
}

func (f *FeishuFilter) correctTranscription(ctx context.Context, raw string) string {
	messages := []model.Message{
		{Role: model.RoleSystem, Content: "You are a transcription post-processor. Your task:\n" +
			"1. Add proper punctuation (commas, periods, question marks, etc.)\n" +
			"2. Fix misheard words, especially technical terms and proper nouns\n" +
			"3. The speaker uses mixed Chinese and English\n" +
			"4. Common domains: technology (API, Kubernetes, Docker, GPU, LLM, MLX), " +
			"finance (ETF, hedge fund, derivatives), arts (sonata, Chopin, Scriabin, Prokofiev)\n" +
			"5. Return ONLY the corrected text with punctuation. No explanations, no quotes."},
		{Role: model.RoleUser, Content: raw},
	}
	resp, err := f.corrector.Chat(ctx, messages, nil)
	if err != nil {
		slog.Warn("transcription correction failed", "err", err)
		return ""
	}
	return resp.Content
}

// Needed for data URL generation (unused in filter but kept for future multimodal support).
var _ = base64.StdEncoding
var _ = http.DetectContentType
