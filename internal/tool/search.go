package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"argus/internal/config"

	"golang.org/x/net/html"
)

// --- Search provider interface ---

type SearchProvider interface {
	Search(ctx context.Context, query string, maxResults int) ([]SearchResult, string, error)
	// Returns (results, answer, error). answer is empty for providers that don't support it.
}

type SearchResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"` // cleaned relevant paragraph (Tavily) or snippet (DDG)
	Score   float64 `json:"score"`   // 0-1 relevance (Tavily); 0 for DDG
}

// --- Search tool (router) ---

type SearchTool struct {
	primary    SearchProvider
	fallback   SearchProvider
	maxResults int
}

func NewSearchTool() *SearchTool {
	ddg := &DuckDuckGoProvider{}
	return &SearchTool{primary: ddg, fallback: nil, maxResults: 5}
}

func NewSearchToolWithConfig(cfg config.SearchConfig) *SearchTool {
	ddg := &DuckDuckGoProvider{}

	var primary SearchProvider = ddg
	var fallback SearchProvider

	if cfg.Provider == "tavily" && cfg.TavilyAPIKey != "" {
		primary = NewTavilyProvider(cfg.TavilyAPIKey, cfg.IncludeAnswer)
		fallback = ddg
		slog.Info("search: using Tavily (DuckDuckGo fallback)")
	} else {
		slog.Info("search: using DuckDuckGo")
	}

	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = 5
	}

	return &SearchTool{primary: primary, fallback: fallback, maxResults: maxResults}
}

func (t *SearchTool) Name() string { return "search" }

func (t *SearchTool) Description() string {
	return "Search the web for real-time information. Returns titles, URLs, and relevant content from search results. May include a pre-generated answer summary."
}

func (t *SearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"}
		},
		"required": ["query"]
	}`)
}

type searchArgs struct {
	Query string `json:"query"`
}

func (t *SearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("empty search query")
	}

	results, answer, err := t.primary.Search(ctx, query, t.maxResults)
	if err != nil && t.fallback != nil {
		slog.Warn("search: primary failed, using fallback", "err", err)
		results, answer, err = t.fallback.Search(ctx, query, t.maxResults)
	}
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	return formatSearchResults(results, answer), nil
}

func formatSearchResults(results []SearchResult, answer string) string {
	var sb strings.Builder

	if answer != "" {
		sb.WriteString("Answer: ")
		sb.WriteString(answer)
		sb.WriteString("\n\nSources:\n")
	}

	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Title))
		sb.WriteString(fmt.Sprintf("   %s\n", r.URL))
		if r.Content != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", r.Content))
		}
		sb.WriteString("\n")
	}

	if len(results) == 0 && answer == "" {
		return "No results found."
	}

	return sb.String()
}

// --- DuckDuckGo provider (free fallback) ---

type DuckDuckGoProvider struct{}

func (p *DuckDuckGoProvider) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, string, error) {
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Argus/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}

	results := parseDDGResults(string(body), maxResults)
	return results, "", nil
}

func parseDDGResults(htmlBody string, max int) []SearchResult {
	doc, err := html.Parse(strings.NewReader(htmlBody))
	if err != nil {
		return nil
	}

	var results []SearchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= max {
			return
		}
		if n.Type == html.ElementNode && n.Data == "div" {
			for _, attr := range n.Attr {
				if attr.Key == "class" && strings.Contains(attr.Val, "result__body") {
					r := extractDDGResult(n)
					if r.Title != "" && r.URL != "" {
						results = append(results, r)
					}
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return results
}

func extractDDGResult(node *html.Node) SearchResult {
	var r SearchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, attr := range n.Attr {
				if attr.Key == "class" {
					switch {
					case strings.Contains(attr.Val, "result__a"):
						r.Title = textContent(n)
						for _, a := range n.Attr {
							if a.Key == "href" {
								r.URL = cleanDDGURL(a.Val)
							}
						}
					case strings.Contains(attr.Val, "result__snippet"):
						r.Content = textContent(n)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(node)
	return r
}

func textContent(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

func cleanDDGURL(raw string) string {
	if strings.Contains(raw, "duckduckgo.com/l/") {
		u, err := url.Parse(raw)
		if err == nil {
			if uddg := u.Query().Get("uddg"); uddg != "" {
				return uddg
			}
		}
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return raw
}
