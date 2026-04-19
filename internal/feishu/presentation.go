package feishu

import "sync"

// PresentationLock serializes user-visible delivery per chat.
// Sync replies block on Begin/End for the full card lifecycle. Outbox
// delivery uses TryBegin so it only sends when no sync card is active.
type PresentationLock struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewPresentationLock() *PresentationLock {
	return &PresentationLock{locks: make(map[string]*sync.Mutex)}
}

func (l *PresentationLock) Begin(chatID string) {
	if l == nil || chatID == "" {
		return
	}
	l.chatLock(chatID).Lock()
}

func (l *PresentationLock) End(chatID string) {
	if l == nil || chatID == "" {
		return
	}
	l.chatLock(chatID).Unlock()
}

func (l *PresentationLock) TryBegin(chatID string) bool {
	if l == nil || chatID == "" {
		return true
	}
	return l.chatLock(chatID).TryLock()
}

func (l *PresentationLock) IsActive(chatID string) bool {
	if l == nil || chatID == "" {
		return false
	}
	lock := l.chatLock(chatID)
	if lock.TryLock() {
		lock.Unlock()
		return false
	}
	return true
}

func (l *PresentationLock) chatLock(chatID string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	lock := l.locks[chatID]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[chatID] = lock
	}
	return lock
}
