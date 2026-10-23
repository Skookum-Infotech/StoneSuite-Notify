package inbound

import (
	"testing"
	"time"
)

func TestJobBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{-3, 15 * time.Second}, // never below the first step
		{0, 15 * time.Second},
		{1, 15 * time.Second},
		{2, 30 * time.Second},
		{3, time.Minute},
		{4, 2 * time.Minute},
		{5, 4 * time.Minute},
		{6, 8 * time.Minute},
		{7, 16 * time.Minute},
		{8, 30 * time.Minute}, // 32 minutes, capped
		{9, 30 * time.Minute},
		{50, 30 * time.Minute},
	}
	for _, tc := range tests {
		if got := JobBackoff(tc.attempts); got != tc.want {
			t.Errorf("JobBackoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

func TestAStuckFileIsRetriedForAboutHalfAnHour(t *testing.T) {
	// The sum of the waits before the last allowed attempt: pinned so a change to
	// MaxJobAttempts or the schedule is a conscious one. Every attempt asks Resend
	// for a fresh download link, so this is not bounded by the link's lifetime.
	var total time.Duration
	for attempt := 1; attempt < MaxJobAttempts; attempt++ {
		total += JobBackoff(attempt)
	}
	if total < 20*time.Minute || total > 45*time.Minute {
		t.Fatalf("a job keeps being retried for %v, want between 20 and 45 minutes", total)
	}
}
