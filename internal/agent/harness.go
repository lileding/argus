package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// assembleContext builds the full context for the LLM from multiple sources:
// 1. System prompt (base + skills + environment)
// 2. Pinned memories (user-defined persistent facts)
// 3. Semantic recall (embedding-similar past messages)
// 4. Recent channel messages (fixed window)
// 5. Relevant document chunks (RAG)
// 6. Current user message
func (a *Agent) assembleContext(ctx context.Context, chatID string, userMsg model.Message, excludeID int64) ([]model.Message, []model.ToolDef, error) {
	systemPrompt := a.buildSystemPrompt()

	// --- Pinned memories ---
	if ps, ok := a.store.(store.PinnedMemoryStore); ok {
		memories, err := ps.ListMemories(ctx, true)
		if err == nil && len(memories) > 0 {
			systemPrompt += "\n\n## Your Memories About the User\n"
			for _, m := range memories {
				systemPrompt += fmt.Sprintf("- [%s] %s\n", m.Category, m.Content)
			}
		}
	}

	// --- Semantic recall ---
	var recalled []model.Message
	var recalledIDs map[int64]bool
	if a.embedder != nil {
		if ss, ok := a.store.(store.SemanticStore); ok {
			queryText := userMsg.TextContent()
			if queryText != "" {
				queryVec, err := a.embedder.EmbedOne(ctx, queryText)
				if err != nil {
					slog.Debug("query embedding failed", "err", err)
				} else {
					similar, err := ss.SearchMessages(ctx, queryVec, chatID, 10)
					if err != nil {
						slog.Debug("semantic search failed", "err", err)
					} else {
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

	// --- Document chunks (RAG) ---
	if a.embedder != nil {
		if ds, ok := a.store.(store.DocumentStore); ok {
			queryText := userMsg.TextContent()
			if queryText != "" {
				queryVec, err := a.embedder.EmbedOne(ctx, queryText)
				if err == nil {
					chunks, err := ds.SearchChunks(ctx, queryVec, 5)
					if err == nil && len(chunks) > 0 {
						systemPrompt += "\n\n## Relevant Documents\n"
						for _, c := range chunks {
							systemPrompt += fmt.Sprintf("\n### From: %s (chunk %d)\n%s\n", c.DocFilename, c.ChunkIndex, c.Content)
						}
					}
				}
			}
		}
	}

	// --- Recent channel messages ---
	recent, err := a.store.RecentMessages(ctx, chatID, a.contextWindow+1)
	if err != nil {
		return nil, nil, fmt.Errorf("load recent messages: %w", err)
	}
	// Exclude the just-saved message and any already in semantic recall.
	var filtered []store.StoredMessage
	for _, m := range recent {
		if m.ID == excludeID {
			continue
		}
		if recalledIDs != nil && recalledIDs[m.ID] {
			continue // already in semantic recall, don't duplicate
		}
		filtered = append(filtered, m)
	}
	if len(filtered) > a.contextWindow {
		filtered = filtered[len(filtered)-a.contextWindow:]
	}
	curated := a.curateHistory(filtered)

	// --- Assemble final message sequence ---
	messages := make([]model.Message, 0, len(recalled)+len(curated)+3)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: systemPrompt})

	// Recalled messages first (older, semantically relevant).
	if len(recalled) > 0 {
		messages = append(messages, recalled...)
	}

	// Recent messages (chronological).
	messages = append(messages, curated...)

	// Current user message last.
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)

	toolDefs := a.toolRegistry.AllToolDefs()
	return messages, toolDefs, nil
}

// buildSystemPrompt assembles: base prompt + environment + skills.
func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString(a.basePrompt)

	// Environment info.
	sb.WriteString(fmt.Sprintf("\n\nToday: %s\n", time.Now().Format("2006-01-02 (Monday)")))
	sb.WriteString(fmt.Sprintf("Home directory: %s\n", os.Getenv("HOME")))
	sb.WriteString(fmt.Sprintf("Workspace directory: %s\n", a.workspaceDir))
	sb.WriteString("The read_file and write_file tools operate on paths relative to this workspace. For files outside the workspace, use the cli tool with absolute paths (e.g. `cat /home/user/file.txt`).\n")
	sb.WriteString("Use the current_time tool when you need to know the date, time, or resolve relative time references.\n")

	// Builtin skill prompts.
	builtinPrompts := a.skillIndex.BuiltinPrompts()
	if builtinPrompts != "" {
		sb.WriteString(builtinPrompts)
	}

	// User skill catalog.
	catalog := a.skillIndex.Catalog()
	if catalog != "" {
		sb.WriteString("\n")
		sb.WriteString(catalog)
	}

	// Skill accumulation.
	sb.WriteString("\n\n## Skill Accumulation\n\n")
	sb.WriteString("When you successfully complete a new type of recurring task, use the save_skill tool to capture your approach as a reusable skill. ")
	sb.WriteString("A good skill should include: trigger conditions, step-by-step instructions, and which tools to use. Do not create skills for one-off tasks.\n")

	// CRITICAL: Reinforce tool usage at the end (recency bias — models pay most attention to the end).
	sb.WriteString("\n\n## REMINDER\n\n")
	sb.WriteString("You MUST use the search tool for any factual question about people, events, technology, music, science, or current affairs. ")
	sb.WriteString("NEVER answer factual questions from memory — your training data is outdated. Always search first, then answer based on search results. ")
	sb.WriteString("When asked to search or look something up, call the search tool IMMEDIATELY in this response. Do not say you will search — actually call the tool.\n")

	return sb.String()
}

// imageExts lists extensions to re-inject as multimodal content.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// curateHistory filters message history and re-injects images from .files/.
func (a *Agent) curateHistory(messages []store.StoredMessage) []model.Message {
	imageFiles := a.scanImageFiles()

	var curated []model.Message
	for _, m := range messages {
		switch m.Role {
		case "user":
			var dataURLs []string
			for name, absPath := range imageFiles {
				if strings.Contains(m.Content, name) {
					data, err := os.ReadFile(absPath)
					if err != nil {
						continue
					}
					contentType := http.DetectContentType(data)
					dataURL := fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(data))
					dataURLs = append(dataURLs, dataURL)
				}
			}
			if len(dataURLs) > 0 {
				curated = append(curated, model.NewMultimodalMessage(model.RoleUser, m.Content, dataURLs...))
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

// scanImageFiles returns filename → absolute path for images in .files/.
func (a *Agent) scanImageFiles() map[string]string {
	filesDir := filepath.Join(a.workspaceDir, ".files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		return nil
	}
	result := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if imageExts[ext] {
			result[e.Name()] = filepath.Join(filesDir, e.Name())
		}
	}
	return result
}
