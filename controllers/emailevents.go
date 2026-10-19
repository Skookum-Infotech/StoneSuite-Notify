package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"stonesuite-notify/emailevents"
	"stonesuite-notify/inbound"
	"stonesuite-notify/models"
)

const (
	headerSvixID        = "svix-id"
	headerSvixTimestamp = "svix-timestamp"
	headerSvixSignature = "svix-signature"

	maxWebhookBodyBytes = 1 << 20
	maxStatusRequestIDs = 200
	maxEventDetailRunes = 500

	// inboundTimeout bounds fetching a received mail and forwarding it; it covers
	// the backend's scale-to-zero cold start with room to spare.
	inboundTimeout = 30 * time.Second
)

// uuidPattern guards the status endpoint's ids before they reach a ::uuid cast.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// InboundProcessor handles a received customer reply (inbound.Processor).
type InboundProcessor interface {
	Process(ctx context.Context, ev inbound.Event) (inbound.Outcome, error)
}

// EmailEventsHandler serves the provider-webhook receiver and the internal
// email-status read. Now is injectable so tests can pin the replay window.
type EmailEventsHandler struct {
	Store  emailevents.Store
	Secret string
	Now    func() time.Time
	// Inbound handles email.received events; nil acknowledges and drops them.
	Inbound InboundProcessor
}

// NewEmailEventsHandler builds the handler. An empty secret is allowed: the
// webhook route then answers 503 (see Resend).
func NewEmailEventsHandler(store emailevents.Store, secret string) *EmailEventsHandler {
	return &EmailEventsHandler{Store: store, Secret: secret, Now: time.Now}
}

// WithInbound enables handling of email.received events and returns h.
func (h *EmailEventsHandler) WithInbound(p InboundProcessor) *EmailEventsHandler {
	h.Inbound = p
	return h
}

// resendWebhook is the subset of Resend's webhook payload notify reads. The
// event-specific objects are optional: a missing one just yields no detail.
type resendWebhook struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		EmailID string `json:"email_id"`
		Bounce  *struct {
			Type    string `json:"type"`
			SubType string `json:"subType"`
			Message string `json:"message"`
		} `json:"bounce"`
		Suppressed *struct {
			Message string `json:"message"`
		} `json:"suppressed"`
		Failed *struct {
			Reason string `json:"reason"`
		} `json:"failed"`
	} `json:"data"`
}

// detail extracts the short provider diagnostic, if the event carries one.
func (w resendWebhook) detail() string {
	var d string
	switch {
	case w.Data.Bounce != nil:
		b := w.Data.Bounce
		kind := strings.TrimSpace(b.Type)
		if sub := strings.TrimSpace(b.SubType); sub != "" {
			kind += "/" + sub
		}
		d = strings.TrimSpace(kind + ": " + strings.TrimSpace(b.Message))
		d = strings.TrimPrefix(d, ": ")
	case w.Data.Suppressed != nil:
		d = strings.TrimSpace(w.Data.Suppressed.Message)
	case w.Data.Failed != nil:
		d = strings.TrimSpace(w.Data.Failed.Reason)
	}
	if utf8.RuneCountInString(d) > maxEventDetailRunes {
		d = string([]rune(d)[:maxEventDetailRunes])
	}
	return d
}

// Resend handles POST /api/webhooks/resend — Resend reporting what happened to
// an email after accepting it, and delivering a customer's reply received on the
// reply domain (email.received). It is public (Resend holds no StoneSuite
// credentials); the Svix signature is its only gate, so nothing is read from
// the payload before it verifies.
func (h *EmailEventsHandler) Resend(w http.ResponseWriter, r *http.Request) {
	if h.Secret == "" {
		fail(w, http.StatusServiceUnavailable, "Webhook receiving is not configured.")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	msgID := r.Header.Get(headerSvixID)
	err = emailevents.VerifySignature(h.Secret, msgID, r.Header.Get(headerSvixTimestamp),
		r.Header.Get(headerSvixSignature), body, h.Now())
	switch {
	case errors.Is(err, emailevents.ErrBadSecret):
		log.Printf("emailevents: event=resend_webhook_misconfigured reason=%q", "RESEND_WEBHOOK_SECRET is not a valid whsec_ secret")
		fail(w, http.StatusServiceUnavailable, "Webhook receiving is not configured.")
		return
	case err != nil:
		// Never log the body, the signature headers or the secret.
		log.Printf("security_event=resend_webhook_rejected remote=%s", r.RemoteAddr)
		fail(w, http.StatusBadRequest, "Invalid signature.")
		return
	}

	var evt resendWebhook
	if err := json.Unmarshal(body, &evt); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if evt.Type == inbound.EventEmailReceived {
		h.receiveInbound(w, r, body)
		return
	}
	if _, relevant := emailevents.StatusForEvent(evt.Type); !relevant || evt.Data.EmailID == "" {
		writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
		return
	}

	occurredAt := evt.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = h.Now()
	}
	res, err := h.Store.Apply(r.Context(), emailevents.Event{
		SvixID:          msgID,
		ProviderEmailID: evt.Data.EmailID,
		Type:            evt.Type,
		OccurredAt:      occurredAt,
		Detail:          evt.detail(),
	})
	if err != nil {
		log.Printf("emailevents: apply %s: %v", evt.Type, err)
		fail(w, http.StatusInternalServerError, "Failed to record event.")
		return
	}
	log.Printf("emailevents: event=%s matched=%v duplicate=%v changed=%v alerted=%v status=%s",
		evt.Type, res.Matched, res.Duplicate, res.Changed, res.Alerted, res.Status)

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// receiveInbound hands a verified email.received event to the inbound processor.
// The status code drives Resend's retries: a failure a retry could fix (Resend or
// the backend unreachable or erroring) answers 502; anything else — handled,
// not ours, not set up, or unfixable — answers 200 so the shared endpoint is not
// failed (and eventually disabled by Resend) over mail no retry can file.
func (h *EmailEventsHandler) receiveInbound(w http.ResponseWriter, r *http.Request, body []byte) {
	if h.Inbound == nil {
		log.Printf("inbound: event=email_received_dropped reason=%q",
			"inbound reply forwarding is not configured (REPLY_EMAIL_DOMAIN, BACKEND_URL, RESEND_API_KEY)")
		writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
		return
	}
	ev, err := inbound.ParseEvent(body)
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), inboundTimeout)
	defer cancel()
	outcome, err := h.Inbound.Process(ctx, ev)
	switch {
	case err == nil:
		log.Printf("inbound: event=email_received email=%s outcome=%s", ev.Data.EmailID, outcome)
	case inbound.IsRetryable(err):
		log.Printf("inbound: event=email_received_retry email=%s error=%v", ev.Data.EmailID, err)
		fail(w, http.StatusBadGateway, "Failed to process the inbound email.")
		return
	default:
		log.Printf("inbound: event=email_received_dropped email=%s error=%v", ev.Data.EmailID, err)
	}
	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

type emailStatusRequest struct {
	TenantID        string   `json:"tenantId"`
	NotificationIDs []string `json:"notificationIds"`
}

// emailStatusEntry is one email's status as the backend reads it. Detail is
// the provider's diagnostic: this route is internal-secret gated and the
// backend keeps the detail server-side.
type emailStatusEntry struct {
	State     string    `json:"state"`
	Recipient string    `json:"recipient,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// EmailStatus handles POST /api/deliveries/email-status — the internal-secret
// gated batched read of "what actually happened to these emails". The tenant is
// named explicitly (there is no JWT on this path) and is part of the query, so
// an id from another tenant yields nothing.
func (h *EmailEventsHandler) EmailStatus(w http.ResponseWriter, r *http.Request) {
	var req emailStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	switch {
	case req.TenantID == "":
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	case len(req.NotificationIDs) == 0:
		fail(w, http.StatusBadRequest, "notificationIds is required.")
		return
	case len(req.NotificationIDs) > maxStatusRequestIDs:
		fail(w, http.StatusBadRequest, "notificationIds has too many entries.")
		return
	}
	for _, id := range req.NotificationIDs {
		if !uuidPattern.MatchString(id) {
			fail(w, http.StatusBadRequest, "notificationIds must be UUIDs.")
			return
		}
	}

	rows, err := h.Store.StatusForNotifications(r.Context(), req.TenantID, req.NotificationIDs)
	if err != nil {
		log.Printf("emailevents: read status for tenant %s: %v", req.TenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to load email status.")
		return
	}

	statuses := make(map[string]emailStatusEntry, len(rows))
	for _, row := range rows {
		statuses[row.NotificationID] = emailStatusEntry{
			State:     emailevents.DeriveState(row.QueueStatus, row.ProviderStatus),
			Recipient: row.Recipient,
			Detail:    row.Detail,
			UpdatedAt: row.UpdatedAt,
		}
	}
	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"statuses": statuses},
	})
}
