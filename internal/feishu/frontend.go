package feishu

import (
	"fmt"
	"log/slog"
	"time"

	"argus/internal/agent"
)

// streamUpdateMinInterval is the steady-state throttle between card updates
// during synthesizer streaming.
const streamUpdateMinInterval = 500 * time.Millisecond

// streamFirstBurst is the number of initial updates sent without throttling.
const streamFirstBurst = 3

// MarkdownProcessor processes markdown (e.g. LaTeX rendering) without IM-specific knowledge.
type MarkdownProcessor interface {
	ProcessMarkdown(markdown string) string
}

// FeishuFrontend implements agent.Frontend for the Feishu IM.
// It receives Messages from the Agent and drives Feishu card updates.
type FeishuFrontend struct {
	client    *Client
	processor MarkdownProcessor
}

func NewFrontend(client *Client, processor MarkdownProcessor) *FeishuFrontend {
	return &FeishuFrontend{
		client:    client,
		processor: processor,
	}
}

// SubmitMessage is called by the Agent scheduler when a task starts processing.
// It spawns a goroutine that opens a thinking card and consumes events to
// drive Feishu card updates. This is non-blocking — the Agent continues
// immediately after calling this.
func (f *FeishuFrontend) SubmitMessage(msg *agent.Message) {
	go f.renderMessage(msg)
}

// renderMessage consumes the event stream and updates the Feishu card.
// Based on the logic from adapter.go:HandleEvents.
func (f *FeishuFrontend) renderMessage(msg *agent.Message) {
	lang := msg.Lang
	if lang == "" {
		lang = "zh"
	}

	// Open thinking card immediately.
	replyMsgID := ""
	if msg.TriggerMsgID != "" {
		cardJSON := ThinkingCard(lang)
		if id, err := f.client.ReplyRichWithID(msg.TriggerMsgID, "interactive", cardJSON); err != nil {
			slog.Warn("frontend: send thinking card", "msg_id", msg.MsgID, "err", err)
		} else {
			replyMsgID = id
		}
	}

	var lastStreamUpdate time.Time
	var streamUpdateCount int
	gotTerminal := false

	for ev := range msg.Events {
		switch ev.Type {
		case agent.EventThinking:
			// Thinking card already sent above. Retry if it failed.
			if replyMsgID != "" || msg.TriggerMsgID == "" {
				continue
			}
			cardJSON := ThinkingCard(lang)
			if id, err := f.client.ReplyRichWithID(msg.TriggerMsgID, "interactive", cardJSON); err != nil {
				slog.Error("frontend: retry thinking card", "err", err)
			} else {
				replyMsgID = id
			}

		case agent.EventToolCall:
			if replyMsgID == "" {
				continue
			}
			p := ev.Payload.(agent.ToolCallPayload)
			cardJSON := ToolStatusCard(p.Name, p.Arguments, lang)
			if err := f.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("frontend: update tool status", "err", err)
			}

		case agent.EventComposing:
			if replyMsgID == "" {
				continue
			}
			cardJSON := ComposingCard(lang)
			if err := f.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("frontend: update composing", "err", err)
			}

		case agent.EventReplyDelta:
			if replyMsgID == "" {
				continue
			}
			streamUpdateCount++
			if streamUpdateCount > streamFirstBurst &&
				time.Since(lastStreamUpdate) < streamUpdateMinInterval {
				continue
			}
			p := ev.Payload.(agent.ReplyDeltaPayload)
			cardJSON := MarkdownToCard(p.Text)
			if err := f.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("frontend: update stream delta", "err", err)
			}
			lastStreamUpdate = time.Now()

		case agent.EventReply:
			gotTerminal = true
			p := ev.Payload.(agent.ReplyPayload)
			md := f.processor.ProcessMarkdown(p.Text)
			cardJSON := MarkdownToCard(md)
			if replyMsgID != "" {
				if err := f.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
					slog.Error("frontend: update final reply", "err", err)
				}
			} else if msg.TriggerMsgID != "" {
				f.client.ReplyRich(msg.TriggerMsgID, "interactive", cardJSON)
			}

		case agent.EventError:
			gotTerminal = true
			p := ev.Payload.(agent.ErrorPayload)
			cardJSON := MarkdownToCard(fmt.Sprintf("Error: %v", p.Err))
			if replyMsgID != "" {
				f.client.UpdateMessage(replyMsgID, cardJSON)
			} else if msg.TriggerMsgID != "" {
				f.client.ReplyRich(msg.TriggerMsgID, "interactive", cardJSON)
			}
		}
	}

	// Events closed without a terminal event (EventReply/EventError).
	// This happens when a task was already processed by drainReady (duplicate).
	// Dismiss the thinking card so it doesn't hang forever.
	if !gotTerminal && replyMsgID != "" {
		f.client.UpdateMessage(replyMsgID, MarkdownToCard("✓"))
	}
}
