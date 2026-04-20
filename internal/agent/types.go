package agent

// Frontend is the UI surface for a task. Defined in the agent package
// so the Agent never imports any Frontend implementation.
// Each Task carries its own Frontend reference; the Agent calls
// task.Frontend.SubmitMessage to hand off the event stream.
type Frontend interface {
	SubmitMessage(*Message)
}

// Task is submitted by a Frontend to the Agent via SubmitTask.
// The Agent scheduler routes it to the per-chat FIFO queue.
type Task struct {
	ChatID       string
	MsgID        int64
	TriggerMsgID string
	Lang         string        // pre-detected language ("zh"/"en") for thinking card
	Frontend     Frontend      // callback for UI rendering
	ReadyCh      <-chan Payload // carries processed content when media is ready
}

// Payload is the processed content delivered through Task.ReadyCh.
// It replaces the old chan struct{} signal + DB round-trip pattern.
type Payload struct {
	Content   string
	FilePaths []string
}

// Message is created by the Agent and handed to the Frontend via
// SubmitMessage. The Frontend consumes Events to drive UI updates.
// Closing Events is the sole "this message is done" signal.
type Message struct {
	ChatID       string
	MsgID        int64
	TriggerMsgID string
	Lang         string
	Events       <-chan Event
}
