package model

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// isRetryable returns true for transient rate-limit errors.
func isRetryable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status=429")
}

const retryDelay = 30 * time.Second
const maxRetries = 2

// RetryClient wraps a Client with 429 retry logic. No fallback to a
// different model — if retries are exhausted, the error is returned.
type RetryClient struct {
	inner Client
}

func NewRetryClient(inner Client) Client {
	return &RetryClient{inner: inner}
}

func (c *RetryClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.inner.Chat(ctx, messages, tools)
		if err == nil {
			return resp, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying", "attempt", attempt+1, "delay", retryDelay)
			if !sleepCtx(ctx, retryDelay) {
				return nil, ctx.Err()
			}
			continue
		}
		return nil, err
	}
}

func (c *RetryClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	for attempt := 0; ; attempt++ {
		ch, err := c.inner.ChatStream(ctx, messages, tools)
		if err == nil {
			return ch, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying stream", "attempt", attempt+1, "delay", retryDelay)
			if !sleepCtx(ctx, retryDelay) {
				return nil, ctx.Err()
			}
			continue
		}
		return nil, err
	}
}

func (c *RetryClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.inner.ChatWithEarlyAbort(ctx, messages, tools, maxTextTokens)
		if err == nil {
			return resp, nil
		}
		if isRetryable(err) && attempt < maxRetries {
			slog.Warn("rate limited, retrying", "attempt", attempt+1, "delay", retryDelay)
			if !sleepCtx(ctx, retryDelay) {
				return nil, ctx.Err()
			}
			continue
		}
		return nil, err
	}
}

func (c *RetryClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	return c.inner.Transcribe(ctx, audioData, filename)
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
