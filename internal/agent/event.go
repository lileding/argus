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
	Iteration int // orchestrator iteration (0-based)
	Seq       int // sequence within iteration (for parallel calls)
}

type ToolResultPayload struct {
	Name       string
	CallID     string
	Result     string // truncated for UI display
	FullResult string // full result for trace storage
	IsError    bool
	DurationMs int // tool execution time
	Iteration  int
	Seq        int
}

// ComposingPayload carries orchestrator summary stats for trace recording.
type ComposingPayload struct {
	Iterations            int
	Summary               string
	TotalPromptTokens     int
	TotalCompletionTokens int
}

type ReplyPayload struct {
	Text                string
	PromptTokens        int // synthesizer token usage
	CompletionTokens    int
}

// ReplyDeltaPayload carries the accumulated (not incremental) reply text so far.
type ReplyDeltaPayload struct {
	Text string
}

type ErrorPayload struct {
	Err error
}
