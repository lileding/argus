package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"argus/internal/config"
)

// OpenAIClient implements the Client interface using an OpenAI-compatible API.
type OpenAIClient struct {
	baseURL            string
	apiKey             string
	modelName          string
	transcriptionModel string
	maxTokens          int
	client             *http.Client
}

func NewOpenAIClient(cfg config.ModelConfig) *OpenAIClient {
	return &OpenAIClient{
		baseURL:            cfg.BaseURL,
		apiKey:             cfg.APIKey,
		modelName:          cfg.ModelName,
		transcriptionModel: cfg.TranscriptionModel,
		maxTokens:          cfg.MaxTokens,
		client:             &http.Client{Timeout: cfg.Timeout},
	}
}

// openAI request/response types for the chat completions API.

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Tools     []ToolDef `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream,omitempty"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
	Error   *apiError    `json:"error,omitempty"`
}

type chatChoice struct {
	Message      choiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type choiceMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func (c *OpenAIClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	reqBody := chatRequest{
		Model:     c.modelName,
		Messages:  messages,
		MaxTokens: c.maxTokens,
	}
	if len(tools) > 0 {
		reqBody.Tools = tools
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api error: status=%d body=%s", resp.StatusCode, respBody)
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if chatResp.Error != nil {
		return nil, fmt.Errorf("api error: %s", chatResp.Error.Message)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}

	choice := chatResp.Choices[0]
	result := &Response{
		Content:      choice.Message.Content,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Usage:        chatResp.Usage,
	}

	return result, nil
}

// ChatStream opens a streaming chat completion and returns a channel of chunks.
// The channel is closed after a chunk with Done=true. On error, a final chunk
// with Done=true and Err set is sent.
func (c *OpenAIClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	reqBody := chatRequest{
		Model:     c.modelName,
		Messages:  messages,
		MaxTokens: c.maxTokens,
		Stream:    true,
	}
	if len(tools) > 0 {
		reqBody.Tools = tools
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	// Streaming uses a separate http.Client without Timeout, since the whole
	// generation may exceed the normal per-request timeout.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("api error: status=%d body=%s", resp.StatusCode, body)
	}

	out := make(chan StreamChunk, 32)
	go func() {
		defer resp.Body.Close()
		defer close(out)

		reader := bufio.NewReader(resp.Body)
		var finalUsage Usage
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					out <- StreamChunk{Done: true, Usage: finalUsage}
					return
				}
				out <- StreamChunk{Done: true, Usage: finalUsage, Err: fmt.Errorf("read stream: %w", err)}
				return
			}

			line = strings.TrimRight(line, "\r\n")
			if line == "" || !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := line[len("data: "):]
			if payload == "[DONE]" {
				out <- StreamChunk{Done: true, Usage: finalUsage}
				return
			}

			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *Usage `json:"usage,omitempty"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue // skip malformed chunks
			}
			if chunk.Usage != nil {
				finalUsage = *chunk.Usage
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				select {
				case out <- StreamChunk{Delta: chunk.Choices[0].Delta.Content}:
				case <-ctx.Done():
					out <- StreamChunk{Done: true, Usage: finalUsage, Err: ctx.Err()}
					return
				}
			}
		}
	}()

	return out, nil
}

// TranscriptionResult contains the transcribed text and confidence info.
type TranscriptionResult struct {
	Text       string
	Confidence float64 // average log probability (higher = more confident, typically -0.0 to -1.0)
}

// Transcribe sends an audio file to the /v1/audio/transcriptions endpoint.
// Includes a prompt hint for domain vocabulary and requests verbose output for confidence.
func (c *OpenAIClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	boundary := "----ArgusAudioBoundary"
	var buf bytes.Buffer

	// Model.
	writeFormField(&buf, boundary, "model", c.transcriptionModel)

	// Prompt: domain vocabulary hints for better accuracy.
	writeFormField(&buf, boundary, "prompt",
		"This audio may contain mixed Chinese and English. "+
			"Domain vocabulary includes technology terms (API, Kubernetes, Docker, GPU, LLM, transformer, embedding, MLX, vLLM), "+
			"finance terms (ETF, PE ratio, hedge fund, derivatives, quantitative), "+
			"and arts terms (sonata, concerto, fugue, Chopin, Debussy, Rachmaninoff, Scriabin, Grieg, Dvořák, Mahler). "+
			"Transcribe accurately, preserving code-switching between languages.")

	// Response format: verbose_json for confidence scores.
	writeFormField(&buf, boundary, "response_format", "verbose_json")

	// Language hint.
	writeFormField(&buf, boundary, "language", "zh")

	// Audio file.
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename))
	buf.WriteString("Content-Type: application/octet-stream\r\n\r\n")
	buf.Write(audioData)
	buf.WriteString("\r\n--" + boundary + "--\r\n")

	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transcribe request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transcribe error: status=%d body=%s", resp.StatusCode, respBody)
	}

	// Parse verbose_json response.
	var verbose struct {
		Text     string `json:"text"`
		Segments []struct {
			AvgLogprob float64 `json:"avg_logprob"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(respBody, &verbose); err != nil {
		// Fallback: try simple format.
		var simple struct {
			Text string `json:"text"`
		}
		if err2 := json.Unmarshal(respBody, &simple); err2 != nil {
			return &TranscriptionResult{Text: string(respBody), Confidence: 0}, nil
		}
		return &TranscriptionResult{Text: simple.Text, Confidence: 0}, nil
	}

	// Compute average confidence across segments.
	var avgConf float64
	if len(verbose.Segments) > 0 {
		var sum float64
		for _, seg := range verbose.Segments {
			sum += seg.AvgLogprob
		}
		avgConf = sum / float64(len(verbose.Segments))
	}

	return &TranscriptionResult{Text: verbose.Text, Confidence: avgConf}, nil
}

func writeFormField(buf *bytes.Buffer, boundary, name, value string) {
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"%s\"\r\n\r\n", name))
	buf.WriteString(value + "\r\n")
}
