package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// Thresholds and budgets for passive recall.
const (
	msgSimThreshold   = 0.30 // cosine similarity minimum for message recall
	chunkSimThreshold = 0.35 // cosine similarity minimum for document chunk recall
	recallBytesBudget = 6000 // max total bytes from all recalled content
	chunkBytesBudget  = 4000 // max total bytes from document chunks (subset of above)
)

// loadHistory retrieves and curates conversation history for context.
// All messages (recalled + recent) go through curateHistory for uniform
// summary/truncation treatment. This prevents semantic recall from
// bypassing the long-reply summarization mechanism.
func (a *Agent) loadHistory(ctx context.Context, chatID string, excludeID int64, contextWindow int) ([]model.Message, error) {
	recent, err := a.store.RecentMessages(ctx, chatID, contextWindow+1)
	if err != nil {
		return nil, fmt.Errorf("load recent messages: %w", err)
	}

	// Build set of recent IDs for dedup.
	recentIDs := make(map[int64]bool, len(recent))
	for _, m := range recent {
		recentIDs[m.ID] = true
	}

	var recalledMsgs []store.StoredMessage
	var docChunkMsg *model.Message

	// Semantic recall via embedding.
	if a.embedder != nil {
		if ss, ok := a.store.(store.SemanticStore); ok {
			// Query text is the most recent user message (excluded from history).
			var queryText string
			for _, m := range recent {
				if m.ID == excludeID {
					queryText = m.Content
					break
				}
			}
			if queryText != "" {
				queryVec, err := a.embedder.EmbedOne(ctx, queryText)
				if err == nil {
					// Search conversation history with similarity threshold + byte budget.
					// Budget is computed on effective size (summary or truncated), not
					// raw content, so a 3000-byte reply with a 100-byte summary doesn't
					// consume 3000 bytes of budget.
					similar, err := ss.SearchMessages(ctx, queryVec, chatID, 10)
					if err == nil {
						totalBytes := 0
						for _, m := range similar {
							if m.Similarity < msgSimThreshold {
								continue
							}
							if recentIDs[m.ID] || m.ID == excludeID {
								continue // already in sliding window
							}
							effectiveSize := effectiveContentSize(m)
							if totalBytes+effectiveSize > recallBytesBudget {
								continue // skip this candidate, try smaller ones
							}
							totalBytes += effectiveSize
							recalledMsgs = append(recalledMsgs, m)
						}
					}

					// Search document chunks (RAG) — passive recall.
					if ds, ok := a.store.(store.DocumentStore); ok {
						chunks, err := ds.SearchChunks(ctx, queryVec, 3)
						if err == nil {
							var sb strings.Builder
							chunkBytes := 0
							for _, c := range chunks {
								if c.Similarity < chunkSimThreshold {
									continue
								}
								content := c.Content
								if chunkBytes+len(content) > chunkBytesBudget {
									content = content[:chunkBytesBudget-chunkBytes]
								}
								if sb.Len() == 0 {
									sb.WriteString("[Relevant document excerpts]\n")
								}
								sb.WriteString(fmt.Sprintf("\n--- From: %s ---\n%s\n", c.DocFilename, content))
								chunkBytes += len(content)
								if chunkBytes >= chunkBytesBudget {
									break
								}
							}
							if sb.Len() > 0 {
								msg := model.Message{Role: model.RoleUser, Content: sb.String()}
								docChunkMsg = &msg
							}
						}
					}
				}
			}
		}
	}

	// Filter recent: remove excluded message, apply sliding window.
	var filtered []store.StoredMessage
	for _, m := range recent {
		if m.ID == excludeID {
			continue
		}
		filtered = append(filtered, m)
	}
	if len(filtered) > contextWindow {
		filtered = filtered[len(filtered)-contextWindow:]
	}

	// Sort recalled by time (SearchMessages returns similarity order).
	sort.Slice(recalledMsgs, func(i, j int) bool {
		return recalledMsgs[i].CreatedAt.Before(recalledMsgs[j].CreatedAt)
	})

	// Both recalled and recent go through curateHistory for uniform
	// summary/truncation. Recalled messages skip image reinjection
	// (they're old context; only recent sliding window gets real images).
	curatedRecalled := a.curateHistory(recalledMsgs)
	curatedRecent := a.curateHistory(filtered)

	// Assemble: doc chunks → recalled messages → recent messages.
	out := make([]model.Message, 0, len(curatedRecalled)+len(curatedRecent)+1)
	if docChunkMsg != nil {
		out = append(out, *docChunkMsg)
	}
	out = append(out, curatedRecalled...)
	out = append(out, curatedRecent...)
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
				content := m.Content
				// For long assistant replies: use async-generated summary if
				// available, otherwise truncate as fallback. This prevents old
				// verbose answers from overshadowing the current user message.
				// The orchestrator can use search_history to retrieve full text.
				const maxAssistantHistoryRunes = 800
				if runes := []rune(content); len(runes) > maxAssistantHistoryRunes {
					if m.Summary != nil && *m.Summary != "" {
						content = "[Summary of previous reply] " + *m.Summary
					} else {
						content = string(runes[:maxAssistantHistoryRunes]) + " …[truncated, use search_history for full text]"
					}
				}
				curated = append(curated, model.Message{Role: model.RoleAssistant, Content: content})
			}
		}
	}
	return curated
}

// effectiveContentSize estimates the byte size a message will occupy in
// context after curateHistory applies summary/truncation. Used to compute
// recall byte budgets on effective content rather than raw content.
func effectiveContentSize(m store.StoredMessage) int {
	const maxAssistantHistoryRunes = 800
	if m.Role == "assistant" {
		runes := []rune(m.Content)
		if len(runes) > maxAssistantHistoryRunes {
			if m.Summary != nil && *m.Summary != "" {
				return len("[Summary of previous reply] ") + len(*m.Summary)
			}
			return maxAssistantHistoryRunes*3 + len(" …[truncated, use search_history for full text]")
		}
	}
	return len(m.Content)
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
