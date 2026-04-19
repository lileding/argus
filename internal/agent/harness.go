package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// loadHistory retrieves and curates conversation history for context.
// Used by both orchestrator and synthesizer phases.
func (a *Agent) loadHistory(ctx context.Context, chatID string, excludeID int64, contextWindow int) ([]model.Message, error) {
	recent, err := a.store.RecentMessages(ctx, chatID, contextWindow+1)
	if err != nil {
		return nil, fmt.Errorf("load recent messages: %w", err)
	}

	var recalledIDs map[int64]bool
	var recalled []model.Message

	// Semantic recall via embedding.
	if a.embedder != nil {
		if ss, ok := a.store.(store.SemanticStore); ok {
			// Query text is the most recent user message (excluded from history).
			var queryText string
			if len(recent) > 0 {
				// Find the just-saved message to use its content as query.
				for _, m := range recent {
					if m.ID == excludeID {
						queryText = m.Content
						break
					}
				}
			}
			if queryText != "" {
				queryVec, err := a.embedder.EmbedOne(ctx, queryText)
				if err == nil {
					similar, err := ss.SearchMessages(ctx, queryVec, chatID, 10)
					if err == nil {
						recalledIDs = make(map[int64]bool)
						for _, m := range similar {
							recalledIDs[m.ID] = true
							recalled = append(recalled, model.Message{
								Role:    model.Role(m.Role),
								Content: m.Content,
							})
						}
					}
				}
			}
		}
	}

	// Filter recent messages: remove excluded (just-saved) and dedup vs recalled.
	var filtered []store.StoredMessage
	for _, m := range recent {
		if m.ID == excludeID {
			continue
		}
		if recalledIDs != nil && recalledIDs[m.ID] {
			continue
		}
		filtered = append(filtered, m)
	}
	if len(filtered) > contextWindow {
		filtered = filtered[len(filtered)-contextWindow:]
	}
	curated := a.curateHistory(filtered)

	// Recalled first (older, semantically relevant), then recent (chronological).
	out := make([]model.Message, 0, len(recalled)+len(curated))
	out = append(out, recalled...)
	out = append(out, curated...)
	return out, nil
}

// buildOrchestratorPrompt assembles the Phase 1 system prompt.
// Focused on tool calling, not answering. Includes pinned memories, skill catalog,
// document RAG context, environment info.
func (a *Agent) buildOrchestratorPrompt() string {
	var sb strings.Builder

	sb.WriteString(OrchestratorPrompt)

	// Environment.
	sb.WriteString(fmt.Sprintf("\n\n## Environment\n\n"))
	sb.WriteString(fmt.Sprintf("Today: %s\n", time.Now().Format("2006-01-02 (Monday)")))
	sb.WriteString(fmt.Sprintf("Home: %s\n", os.Getenv("HOME")))
	sb.WriteString(fmt.Sprintf("Workspace: %s\n", a.workspaceDir))

	// Pinned memories.
	if ps, ok := a.store.(store.PinnedMemoryStore); ok {
		if memories, err := ps.ListMemories(context.Background(), true); err == nil && len(memories) > 0 {
			sb.WriteString("\n## User Memories\n\n")
			for _, m := range memories {
				sb.WriteString(fmt.Sprintf("- [%s] %s\n", m.Category, m.Content))
			}
		}
	}

	// Builtin skills (always injected).
	if builtin := a.skillIndex.BuiltinPrompts(); builtin != "" {
		sb.WriteString(builtin)
	}

	// User skill catalog (model can load via activate_skill).
	if catalog := a.skillIndex.Catalog(); catalog != "" {
		sb.WriteString("\n")
		sb.WriteString(catalog)
	}

	return sb.String()
}

// buildSynthesizerPrompt assembles the Phase 2 system prompt.
// Focused on composing a final answer from materials, no tools.
func (a *Agent) buildSynthesizerPrompt() string {
	var sb strings.Builder

	sb.WriteString(SynthesizerPrompt)

	// Environment.
	sb.WriteString(fmt.Sprintf("\n\n## Environment\n\n"))
	sb.WriteString(fmt.Sprintf("Today: %s\n", time.Now().Format("2006-01-02 (Monday)")))
	sb.WriteString(fmt.Sprintf("Workspace: %s\n", a.workspaceDir))

	// Pinned memories (for personalization).
	if ps, ok := a.store.(store.PinnedMemoryStore); ok {
		if memories, err := ps.ListMemories(context.Background(), true); err == nil && len(memories) > 0 {
			sb.WriteString("\n## User Memories\n\n")
			for _, m := range memories {
				sb.WriteString(fmt.Sprintf("- [%s] %s\n", m.Category, m.Content))
			}
		}
	}

	return sb.String()
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// curateHistory filters message history and re-injects images from stored FilePaths.
// Only the most recent image-bearing message gets real image data injected;
// older images are replaced with a text placeholder to save context tokens.
func (a *Agent) curateHistory(messages []store.StoredMessage) []model.Message {
	// Find the last user message index that has image file paths.
	lastImageIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && hasImagePaths(messages[i].FilePaths) {
			lastImageIdx = i
			break
		}
	}

	var curated []model.Message
	for i, m := range messages {
		switch m.Role {
		case "user":
			if i == lastImageIdx {
				// Most recent image message: inject real images.
				curated = append(curated, buildUserMessage(m.Content, m.FilePaths, a.workspaceDir))
			} else if hasImagePaths(m.FilePaths) {
				// Older image message: text placeholder only.
				text := m.Content
				if text == "" {
					text = "[User previously sent an image]"
				} else {
					text += "\n[Image(s) omitted from context]"
				}
				curated = append(curated, model.Message{Role: model.RoleUser, Content: text})
			} else {
				curated = append(curated, model.Message{Role: model.RoleUser, Content: m.Content})
			}
		case "assistant":
			if m.Content != "" && m.ToolCallID == nil {
				curated = append(curated, model.Message{Role: model.RoleAssistant, Content: m.Content})
			}
		}
	}
	return curated
}

func hasImagePaths(paths []string) bool {
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		if imageExts[ext] {
			return true
		}
	}
	return false
}

// imageExts and buildUserMessage are defined in agent.go
