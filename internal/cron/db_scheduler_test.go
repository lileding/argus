package cron

import (
	"testing"
	"time"
)

func TestNextDailyRun(t *testing.T) {
	base := time.Date(2026, 4, 20, 21, 30, 0, 0, time.UTC)
	next, err := NextDailyRun(base, 22, 0, "UTC")
	if err != nil {
		t.Fatalf("NextDailyRun returned error: %v", err)
	}
	want := time.Date(2026, 4, 20, 22, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}

	next, err = NextDailyRun(base, 21, 0, "UTC")
	if err != nil {
		t.Fatalf("NextDailyRun returned error: %v", err)
	}
	want = time.Date(2026, 4, 21, 21, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestNextDailyRunRejectsInvalidTime(t *testing.T) {
	if _, err := NextDailyRun(time.Now(), 24, 0, "UTC"); err == nil {
		t.Fatal("expected invalid hour error")
	}
	if _, err := NextDailyRun(time.Now(), 12, 60, "UTC"); err == nil {
		t.Fatal("expected invalid minute error")
	}
}
