package model

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// isRetryable returns true for transient errors (rate limits) that should
// be retried on the same model rather than falling back.
func isRetryable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status=429")
}

const retryDelay = 5 * time.Second
const maxRetries = 2

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
	for attempt := 0; ; attempt++ {
		resp, err := c.primary.Chat(ctx, messages, tools)
		if err == nil {
			return resp, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying", "attempt", attempt+1, "delay", retryDelay)
			time.Sleep(retryDelay)
			continue
		}
		slog.Warn("primary model failed, using fallback", "err", err)
		return c.fallback.Chat(ctx, messages, tools)
	}
}

func (c *FallbackClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	for attempt := 0; ; attempt++ {
		ch, err := c.primary.ChatStream(ctx, messages, tools)
		if err == nil {
			return ch, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying", "attempt", attempt+1, "delay", retryDelay)
			time.Sleep(retryDelay)
			continue
		}
		slog.Warn("primary stream failed, using fallback", "err", err)
		return c.fallback.ChatStream(ctx, messages, tools)
	}
}

func (c *FallbackClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.primary.ChatWithEarlyAbort(ctx, messages, tools, maxTextTokens)
		if err == nil {
			return resp, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying", "attempt", attempt+1, "delay", retryDelay)
			time.Sleep(retryDelay)
			continue
		}
		slog.Warn("primary model failed, using fallback", "err", err)
		return c.fallback.ChatWithEarlyAbort(ctx, messages, tools, maxTextTokens)
	}
}

func (c *FallbackClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	result, err := c.primary.Transcribe(ctx, audioData, filename)
	if err != nil {
		slog.Warn("primary transcribe failed, using fallback", "err", err)
		return c.fallback.Transcribe(ctx, audioData, filename)
	}
	return result, nil
}
