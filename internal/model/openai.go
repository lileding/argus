package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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

// Transcribe sends an audio file to the /v1/audio/transcriptions endpoint.
func (c *OpenAIClient) Transcribe(ctx context.Context, audioData []byte, filename string) (string, error) {
	// Build multipart form.
	boundary := "----ArgusAudioBoundary"
	var buf bytes.Buffer
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString("Content-Disposition: form-data; name=\"model\"\r\n\r\n")
	buf.WriteString(c.transcriptionModel + "\r\n")
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(fmt.Sprintf("Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", filename))
	buf.WriteString("Content-Type: application/octet-stream\r\n\r\n")
	buf.Write(audioData)
	buf.WriteString("\r\n--" + boundary + "--\r\n")

	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("transcribe request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcribe error: status=%d body=%s", resp.StatusCode, respBody)
	}

	// Response is {"text": "transcribed text"} in OpenAI format.
	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		// If not JSON, return raw text.
		return string(respBody), nil
	}

	return result.Text, nil
}
