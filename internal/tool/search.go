package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type SearchTool struct{}

func NewSearchTool() *SearchTool { return &SearchTool{} }

func (t *SearchTool) Name() string { return "search" }

func (t *SearchTool) Description() string {
	return "Search the web for real-time information. Returns titles, URLs, and snippets from search results."
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

type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

func (t *SearchTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	results, err := duckduckgoSearch(ctx, args.Query, 5)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	if len(results) == 0 {
		return "No results found.", nil
	}

	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
	}
	return sb.String(), nil
}

func duckduckgoSearch(ctx context.Context, query string, maxResults int) ([]searchResult, error) {
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Argus/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return parseDDGResults(resp.Body, maxResults)
}

func parseDDGResults(r io.Reader, maxResults int) ([]searchResult, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}

	var results []searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(results) >= maxResults {
			return
		}

		// DuckDuckGo results are in <a class="result__a"> for title/link
		// and <a class="result__snippet"> for snippet
		if n.Type == html.ElementNode && n.Data == "a" {
			class := getAttr(n, "class")
			if strings.Contains(class, "result__a") {
				href := getAttr(n, "href")
				title := textContent(n)
				if title != "" && href != "" {
					// DDG wraps URLs in a redirect, extract actual URL
					actualURL := extractDDGURL(href)
					results = append(results, searchResult{
						Title: strings.TrimSpace(title),
						URL:   actualURL,
					})
				}
			}
			if strings.Contains(class, "result__snippet") {
				snippet := strings.TrimSpace(textContent(n))
				if snippet != "" && len(results) > 0 {
					results[len(results)-1].Snippet = snippet
				}
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return results, nil
}

func extractDDGURL(href string) string {
	// DDG HTML wraps links as //duckduckgo.com/l/?uddg=<encoded_url>&...
	if u, err := url.Parse(href); err == nil {
		if uddg := u.Query().Get("uddg"); uddg != "" {
			return uddg
		}
	}
	return href
}

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	if n.Type == html.TextNode {
		return n.Data
	}
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		sb.WriteString(textContent(c))
	}
	return sb.String()
}
