package emailevents

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStatusForEvent(t *testing.T) {
	tests := []struct {
		event  string
		want   string
		wantOK bool
	}{
		{"email.sent", StatusSent, true},
		{"email.delivery_delayed", StatusDelayed, true},
		{"email.delivered", StatusDelivered, true},
		{"email.complained", StatusComplained, true},
		{"email.bounced", StatusBounced, true},
		{"email.failed", StatusFailed, true},
		{"email.suppressed", StatusSuppressed, true},
		{"email.opened", "", false},
		{"email.clicked", "", false},
		{"email.scheduled", "", false},
		{"email.received", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got, ok := StatusForEvent(tc.event)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("StatusForEvent(%q) = (%q, %v), want (%q, %v)", tc.event, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestAdvance(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		incoming    string
		want        string
		wantChanged bool
	}{
		{"first event", "", StatusSent, StatusSent, true},
		{"sent to delivered", StatusSent, StatusDelivered, StatusDelivered, true},
		{"late sent after delivered", StatusDelivered, StatusSent, StatusDelivered, false},
		{"late delayed after delivered", StatusDelivered, StatusDelayed, StatusDelivered, false},
		{"delayed to delivered", StatusDelayed, StatusDelivered, StatusDelivered, true},
		{"delivered to complained", StatusDelivered, StatusComplained, StatusComplained, true},
		{"late delivered after complained", StatusComplained, StatusDelivered, StatusComplained, false},
		{"delivered to bounced", StatusDelivered, StatusBounced, StatusBounced, true},
		{"equal rank keeps the first", StatusBounced, StatusFailed, StatusBounced, false},
		{"equal rank keeps the first (suppressed)", StatusFailed, StatusSuppressed, StatusFailed, false},
		{"same status again", StatusBounced, StatusBounced, StatusBounced, false},
		{"unknown incoming ignored", StatusSent, "bogus", StatusSent, false},
		{"unknown incoming on empty", "", "bogus", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := Advance(tc.current, tc.incoming)
			if got != tc.want || changed != tc.wantChanged {
				t.Fatalf("Advance(%q, %q) = (%q, %v), want (%q, %v)", tc.current, tc.incoming, got, changed, tc.want, tc.wantChanged)
			}
		})
	}
}

func TestIsProblem(t *testing.T) {
	problems := []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed}
	fine := []string{StatusSent, StatusDelivered, "", "bogus"}
	for _, s := range problems {
		if !IsProblem(s) {
			t.Errorf("IsProblem(%q) = false, want true", s)
		}
	}
	for _, s := range fine {
		if IsProblem(s) {
			t.Errorf("IsProblem(%q) = true, want false", s)
		}
	}
}

func TestDeriveState(t *testing.T) {
	tests := []struct {
		name     string
		queue    string
		provider string
		want     string
	}{
		{"provider status wins", "sent", StatusDelivered, "delivered"},
		{"provider delayed is spelled delayed", "sent", StatusDelayed, StateDelayed},
		{"provider bounced", "sent", StatusBounced, "bounced"},
		{"no provider status, pending", "pending", "", StateQueued},
		{"no provider status, processing", "processing", "", StateQueued},
		{"no provider status, sent", "sent", "", "sent"},
		{"no provider status, retrying", "retrying", "", StateRetrying},
		{"no provider status, failed", "failed", "", "failed"},
		{"no provider status, skipped", "skipped", "", StateSkipped},
		{"unrecognised provider status falls back to queue", "sent", "bogus", "sent"},
		{"nothing known", "", "", StateUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveState(tc.queue, tc.provider); got != tc.want {
				t.Fatalf("DeriveState(%q, %q) = %q, want %q", tc.queue, tc.provider, got, tc.want)
			}
		})
	}
}

func TestAlertText(t *testing.T) {
	for _, status := range []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed} {
		t.Run(status, func(t *testing.T) {
			title := AlertTitle(status, "alice@example.com")
			if !strings.Contains(title, "alice@example.com") {
				t.Fatalf("title %q must name the recipient", title)
			}
			if AlertBody(status) == "" {
				t.Fatalf("body for %q is empty", status)
			}
		})
	}

	for _, status := range []string{StatusSent, StatusDelivered, "", "bogus"} {
		if AlertTitle(status, "a@b.com") != "" || AlertBody(status) != "" {
			t.Errorf("non-problem status %q must produce no alert text", status)
		}
	}

	t.Run("empty recipient uses a fallback", func(t *testing.T) {
		if got := AlertTitle(StatusBounced, "  "); !strings.Contains(got, fallbackRecipient) {
			t.Fatalf("title %q must use the fallback recipient", got)
		}
	})

	t.Run("a long recipient keeps the title inside the column width", func(t *testing.T) {
		long := strings.Repeat("x", 400) + "@example.com"
		title := AlertTitle(StatusBounced, long)
		if n := utf8.RuneCountInString(title); n > 200 { // notifications.title is VARCHAR(200)
			t.Fatalf("title is %d characters, want <= 200", n)
		}
	})

	t.Run("body fits its column", func(t *testing.T) {
		for _, status := range []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed} {
			if n := utf8.RuneCountInString(AlertBody(status)); n > 500 { // notifications.body is VARCHAR(500)
				t.Fatalf("body for %q is %d characters, want <= 500", status, n)
			}
		}
	})
}
