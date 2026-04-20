package embedding

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// Worker runs a background loop that embeds unembedded messages/chunks
// and generates summaries for long assistant replies.
type Worker struct {
	client       *Client
	semantic     store.SemanticStore
	pinned       store.PinnedMemoryStore
	docs         store.DocumentStore
	summarizer   model.Client // cheap/fast model for summary generation (may be nil)
	batchSize    int
	interval     time.Duration
	notify       chan struct{}
	quit         chan struct{}
	wg           sync.WaitGroup
}

func NewWorker(client *Client, semantic store.SemanticStore, pinned store.PinnedMemoryStore, docs store.DocumentStore, batchSize int, interval time.Duration) *Worker {
	if batchSize == 0 {
		batchSize = 32
	}
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &Worker{
		client:    client,
		semantic:  semantic,
		pinned:    pinned,
		docs:      docs,
		batchSize: batchSize,
		interval:  interval,
		notify:    make(chan struct{}, 1),
		quit:      make(chan struct{}),
	}
}

func (w *Worker) Start() {
	w.wg.Add(1)
	go w.run()
	slog.Info("embedding worker started", "batch_size", w.batchSize, "interval", w.interval)
}

func (w *Worker) Stop() {
	close(w.quit)
	w.wg.Wait()
	slog.Info("embedding worker stopped")
}

// Notify signals the worker to wake up and process new content.
func (w *Worker) Notify() {
	select {
	case w.notify <- struct{}{}:
	default: // already notified
	}
}

func (w *Worker) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.processAll()
		case <-w.notify:
			w.processAll()
		case <-w.quit:
			return
		}
	}
}

// SetSummarizer configures the model client used for async summary generation.
func (w *Worker) SetSummarizer(client model.Client) {
	w.summarizer = client
}

func (w *Worker) processAll() {
	ctx := context.Background()

	// Embed unembedded messages.
	if w.semantic != nil {
		w.embedMessages(ctx)
	}

	// Embed unembedded chunks.
	if w.docs != nil {
		w.embedChunks(ctx)
	}

	// Summarize long assistant replies.
	if w.summarizer != nil && w.semantic != nil {
		w.summarizeMessages(ctx)
	}
}

func (w *Worker) embedMessages(ctx context.Context) {
	msgs, err := w.semantic.UnembeddedMessages(ctx, w.batchSize)
	if err != nil {
		slog.Warn("fetch unembedded messages", "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	texts := make([]string, len(msgs))
	for i, m := range msgs {
		texts[i] = m.Content
	}

	vectors, err := w.client.Embed(ctx, texts)
	if err != nil {
		// Batch failed — fallback to one-by-one to isolate the bad message.
		slog.Warn("embed messages batch failed, trying individually", "err", err, "count", len(texts))
		for i, m := range msgs {
			vec, err := w.client.Embed(ctx, []string{texts[i]})
			if err != nil {
				slog.Warn("embed single message failed", "id", m.ID, "err", err)
				continue
			}
			if len(vec) > 0 && vec[0] != nil {
				w.semantic.SetMessageEmbedding(ctx, m.ID, vec[0])
			}
		}
		return
	}

	for i, vec := range vectors {
		if vec == nil {
			continue
		}
		if err := w.semantic.SetMessageEmbedding(ctx, msgs[i].ID, vec); err != nil {
			slog.Warn("set message embedding", "id", msgs[i].ID, "err", err)
		}
	}

	slog.Info("embedded messages", "count", len(msgs))
}

func (w *Worker) embedChunks(ctx context.Context) {
	chunks, err := w.docs.UnembeddedChunks(ctx, w.batchSize)
	if err != nil {
		slog.Warn("fetch unembedded chunks", "err", err)
		return
	}
	if len(chunks) == 0 {
		return
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	vectors, err := w.client.Embed(ctx, texts)
	if err != nil {
		slog.Warn("embed chunks batch failed, trying individually", "err", err, "count", len(texts))
		for i, c := range chunks {
			vec, err := w.client.Embed(ctx, []string{texts[i]})
			if err != nil {
				slog.Warn("embed single chunk failed", "id", c.ID, "err", err)
				continue
			}
			if len(vec) > 0 && vec[0] != nil {
				w.docs.SetChunkEmbedding(ctx, c.ID, vec[0])
			}
		}
		return
	}

	for i, vec := range vectors {
		if vec == nil {
			continue
		}
		if err := w.docs.SetChunkEmbedding(ctx, chunks[i].ID, vec); err != nil {
			slog.Warn("set chunk embedding", "id", chunks[i].ID, "err", err)
		}
	}

	slog.Info("embedded chunks", "count", len(chunks))
}

const summaryPrompt = `Summarize the following assistant reply in 2-3 concise sentences.
Preserve the key facts, conclusions, and any specific data points.
Use the same language as the original. Output ONLY the summary, nothing else.`

func (w *Worker) summarizeMessages(ctx context.Context) {
	msgs, err := w.semantic.UnsummarizedMessages(ctx, 5)
	if err != nil {
		slog.Warn("fetch unsummarized messages", "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	for _, m := range msgs {
		messages := []model.Message{
			{Role: model.RoleSystem, Content: summaryPrompt},
			{Role: model.RoleUser, Content: m.Content},
		}
		resp, err := w.summarizer.Chat(ctx, messages, nil)
		if err != nil {
			slog.Warn("summarize message failed, will retry next cycle", "id", m.ID, "err", err)
			continue
		}
		if err := w.semantic.SetMessageSummary(ctx, m.ID, resp.Content); err != nil {
			slog.Warn("set message summary", "id", m.ID, "err", err)
		}
	}

	slog.Info("summarized messages", "count", len(msgs))
}
