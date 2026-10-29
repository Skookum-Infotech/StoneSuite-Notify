// Package emailevents records what the email provider (Resend) reports about
// an email after stonesuite-notify has handed it over — delivered, delayed,
// bounced, complained, … — and raises an in-app alert for the sender when it
// goes wrong. The send-queue state lives in package deliveries and is never
// touched here; this package owns the provider lifecycle only.
package emailevents

import (
	"fmt"
	"strings"

	"stonesuite-notify/deliveries"
)

// Provider statuses, stored in notification_deliveries.provider_status.
const (
	StatusSent       = "sent"
	StatusDelayed    = "delivery_delayed"
	StatusDelivered  = "delivered"
	StatusComplained = "complained"
	StatusBounced    = "bounced"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
)

// Client-facing states with no provider status of their own. The rest of the
// client vocabulary reuses the Status* strings; StateDelayed is the client
// spelling of StatusDelayed.
const (
	StateQueued   = "queued"
	StateRetrying = "retrying"
	StateDelayed  = "delayed"
	StateSkipped  = "skipped"
	StateUnknown  = "unknown"
)

// AlertEventType is the notifications.event_type of the in-app alert raised for
// the sender of a problem email.
const AlertEventType = "email.delivery_problem"

const (
	maxAlertRecipientRunes = 120
	fallbackRecipient      = "a recipient"
)

// eventStatuses maps a Resend webhook event type to the provider status it sets.
// Event types not listed (opened, clicked, scheduled, received, …) are ignored.
var eventStatuses = map[string]string{
	"email.sent":             StatusSent,
	"email.delivery_delayed": StatusDelayed,
	"email.delivered":        StatusDelivered,
	"email.complained":       StatusComplained,
	"email.bounced":          StatusBounced,
	"email.failed":           StatusFailed,
	"email.suppressed":       StatusSuppressed,
}

// statusRank orders provider statuses. State only moves to a strictly higher
// rank, so a late or replayed event can never move an email backwards. An empty
// or unrecognised current status ranks 0 (the map's zero value).
var statusRank = map[string]int{
	StatusSent:       10,
	StatusDelayed:    20,
	StatusDelivered:  30,
	StatusComplained: 40,
	StatusBounced:    50,
	StatusFailed:     50,
	StatusSuppressed: 50,
}

// StatusForEvent returns the provider status a Resend event type sets.
func StatusForEvent(eventType string) (string, bool) {
	status, ok := eventStatuses[eventType]
	return status, ok
}

// Advance returns the status after applying incoming to current, and whether
// it changed. Only a strictly higher rank wins; an equal rank keeps the first
// status seen, so a bounce followed by a failure does not alert twice.
func Advance(current, incoming string) (string, bool) {
	incomingRank, ok := statusRank[incoming]
	if !ok {
		return current, false
	}
	if incomingRank > statusRank[current] {
		return incoming, true
	}
	return current, false
}

// IsProblem reports whether status is one the sender should be alerted about.
func IsProblem(status string) bool {
	switch status {
	case StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed:
		return true
	}
	return false
}

// DeriveState returns the single client-facing state for one email: the
// provider status when there is one, otherwise a reading of the send-queue
// status.
func DeriveState(queueStatus, providerStatus string) string {
	if _, known := statusRank[providerStatus]; known {
		if providerStatus == StatusDelayed {
			return StateDelayed
		}
		return providerStatus
	}
	switch queueStatus {
	case deliveries.StatusPending, deliveries.StatusProcessing:
		return StateQueued
	case deliveries.StatusSent:
		return StatusSent
	case deliveries.StatusRetrying:
		return StateRetrying
	case deliveries.StatusFailed:
		return StatusFailed
	case deliveries.StatusSkipped:
		return StateSkipped
	}
	return StateUnknown
}

var alertTitles = map[string]string{
	StatusBounced:    "Email to %s bounced",
	StatusComplained: "Email to %s was marked as spam",
	StatusFailed:     "Email to %s could not be sent",
	StatusSuppressed: "Email to %s was not sent",
	StatusDelayed:    "Email to %s is delayed",
}

var alertBodies = map[string]string{
	StatusBounced:    "The recipient's mail server rejected this email. Check the address and try again.",
	StatusComplained: "The recipient reported this email as spam. Avoid re-sending it to this address.",
	StatusFailed:     "The email could not be sent. Try again, or contact support if the problem continues.",
	StatusSuppressed: "This address is on a suppression list because earlier emails to it bounced or were reported as spam.",
	StatusDelayed:    "The recipient's mail server is slow to accept this email. We will keep trying; no action is needed yet.",
}

// AlertTitle is the client-safe alert headline for a problem status, or "" for
// any other status. The recipient is shortened so the title always fits its
// column. Provider detail is deliberately never part of it.
func AlertTitle(status, recipient string) string {
	format, ok := alertTitles[status]
	if !ok {
		return ""
	}
	return fmt.Sprintf(format, shortRecipient(recipient))
}

// AlertBody is the client-safe alert explanation for a problem status, or ""
// for any other status.
func AlertBody(status string) string {
	return alertBodies[status]
}

func shortRecipient(recipient string) string {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return fallbackRecipient
	}
	runes := []rune(recipient)
	if len(runes) > maxAlertRecipientRunes {
		return string(runes[:maxAlertRecipientRunes]) + "…"
	}
	return recipient
}
