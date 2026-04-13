package agent

import (
	"context"
	"fmt"
	"os"
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
func (a *Agent) assembleContext(ctx context.Context, chatID, userMessage string) ([]model.Message, []model.ToolDef, error) {
	// Build system prompt with skill catalog.
	systemPrompt := a.buildSystemPrompt()

	// Curate history: keep only user messages + assistant final replies.
	recent, err := a.store.RecentMessages(ctx, chatID, a.contextWindow)
	if err != nil {
		return nil, nil, fmt.Errorf("load recent messages: %w", err)
	}
	curated := curateHistory(recent)

	// Assemble messages.
	messages := make([]model.Message, 0, len(curated)+2)
	messages = append(messages, model.Message{
		Role:    model.RoleSystem,
		Content: systemPrompt,
	})
	messages = append(messages, curated...)
	messages = append(messages, model.Message{
		Role:    model.RoleUser,
		Content: userMessage,
	})

	// All tools available — LLM decides which to use.
	toolDefs := a.toolRegistry.AllToolDefs()

	return messages, toolDefs, nil
}

// buildSystemPrompt assembles: base prompt + skill catalog + skill accumulation guide.
func (a *Agent) buildSystemPrompt() string {
	var sb strings.Builder

	sb.WriteString(a.basePrompt)

	// Inject environment info.
	sb.WriteString(fmt.Sprintf("\n\nCurrent time: %s\n", time.Now().Format("2006-01-02 15:04:05 (Monday)")))
	sb.WriteString(fmt.Sprintf("Home directory: %s\n", os.Getenv("HOME")))
	sb.WriteString(fmt.Sprintf("Workspace directory: %s\n", a.workspaceDir))
	sb.WriteString("The read_file and write_file tools operate on paths relative to this workspace. For files outside the workspace, use the cli tool with absolute paths (e.g. `cat /home/user/file.txt`).\n")

	// Skill catalog: all skills' name + description, so LLM knows what's available.
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

// curateHistory filters message history to keep only high-signal content:
// user messages and assistant final replies. Removes tool_call/tool_result noise.
func curateHistory(messages []store.StoredMessage) []model.Message {
	var curated []model.Message
	for _, m := range messages {
		switch m.Role {
		case "user":
			curated = append(curated, model.Message{
				Role:    model.RoleUser,
				Content: m.Content,
			})
		case "assistant":
			// Only keep final replies (has content, no tool call ID).
			if m.Content != "" && m.ToolCallID == nil {
				curated = append(curated, model.Message{
					Role:    model.RoleAssistant,
					Content: m.Content,
				})
			}
		}
	}
	return curated
}
