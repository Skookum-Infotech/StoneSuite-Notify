// Package deliveries persists notification_deliveries — the combined work
// queue and delivery log for the reliability layer. One row exists per
// (notification, channel): email/push start pending and are claimed by the
// workers package; in_app is written already-terminal since the
// notifications row itself is the delivery. No address/endpoint is stored
// here — email/push resolve addressing fresh at send time from the
// notifications and push_subscriptions tables, so a retry never acts on
// stale data.
package deliveries

import (
	"encoding/json"
	"time"
)

// Channel identifies which side of a notification a delivery row tracks.
const (
	ChannelEmail = "email"
	ChannelPush  = "push"
	ChannelInApp = "in_app"
)

// Status is the lifecycle state of one delivery row.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusSent       = "sent"
	StatusRetrying   = "retrying"
	StatusFailed     = "failed"
	StatusSkipped    = "skipped"
)

// MaxAttempts is the total number of send attempts (including the first)
// before a delivery is terminated as failed.
const MaxAttempts = 5

// Delivery is one row tracking a single (notification, channel) delivery's
// attempt history.
type Delivery struct {
	ID              string    `json:"id"`
	NotificationID  string    `json:"notificationId"`
	TenantID        string    `json:"tenantId"`
	RecipientUserID string    `json:"recipientUserId"`
	Channel         string    `json:"channel"`
	Status          string    `json:"status"`
	Attempts        int       `json:"attempts"`
	MaxAttempts     int       `json:"maxAttempts"`
	NextAttemptAt   time.Time `json:"nextAttemptAt"`
	LastError       string    `json:"lastError,omitempty"`
	// json.RawMessage (not []byte) so the stored provider payload is
	// embedded as JSON in API responses rather than base64-encoded.
	ProviderResponse json.RawMessage `json:"providerResponse,omitempty"`
	// ProviderStatus is what the email provider reported after accepting the
	// message (delivered, bounced, …); empty until a webhook arrives, and
	// always empty for non-email channels. Distinct from Status, which is the
	// send-queue state.
	ProviderStatus   string     `json:"providerStatus,omitempty"`
	ProviderStatusAt *time.Time `json:"providerStatusAt,omitempty"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}
