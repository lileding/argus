package model

import (
	"context"
	"log/slog"
)

// FallbackClient wraps a primary Client with a fallback. If the primary
// fails, the fallback is tried. Both must implement the full Client interface.
type FallbackClient struct {
	primary  Client
	fallback Client
}

func NewFallbackClient(primary, fallback Client) Client {
	return &FallbackClient{primary: primary, fallback: fallback}
}

func (c *FallbackClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	resp, err := c.primary.Chat(ctx, messages, tools)
	if err != nil {
		slog.Warn("primary model failed, using fallback", "err", err)
		return c.fallback.Chat(ctx, messages, tools)
	}
	return resp, nil
}

func (c *FallbackClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	ch, err := c.primary.ChatStream(ctx, messages, tools)
	if err != nil {
		slog.Warn("primary stream failed, using fallback", "err", err)
		return c.fallback.ChatStream(ctx, messages, tools)
	}
	return ch, nil
}

func (c *FallbackClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	resp, err := c.primary.ChatWithEarlyAbort(ctx, messages, tools, maxTextTokens)
	if err != nil {
		slog.Warn("primary model failed, using fallback", "err", err)
		return c.fallback.ChatWithEarlyAbort(ctx, messages, tools, maxTextTokens)
	}
	return resp, nil
}

func (c *FallbackClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	result, err := c.primary.Transcribe(ctx, audioData, filename)
	if err != nil {
		slog.Warn("primary transcribe failed, using fallback", "err", err)
		return c.fallback.Transcribe(ctx, audioData, filename)
	}
	return result, nil
}
