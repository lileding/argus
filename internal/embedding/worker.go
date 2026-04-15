package embedding

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"argus/internal/store"
)

// Worker runs a background loop that embeds unembedded messages and chunks.
type Worker struct {
	client    *Client
	semantic  store.SemanticStore
	pinned    store.PinnedMemoryStore
	docs      store.DocumentStore
	batchSize int
	interval  time.Duration
	notify    chan struct{}
	quit      chan struct{}
	wg        sync.WaitGroup
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
		slog.Warn("embed messages batch", "err", err, "count", len(texts))
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
		slog.Warn("embed chunks batch", "err", err, "count", len(texts))
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
