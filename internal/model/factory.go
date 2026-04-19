package model

import (
	"context"
	"fmt"
	"log/slog"

	"argus/internal/config"
)

// NewClientFromConfig creates a model.Client for a specific role using
// the named upstream configuration.
func NewClientFromConfig(ctx context.Context, upstreams map[string]config.UpstreamConfig, role config.RoleConfig) (Client, error) {
	up, ok := upstreams[role.Upstream]
	if !ok {
		return nil, fmt.Errorf("upstream %q not found (available: %v)", role.Upstream, upstreamNames(upstreams))
	}

	switch up.Type {
	case "openai":
		client := NewOpenAIClientFromUpstream(up, role)
		slog.Info("model client created",
			"upstream", role.Upstream, "type", "openai",
			"model", role.ModelName, "max_tokens", role.MaxTokens,
		)
		return client, nil

	case "gemini":
		client, err := NewGeminiClient(ctx, up.APIKey, role.ModelName, role.MaxTokens)
		if err != nil {
			return nil, fmt.Errorf("gemini client for %q: %w", role.ModelName, err)
		}
		slog.Info("model client created",
			"upstream", role.Upstream, "type", "gemini",
			"model", role.ModelName, "max_tokens", role.MaxTokens,
		)
		return client, nil

	case "anthropic":
		client := NewAnthropicClient(up.APIKey, role.ModelName, role.MaxTokens, up.Timeout)
		slog.Info("model client created",
			"upstream", role.Upstream, "type", "anthropic",
			"model", role.ModelName, "max_tokens", role.MaxTokens,
		)
		return client, nil

	default:
		return nil, fmt.Errorf("unknown upstream type %q for upstream %q", up.Type, role.Upstream)
	}
}

// NewClientsForAgent creates orchestrator + synthesizer + transcription clients.
// Each is wrapped in RetryClient for 429 handling. No fallback — errors are
// returned directly to the user.
func NewClientsForAgent(ctx context.Context, upstreams map[string]config.UpstreamConfig, model config.ModelConfig) (orchestrator, synthesizer, transcription Client, err error) {
	orch, err := NewClientFromConfig(ctx, upstreams, model.Orchestrator)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("orchestrator: %w", err)
	}

	synth, err := NewClientFromConfig(ctx, upstreams, model.Synthesizer)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("synthesizer: %w", err)
	}

	trans, err := NewClientFromConfig(ctx, upstreams, model.Transcription)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("transcription: %w", err)
	}

	return NewRetryClient(orch), NewRetryClient(synth), trans, nil
}

func upstreamNames(m map[string]config.UpstreamConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}
