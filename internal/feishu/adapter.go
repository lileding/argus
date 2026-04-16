package feishu

import (
	"fmt"
	"log/slog"

	"argus/internal/agent"
)

// MarkdownProcessor processes markdown (e.g. LaTeX rendering) without IM-specific knowledge.
type MarkdownProcessor interface {
	ProcessMarkdown(markdown string) string
}

// Adapter consumes agent events and drives Feishu message updates.
type Adapter struct {
	client    *Client
	processor MarkdownProcessor
}

func NewAdapter(client *Client, processor MarkdownProcessor) *Adapter {
	return &Adapter{client: client, processor: processor}
}

// HandleEvents consumes an agent event stream and updates Feishu messages accordingly.
// triggerMessageID is the incoming Feishu message to reply to.
// userText is the user's message text (for language detection).
func (a *Adapter) HandleEvents(ch <-chan agent.Event, triggerMessageID, userText string) {
	lang := detectLang(userText)
	var replyMsgID string

	for ev := range ch {
		switch ev.Type {
		case agent.EventThinking:
			cardJSON := ThinkingCard(lang)
			id, err := a.client.ReplyRichWithID(triggerMessageID, "interactive", cardJSON)
			if err != nil {
				slog.Error("send thinking card", "err", err)
			} else {
				replyMsgID = id
			}

		case agent.EventToolCall:
			if replyMsgID == "" {
				continue
			}
			p := ev.Payload.(agent.ToolCallPayload)
			cardJSON := ToolStatusCard(p.Name, p.Arguments, lang)
			if err := a.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("update tool status", "err", err)
			}

		case agent.EventReply:
			p := ev.Payload.(agent.ReplyPayload)
			md := a.processor.ProcessMarkdown(p.Text)
			cardJSON := MarkdownToCard(md)
			if replyMsgID != "" {
				if err := a.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
					slog.Error("update final reply", "err", err)
				}
			} else {
				a.client.ReplyRich(triggerMessageID, "interactive", cardJSON)
			}

		case agent.EventError:
			p := ev.Payload.(agent.ErrorPayload)
			cardJSON := MarkdownToCard(fmt.Sprintf("Error: %v", p.Err))
			if replyMsgID != "" {
				a.client.UpdateMessage(replyMsgID, cardJSON)
			} else {
				a.client.ReplyRich(triggerMessageID, "interactive", cardJSON)
			}
		}
	}
}

// detectLang checks if the text is primarily Chinese.
func detectLang(text string) string {
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	return "en"
}
