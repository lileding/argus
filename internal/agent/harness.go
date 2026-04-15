package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/model"
	"argus/internal/store"
)

// assembleContext implements the harness: system prompt assembly with skill catalog,
// history curation, and tool preparation.
//
// The LLM sees the skill catalog (name + description) in the system prompt and
// uses activate_skill to load full instructions when needed. No code-level
// pre-filtering — the LLM is the best judge of intent.
func (a *Agent) assembleContext(ctx context.Context, chatID string, userMsg model.Message, excludeID int64) ([]model.Message, []model.ToolDef, error) {
	// Build system prompt with skill catalog.
	systemPrompt := a.buildSystemPrompt()

	// Curate history: keep only user messages + assistant final replies.
	// Exclude the just-saved current message to prevent duplication.
	recent, err := a.store.RecentMessages(ctx, chatID, a.contextWindow+1) // +1 to compensate for potential exclusion
	if err != nil {
		return nil, nil, fmt.Errorf("load recent messages: %w", err)
	}
	filtered := make([]store.StoredMessage, 0, len(recent))
	for _, m := range recent {
		if m.ID != excludeID {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) > a.contextWindow {
		filtered = filtered[len(filtered)-a.contextWindow:]
	}
	curated := a.curateHistory(filtered)

	// Assemble messages.
	messages := make([]model.Message, 0, len(curated)+2)
	messages = append(messages, model.Message{
		Role:    model.RoleSystem,
		Content: systemPrompt,
	})
	messages = append(messages, curated...)

	// Append user message (may be multimodal).
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)

	// All tools available — LLM decides which to use.
	toolDefs := a.toolRegistry.AllToolDefs()

	return messages, toolDefs, nil
}

// buildSystemPrompt assembles: base prompt + skill catalog + skill accumulation guide.
func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString(a.basePrompt)

	// Inject environment info. Date is always present so the model never thinks it's in the past.
	sb.WriteString(fmt.Sprintf("\n\nToday: %s\n", time.Now().Format("2006-01-02 (Monday)")))
	sb.WriteString(fmt.Sprintf("Home directory: %s\n", os.Getenv("HOME")))
	sb.WriteString(fmt.Sprintf("Workspace directory: %s\n", a.workspaceDir))
	sb.WriteString("The read_file and write_file tools operate on paths relative to this workspace. For files outside the workspace, use the cli tool with absolute paths (e.g. `cat /home/user/file.txt`).\n")
	sb.WriteString("Use the current_time tool when you need to know the date, time, or resolve relative time references.\n")

	// Inject builtin skill prompts directly — these contain critical behavioral rules.
	builtinPrompts := a.skillIndex.BuiltinPrompts()
	if builtinPrompts != "" {
		sb.WriteString(builtinPrompts)
	}

	// Skill catalog for user-created skills.
	catalog := a.skillIndex.Catalog()
	if catalog != "" {
		sb.WriteString("\n")
		sb.WriteString(catalog)
	}

	// Skill accumulation instructions.
	sb.WriteString("\n\n## Skill Accumulation\n\n")
	sb.WriteString("When you successfully complete a new type of recurring task, use the save_skill tool to capture your approach as a reusable skill. ")
	sb.WriteString("A good skill should include: trigger conditions, step-by-step instructions, and which tools to use. Do not create skills for one-off tasks.\n")

	return sb.String()
}

// imageExts lists extensions to re-inject as multimodal content.
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}

// curateHistory filters message history to keep only high-signal content:
// user messages and assistant final replies. Removes tool_call/tool_result noise.
// For user messages referencing images in .files/, re-loads them as multimodal content.
func (a *Agent) curateHistory(messages []store.StoredMessage) []model.Message {
	// Build a lookup of image filenames in .files/ for robust matching.
	imageFiles := a.scanImageFiles()

	var curated []model.Message
	for _, m := range messages {
		switch m.Role {
		case "user":
			// Check if this message references any known image file by name.
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

// scanImageFiles returns a map of filename → absolute path for image files in .files/.
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
