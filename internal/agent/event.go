package agent

// EventType identifies the kind of agent event.
type EventType int

const (
	EventThinking   EventType = iota // agent started processing
	EventToolCall                     // about to call a tool
	EventToolResult                   // tool returned
	EventReply                        // final text reply
	EventError                        // error occurred
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

type ErrorPayload struct {
	Err error
}
