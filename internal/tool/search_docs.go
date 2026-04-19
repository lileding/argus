package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"argus/internal/embedding"
	"argus/internal/store"
)

// --- SearchDocsTool ---

type SearchDocsTool struct {
	docStore store.DocumentStore
	embedder *embedding.Client
}

func NewSearchDocsTool(ds store.DocumentStore, emb *embedding.Client) *SearchDocsTool {
	return &SearchDocsTool{docStore: ds, embedder: emb}
}

func (t *SearchDocsTool) Name() string { return "search_docs" }

func (t *SearchDocsTool) Description() string {
	return "Search your personal knowledge base (uploaded PDFs, documents). " +
		"Returns relevant excerpts from indexed documents ranked by relevance. " +
		"Use list_docs first to see available documents, then search_docs to find specific content."
}

func (t *SearchDocsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query — what to look for in the documents"},
			"limit": {"type": "integer", "description": "Max results to return (default 5)"},
			"filename": {"type": "string", "description": "Optional: filter to a specific document by filename"}
		},
		"required": ["query"]
	}`)
}

type searchDocsArgs struct {
	Query    string `json:"query"`
	Limit    int    `json:"limit"`
	Filename string `json:"filename"`
}

func (t *SearchDocsTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args searchDocsArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	// Embed the query.
	vec, err := t.embedder.EmbedOne(ctx, args.Query)
	if err != nil {
		return "", fmt.Errorf("embed query: %w", err)
	}

	// Search chunks. Fetch more if filtering by filename.
	searchLimit := limit
	if args.Filename != "" {
		searchLimit = limit * 5 // over-fetch then filter
	}
	chunks, err := t.docStore.SearchChunks(ctx, vec, searchLimit)
	if err != nil {
		return "", fmt.Errorf("search chunks: %w", err)
	}

	// Filter by filename if specified.
	if args.Filename != "" {
		var filtered []store.Chunk
		for _, c := range chunks {
			if strings.Contains(strings.ToLower(c.DocFilename), strings.ToLower(args.Filename)) {
				filtered = append(filtered, c)
				if len(filtered) >= limit {
					break
				}
			}
		}
		chunks = filtered
	} else if len(chunks) > limit {
		chunks = chunks[:limit]
	}

	if len(chunks) == 0 {
		if args.Filename != "" {
			return fmt.Sprintf("No results found for %q in document %q. Use list_docs to see available documents.", args.Query, args.Filename), nil
		}
		return fmt.Sprintf("No results found for %q. Use list_docs to see available documents.", args.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d relevant excerpts:\n", len(chunks)))
	for i, c := range chunks {
		sb.WriteString(fmt.Sprintf("\n%d. [%s, chunk %d] (similarity: %.2f)\n",
			i+1, c.DocFilename, c.ChunkIndex, c.Similarity))
		// Truncate long chunks for readability.
		content := c.Content
		if len(content) > 2000 {
			content = content[:2000] + "..."
		}
		sb.WriteString(content)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// --- ListDocsTool ---

type ListDocsTool struct {
	docStore store.DocumentStore
}

func NewListDocsTool(ds store.DocumentStore) *ListDocsTool {
	return &ListDocsTool{docStore: ds}
}

func (t *ListDocsTool) Name() string { return "list_docs" }

func (t *ListDocsTool) Description() string {
	return "List all indexed documents in your personal knowledge base. " +
		"Shows filenames and chunk counts. Use search_docs to search within them."
}

func (t *ListDocsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *ListDocsTool) Execute(ctx context.Context, _ string) (string, error) {
	docs, err := t.docStore.ListDocuments(ctx)
	if err != nil {
		return "", fmt.Errorf("list documents: %w", err)
	}
	if len(docs) == 0 {
		return "No documents indexed yet. Upload a PDF or document via Feishu to get started.", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Indexed documents (%d):\n", len(docs)))
	for i, d := range docs {
		sb.WriteString(fmt.Sprintf("%d. %s (%s, indexed %s)\n",
			i+1, d.Filename, d.ErrorMsg, d.CreatedAt.Format("2006-01-02")))
	}
	return sb.String(), nil
}
