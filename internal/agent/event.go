package agent

// EventType identifies the kind of agent event.
type EventType int

const (
	EventThinking   EventType = iota // agent started processing
	EventToolCall                    // about to call a tool
	EventToolResult                  // tool returned
	EventComposing                   // orchestrator done, synthesizer starting
	EventReplyDelta                  // partial reply text (streaming synthesis)
	EventReply                       // final text reply
	EventError                       // error occurred
)

// Event is emitted by the agent during processing.
type Event struct {
	Type    EventType
	Payload any
}

type ThinkingPayload struct {
	UserText string
}

type ToolCallPayload struct {
	Name      string
	Arguments string
	CallID    string
}

type ToolResultPayload struct {
	Name    string
	CallID  string
	Result  string // truncated summary
	IsError bool
}

type ReplyPayload struct {
	Text string
}

// ReplyDeltaPayload carries the accumulated (not incremental) reply text so far.
// Downstream consumers receive the full current string — simpler for card updates.
type ReplyDeltaPayload struct {
	Text string
}

type ErrorPayload struct {
	Err error
}
