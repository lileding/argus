package feishu

import (
	"sync"
	"time"
)

// Dedup tracks seen event IDs to prevent duplicate processing.
// Feishu retries delivery if it doesn't get a 200 within 3 seconds.
type Dedup struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	ttl     time.Duration
	closeCh chan struct{}
}

func NewDedup(ttl time.Duration) *Dedup {
	d := &Dedup{
		seen:    make(map[string]time.Time),
		ttl:     ttl,
		closeCh: make(chan struct{}),
	}
	go d.cleanup()
	return d
}

// IsDuplicate returns true if the event ID has been seen before.
// If not seen, it marks the ID as seen and returns false.
func (d *Dedup) IsDuplicate(eventID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[eventID]; ok {
		return true
	}
	d.seen[eventID] = time.Now()
	return false
}

func (d *Dedup) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.mu.Lock()
			cutoff := time.Now().Add(-d.ttl)
			for id, t := range d.seen {
				if t.Before(cutoff) {
					delete(d.seen, id)
				}
			}
			d.mu.Unlock()
		case <-d.closeCh:
			return
		}
	}
}

func (d *Dedup) Close() {
	close(d.closeCh)
}
