package feishu

import (
	"fmt"
	"log/slog"
	"time"

	"argus/internal/agent"
)

// streamUpdateMinInterval is the minimum time between Feishu card updates during
// streamUpdateMinInterval is the steady-state throttle between card updates
// during synthesizer streaming.
const streamUpdateMinInterval = 500 * time.Millisecond

// streamFirstBurst is the number of initial updates sent without throttling,
// so the user sees the first tokens arrive instantly.
const streamFirstBurst = 3

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
// existingReplyID is the thinking card already sent by the handler (may be "").
// userText is the user's message text (for language detection).
func (a *Adapter) HandleEvents(ch <-chan agent.Event, triggerMessageID, existingReplyID, userText string) {
	lang := detectLang(userText)
	replyMsgID := existingReplyID
	var lastStreamUpdate time.Time
	var streamUpdateCount int

	for ev := range ch {
		switch ev.Type {
		case agent.EventThinking:
			// Handler already sent the thinking card. If it failed (replyMsgID==""),
			// try again here as a fallback.
			if replyMsgID != "" {
				continue
			}
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

		case agent.EventComposing:
			if replyMsgID == "" {
				continue
			}
			cardJSON := ComposingCard(lang)
			if err := a.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("update composing status", "err", err)
			}

		case agent.EventReplyDelta:
			if replyMsgID == "" {
				continue
			}
			streamUpdateCount++
			// First few updates are unthrottled so the user sees tokens instantly.
			// After the burst, throttle to streamUpdateMinInterval.
			if streamUpdateCount > streamFirstBurst &&
				time.Since(lastStreamUpdate) < streamUpdateMinInterval {
				continue
			}
			p := ev.Payload.(agent.ReplyDeltaPayload)
			// Skip LaTeX rendering during streaming — partial $...$ would break.
			// Raw markdown is good enough; final EventReply applies full processing.
			cardJSON := MarkdownToCard(p.Text)
			if err := a.client.UpdateMessage(replyMsgID, cardJSON); err != nil {
				slog.Debug("update stream delta", "err", err)
			}
			lastStreamUpdate = time.Now()

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

// quickDetectLang does a fast language detection on raw Feishu message
// content JSON. Used by the handler to pick the thinking-card language
// before the full message is built. Falls back to "zh" if undetermined
// (the primary user is Chinese).
func quickDetectLang(rawContentJSON string) string {
	for _, r := range rawContentJSON {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	// Default to Chinese for audio/image messages where no text exists yet.
	return "zh"
}
