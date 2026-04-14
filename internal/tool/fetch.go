package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const maxFetchLen = 16000

type FetchTool struct{}

func NewFetchTool() *FetchTool { return &FetchTool{} }

func (t *FetchTool) Name() string { return "fetch" }

func (t *FetchTool) Description() string {
	return "Fetch a URL and return its content as plain text. HTML pages are converted to readable text. Use this to read web pages, follow links from search results, or download text content."
}

func (t *FetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`)
}

type fetchArgs struct {
	URL string `json:"url"`
}

func (t *FetchTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args fetchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", args.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Argus/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")

	// Limit read to 1MB raw.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	var text string
	if strings.Contains(contentType, "text/html") {
		text = htmlToText(strings.NewReader(string(body)))
	} else {
		text = string(body)
	}

	if len(text) > maxFetchLen {
		text = text[:maxFetchLen] + fmt.Sprintf("\n\n... [truncated: %d chars total]", len(text))
	}

	return text, nil
}

// htmlToText extracts visible text from HTML, skipping script/style tags.
func htmlToText(r io.Reader) string {
	doc, err := html.Parse(r)
	if err != nil {
		return ""
	}

	var sb strings.Builder
	var skip int // nesting depth inside skip tags
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "head":
				skip++
				defer func() { skip-- }()
			case "br":
				sb.WriteString("\n")
			case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr":
				sb.WriteString("\n")
			}
		}

		if n.Type == html.TextNode && skip == 0 {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				sb.WriteString(text)
				sb.WriteString(" ")
			}
		}

		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return strings.TrimSpace(sb.String())
}
