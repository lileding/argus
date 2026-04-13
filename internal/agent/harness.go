package agent

import (
	"context"
	"fmt"
	"strings"

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

	// Skill catalog: all skills' name + description, so LLM knows what's available.
	catalog := a.skillIndex.Catalog()
	if catalog != "" {
		sb.WriteString("\n\n")
		sb.WriteString(catalog)
	}

	// Skill accumulation instructions.
	sb.WriteString("\n\n## Skill Accumulation\n\n")
	sb.WriteString("当你成功完成一个新类型的任务，且这种任务可能会反复出现时，使用 save_skill 工具将你的方法沉淀为可复用的 skill。")
	sb.WriteString("好的 skill 应包含：触发条件、处理步骤、使用哪些工具。不要为一次性任务创建 skill。\n")

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
