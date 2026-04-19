package feishu

import "testing"

func TestPresentationLockTryBeginBlockedByActiveSync(t *testing.T) {
	lock := NewPresentationLock()
	lock.Begin("chat-1")

	if lock.TryBegin("chat-1") {
		t.Fatal("TryBegin succeeded while sync presentation was active")
	}
	if !lock.IsActive("chat-1") {
		t.Fatal("expected chat to be active")
	}

	lock.End("chat-1")
	if !lock.TryBegin("chat-1") {
		t.Fatal("TryBegin failed after sync presentation ended")
	}
	lock.End("chat-1")
}

func TestParseChatID(t *testing.T) {
	tests := []struct {
		chatID string
		typ    string
		id     string
	}{
		{"p2p:ou_x", "open_id", "ou_x"},
		{"group:oc_x", "chat_id", "oc_x"},
		{"oc_raw", "chat_id", "oc_raw"},
	}
	for _, tt := range tests {
		typ, id := ParseChatID(tt.chatID)
		if typ != tt.typ || id != tt.id {
			t.Fatalf("ParseChatID(%q) = (%q, %q), want (%q, %q)", tt.chatID, typ, id, tt.typ, tt.id)
		}
	}
}
