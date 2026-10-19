// Package notifications implements the notify service's core domain: a
// per-user, per-tenant feed of events raised elsewhere in StoneSuite
// (invoice approved, quote sent, etc.), consumed by the frontend bell.
package notifications

import "time"

// Notification is one row of a recipient's feed. The row is always
// created regardless of the recipient's in-app preference — VisibleInApp
// controls only whether it appears in the feed/bell, since email and push
// delivery need this row's content (and RecipientEmail) regardless of that
// toggle.
type Notification struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"tenantId"`
	RecipientUserID string     `json:"recipientUserId"`
	RecipientEmail  string     `json:"recipientEmail,omitempty"`
	ActorUserID     string     `json:"actorUserId,omitempty"`
	EventType       string     `json:"eventType"`
	Resource        string     `json:"resource"`
	ResourceID      string     `json:"resourceId"`
	Title           string     `json:"title"`
	Body            string     `json:"body,omitempty"`
	Link            string     `json:"link,omitempty"`
	VisibleInApp    bool       `json:"visibleInApp"`
	ReadAt          *time.Time `json:"readAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

// CreateInput is the payload accepted by the internal create endpoint —
// called by another StoneSuite service when a business event fires.
// RecipientEmail and VisibleInApp are populated by controllers.Handler.Create
// (from the request's recipient address and the resolved in-app preference)
// rather than being required fields a caller must set directly.
type CreateInput struct {
	TenantID        string `json:"tenantId"`
	RecipientUserID string `json:"recipientUserId"`
	RecipientEmail  string `json:"recipientEmail,omitempty"`
	ActorUserID     string `json:"actorUserId,omitempty"`
	EventType       string `json:"eventType"`
	Resource        string `json:"resource"`
	ResourceID      string `json:"resourceId"`
	Title           string `json:"title"`
	Body            string `json:"body,omitempty"`
	Link            string `json:"link,omitempty"`
	// StatusLink is where a delivery-problem alert for this notification's
	// email should send the user: the staff-side page that shows the email's
	// status. Optional; distinct from Link, which for a customer email is the
	// customer's URL.
	StatusLink string `json:"statusLink,omitempty"`
	// StatusResource is the RBAC resource that governs that page ("invoice",
	// "user", "portal_access", …). The alert is stored under it so the bell's
	// accessible_resources filter shows it to the sender; empty falls back to
	// Resource.
	StatusResource string `json:"statusResource,omitempty"`
	VisibleInApp    bool   `json:"visibleInApp"`
}

// AttachmentInput is the optional per-notification content only the email
// delivery worker reads: an attached file, custom HTML to use in place of
// the generic template, or both. Persisted in notification_attachments, not
// on the notification row itself — see that table's doc comment in
// schema.sql.
type AttachmentInput struct {
	FileName      string
	ContentType   string
	Content       []byte
	EmailBodyHTML string
	EmailOptions  EmailOptions
}

// EmailOptions are the per-message header overrides persisted with the
// attachment row and read back by the email delivery worker: a Reply-To that
// routes a customer's reply to a specific record, a human sender name, and the
// threading headers that keep a reply inside its Gmail/Outlook conversation.
// The zero value means "use the service-wide defaults".
type EmailOptions struct {
	ReplyTo    string `json:"replyTo,omitempty"`
	FromName   string `json:"fromName,omitempty"`
	InReplyTo  string `json:"inReplyTo,omitempty"`
	References string `json:"references,omitempty"`
}

// IsZero reports whether no override is set.
func (o EmailOptions) IsZero() bool { return o == EmailOptions{} }

// Paging bounds for the feed. DefaultPageSize matches what the bell
// dropdown asks for; MaxPageSize caps the "view all" screen so no caller
// can pull an unbounded history in one request.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// NormalizePaging clamps a caller-supplied limit/offset to the supported
// bounds. Store and handlers share it so the page size the response reports
// is always the one the query actually used.
func NormalizePaging(limit, offset int) (int, int) {
	if limit <= 0 || limit > MaxPageSize {
		limit = DefaultPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// Validate reports the first missing required field, if any.
func (in CreateInput) Validate() error {
	switch {
	case in.TenantID == "":
		return errRequired("tenantId")
	case in.RecipientUserID == "" && in.RecipientEmail == "":
		return errRequired("recipientUserId or recipientEmail")
	case in.EventType == "":
		return errRequired("eventType")
	case in.Resource == "":
		return errRequired("resource")
	case in.ResourceID == "":
		return errRequired("resourceId")
	case in.Title == "":
		return errRequired("title")
	}
	return nil
}

func errRequired(field string) error {
	return &ValidationError{Field: field}
}

// ValidationError reports a missing/invalid input field.
type ValidationError struct {
	Field string
}

func (e *ValidationError) Error() string {
	return e.Field + " is required"
}
