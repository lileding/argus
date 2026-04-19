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

	case "vertex_ai":
		client, err := NewVertexAIClient(ctx, up.Project, up.Location, role.ModelName, role.MaxTokens)
		if err != nil {
			return nil, fmt.Errorf("vertex_ai client for %q: %w", role.ModelName, err)
		}
		slog.Info("model client created",
			"upstream", role.Upstream, "type", "vertex_ai",
			"model", role.ModelName, "max_tokens", role.MaxTokens,
		)
		return client, nil

	default:
		return nil, fmt.Errorf("unknown upstream type %q for upstream %q", up.Type, role.Upstream)
	}
}

// NewClientsForAgent creates orchestrator + synthesizer + fallback clients.
func NewClientsForAgent(ctx context.Context, upstreams map[string]config.UpstreamConfig, model config.ModelConfig) (orchestrator, synthesizer, fallback, transcription Client, err error) {
	orchestrator, err = NewClientFromConfig(ctx, upstreams, model.Orchestrator)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("orchestrator: %w", err)
	}

	synthesizer, err = NewClientFromConfig(ctx, upstreams, model.Synthesizer)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("synthesizer: %w", err)
	}

	fallback, err = NewClientFromConfig(ctx, upstreams, model.Fallback)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("fallback: %w", err)
	}

	transcription, err = NewClientFromConfig(ctx, upstreams, model.Transcription)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("transcription: %w", err)
	}

	return orchestrator, synthesizer, fallback, transcription, nil
}

func upstreamNames(m map[string]config.UpstreamConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}
