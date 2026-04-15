package docindex

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"argus/internal/sandbox"
	"argus/internal/store"
)

// Ingester processes pending documents: extracts text, chunks it, and stores chunks for embedding.
type Ingester struct {
	docStore  store.DocumentStore
	sandbox   sandbox.Sandbox
	interval  time.Duration
	chunkSize int // target chunk size in runes
	overlap   int // overlap in runes
	quit      chan struct{}
	wg        sync.WaitGroup
}

func NewIngester(docStore store.DocumentStore, sb sandbox.Sandbox, interval time.Duration) *Ingester {
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &Ingester{
		docStore:  docStore,
		sandbox:   sb,
		interval:  interval,
		chunkSize: 1500, // ~500 tokens
		overlap:   300,  // ~100 tokens
		quit:      make(chan struct{}),
	}
}

func (ing *Ingester) Start() {
	ing.wg.Add(1)
	go ing.run()
	slog.Info("document ingester started")
}

func (ing *Ingester) Stop() {
	close(ing.quit)
	ing.wg.Wait()
	slog.Info("document ingester stopped")
}

func (ing *Ingester) run() {
	defer ing.wg.Done()
	ticker := time.NewTicker(ing.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ing.processAll()
		case <-ing.quit:
			return
		}
	}
}

func (ing *Ingester) processAll() {
	ctx := context.Background()
	docs, err := ing.docStore.PendingDocuments(ctx, 5)
	if err != nil {
		slog.Warn("fetch pending documents", "err", err)
		return
	}

	for _, doc := range docs {
		ing.processDocument(ctx, doc)
	}
}

func (ing *Ingester) processDocument(ctx context.Context, doc store.Document) {
	slog.Info("processing document", "id", doc.ID, "filename", doc.Filename)

	ing.docStore.UpdateDocumentStatus(ctx, doc.ID, "processing", "")

	// Extract text based on file extension.
	text, err := ing.extractText(ctx, doc)
	if err != nil {
		slog.Warn("extract text failed", "doc", doc.Filename, "err", err)
		ing.docStore.UpdateDocumentStatus(ctx, doc.ID, "error", err.Error())
		return
	}

	if strings.TrimSpace(text) == "" {
		ing.docStore.UpdateDocumentStatus(ctx, doc.ID, "error", "no text content extracted")
		return
	}

	// Chunk the text.
	textChunks := ChunkText(text, ing.chunkSize, ing.overlap)

	// Store chunks.
	chunks := make([]store.Chunk, len(textChunks))
	for i, content := range textChunks {
		chunks[i] = store.Chunk{
			DocumentID: doc.ID,
			ChunkIndex: i,
			Content:    content,
		}
	}

	if err := ing.docStore.SaveChunks(ctx, chunks); err != nil {
		slog.Warn("save chunks failed", "doc", doc.Filename, "err", err)
		ing.docStore.UpdateDocumentStatus(ctx, doc.ID, "error", err.Error())
		return
	}

	ing.docStore.UpdateDocumentStatus(ctx, doc.ID, "ready", "")
	slog.Info("document ready", "id", doc.ID, "filename", doc.Filename, "chunks", len(chunks))
}

func (ing *Ingester) extractText(ctx context.Context, doc store.Document) (string, error) {
	ext := strings.ToLower(doc.Filename)

	switch {
	case strings.HasSuffix(ext, ".pdf"):
		return ing.sandbox.Exec(ctx, "pdftotext '"+doc.FilePath+"' -", "")
	case strings.HasSuffix(ext, ".txt"), strings.HasSuffix(ext, ".md"),
		strings.HasSuffix(ext, ".csv"), strings.HasSuffix(ext, ".json"),
		strings.HasSuffix(ext, ".xml"), strings.HasSuffix(ext, ".yaml"),
		strings.HasSuffix(ext, ".yml"), strings.HasSuffix(ext, ".log"):
		return ing.sandbox.Exec(ctx, "cat '"+doc.FilePath+"'", "")
	case strings.HasSuffix(ext, ".docx"):
		// Try python-docx if available.
		return ing.sandbox.Exec(ctx, "python3 -c \"from docx import Document; d=Document('"+doc.FilePath+"'); print('\\n'.join(p.text for p in d.paragraphs))\"", "")
	default:
		// Try cat as fallback.
		return ing.sandbox.Exec(ctx, "cat '"+doc.FilePath+"'", "")
	}
}

// ChunkText splits text into overlapping chunks.
func ChunkText(text string, chunkSize, overlap int) []string {
	runes := []rune(text)
	if len(runes) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	start := 0
	for start < len(runes) {
		end := start + chunkSize
		if end > len(runes) {
			end = len(runes)
		}

		chunk := string(runes[start:end])
		chunks = append(chunks, strings.TrimSpace(chunk))

		if end >= len(runes) {
			break
		}

		start += chunkSize - overlap
	}

	return chunks
}
