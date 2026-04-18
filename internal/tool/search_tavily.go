package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const tavilyURL = "https://api.tavily.com/search"

type TavilyProvider struct {
	apiKey        string
	includeAnswer bool
}

func NewTavilyProvider(apiKey string, includeAnswer bool) *TavilyProvider {
	return &TavilyProvider{apiKey: apiKey, includeAnswer: includeAnswer}
}

type tavilyRequest struct {
	Query             string `json:"query"`
	SearchDepth       string `json:"search_depth"`
	MaxResults        int    `json:"max_results"`
	IncludeAnswer     bool   `json:"include_answer"`
	IncludeRawContent bool   `json:"include_raw_content"`
	APIKey            string `json:"api_key"`
}

type tavilyResponse struct {
	Answer  string         `json:"answer"`
	Results []tavilyResult `json:"results"`
}

type tavilyResult struct {
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Content string  `json:"content"` // cleaned relevant paragraph
	Score   float64 `json:"score"`   // 0-1 relevance
}

func (p *TavilyProvider) Search(ctx context.Context, query string, maxResults int) ([]SearchResult, string, error) {
	reqBody := tavilyRequest{
		Query:             query,
		SearchDepth:       "basic",
		MaxResults:        maxResults,
		IncludeAnswer:     p.includeAnswer,
		IncludeRawContent: false,
		APIKey:            p.apiKey,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "POST", tavilyURL, bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("tavily request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("tavily error: status=%d body=%s", resp.StatusCode, body)
	}

	var tavilyResp tavilyResponse
	if err := json.Unmarshal(body, &tavilyResp); err != nil {
		return nil, "", fmt.Errorf("parse response: %w", err)
	}

	results := make([]SearchResult, len(tavilyResp.Results))
	for i, r := range tavilyResp.Results {
		results[i] = SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Content: r.Content,
			Score:   r.Score,
		}
	}

	return results, tavilyResp.Answer, nil
}
