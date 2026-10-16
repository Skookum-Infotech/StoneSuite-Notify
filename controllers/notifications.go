// Package controllers wires HTTP handlers to the notifications store.
package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"stonesuite-notify/audit"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/notifications"
	"stonesuite-notify/preferences"
)

// Handler exposes the notification HTTP endpoints against a Store.
type Handler struct {
	Store           notifications.Store
	PreferenceStore preferences.Store
	DeliveryStore   deliveries.Store
	Audit           audit.Recorder
	Config          config.Config
}

// NewHandler builds a Handler backed by the given stores. Config carries the
// email/push provider settings the workers package uses to actually send
// once a delivery is enqueued here.
func NewHandler(store notifications.Store, prefStore preferences.Store, deliveryStore deliveries.Store, recorder audit.Recorder, cfg config.Config) *Handler {
	return &Handler{
		Store:           store,
		PreferenceStore: prefStore,
		DeliveryStore:   deliveryStore,
		Audit:           recorder,
		Config:          cfg,
	}
}

// Summary handles GET /api/notifications/summary — the endpoint the
// frontend bell polls for its unread badge count.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	count, err := h.Store.UnreadCount(r.Context(), user.TenantID, user.UserID, user.AccessibleResources)
	if err != nil {
		log.Printf("notifications: summary for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notification summary.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"unreadCount": count},
	})
}

// List handles GET /api/notifications — the bell dropdown's "latest"
// feed: newest first, optionally unread-only, capped by limit
// (default/max enforced in Store). Use History for the paged "view all"
// screen instead.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	unreadOnly := r.URL.Query().Get("unreadOnly") == "true"
	limit := notifications.DefaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			limit = parsed
		}
	}

	list, err := h.Store.ListForUser(r.Context(), user.TenantID, user.UserID, user.AccessibleResources, unreadOnly, limit, 0)
	if err != nil {
		log.Printf("notifications: list for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"notifications": list},
	})
}

// History handles GET /api/notifications/history — the bell's "view all"
// screen. Same rows as List, but paged and accompanied by a total so the
// client can render page controls.
func (h *Handler) History(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	unreadOnly := r.URL.Query().Get("unreadOnly") == "true"
	page, pageSize := parsePageParams(r.URL.Query(), notifications.DefaultPageSize, notifications.MaxPageSize)

	total, err := h.Store.CountForUser(r.Context(), user.TenantID, user.UserID, user.AccessibleResources, unreadOnly)
	if err != nil {
		log.Printf("notifications: count history for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	list, err := h.Store.ListForUser(r.Context(), user.TenantID, user.UserID, user.AccessibleResources, unreadOnly, pageSize, (page-1)*pageSize)
	if err != nil {
		log.Printf("notifications: list history for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load notifications.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data: map[string]any{
			"notifications": list,
			"pagination":    newPagination(page, pageSize, total),
		},
	})
}

// MarkRead handles POST /api/notifications/{id}/read.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	id := r.PathValue("id")
	if id == "" {
		fail(w, http.StatusBadRequest, "Notification id is required.")
		return
	}

	if err := h.Store.MarkRead(r.Context(), user.TenantID, user.UserID, id); err != nil {
		if errors.Is(err, notifications.ErrNotFound) {
			fail(w, http.StatusNotFound, "Notification not found.")
			return
		}
		log.Printf("notifications: mark read %s for user %s: %v", id, user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to mark notification read.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionNotificationRead,
		Resource:    audit.ResourceNotification,
		ResourceID:  id,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// MarkAllRead handles POST /api/notifications/read-all.
func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	if err := h.Store.MarkAllRead(r.Context(), user.TenantID, user.UserID, user.AccessibleResources); err != nil {
		log.Printf("notifications: mark all read for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to mark notifications read.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionNotificationReadAll,
		Resource:    audit.ResourceNotification,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// recipientTarget is one addressee of a create request. Email is only used
// if "email" is requested in Channels; the caller (e.g. StoneSuite-Backend
// resolving "everyone with role Manager") supplies it since this service
// has no user directory of its own to look addresses up in.
type recipientTarget struct {
	UserID string `json:"userId"`
	Email  string `json:"email,omitempty"`
}

// attachmentInput is the wire shape of one entry in
// createNotificationRequest.Attachments — content travels as base64 over
// JSON, decoded to bytes before building notifications.AttachmentInput.
type attachmentInput struct {
	FileName      string `json:"fileName"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
}

// createNotificationRequest is the payload accepted by the internal create
// endpoint. Multiple recipients let a caller fan a single business event
// out to every user holding a role (role membership is resolved by the
// caller, which already owns that RBAC data) in one request. The in-app
// row is always created; email is opt-in per request via Channels; push is
// attempted automatically for any device the recipient has subscribed.
type createNotificationRequest struct {
	TenantID    string            `json:"tenantId"`
	Recipients  []recipientTarget `json:"recipients"`
	ActorUserID string            `json:"actorUserId,omitempty"`
	EventType   string            `json:"eventType"`
	Resource    string            `json:"resource"`
	ResourceID  string            `json:"resourceId"`
	Title       string            `json:"title"`
	Body        string            `json:"body,omitempty"`
	Link        string            `json:"link,omitempty"`
	Channels    []string          `json:"channels,omitempty"`
	// EmailBodyHTML, when set, is used verbatim as the email's HTML body
	// instead of the generic <h2>title</h2><p>body</p> template — see
	// channels.SendNotificationEmail.
	EmailBodyHTML string `json:"emailBodyHtml,omitempty"`
	// Attachments matches StoneSuite-Backend's services.NotificationRequest
	// wire shape (a plural array under "attachments") — only the first entry
	// is used, since a notification carries at most one file today
	// (notification_attachments is one row per notification_id).
	Attachments []attachmentInput `json:"attachments,omitempty"`
	// IdempotencyKey makes a retried request safe. It is scoped per
	// recipient (key + userId + email) because one request fans out to many
	// notifications; a replay returns the original rows and enqueues nothing.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

const channelEmail = "email"

// Create handles POST /api/notifications/internal — called by another
// StoneSuite service (not an end user) when a business event fires. Gated
// by middleware.RequireInternalSecret rather than end-user auth.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	if req.TenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}
	if len(req.Recipients) == 0 {
		fail(w, http.StatusBadRequest, "recipients is required.")
		return
	}

	var attachment *notifications.AttachmentInput
	if len(req.Attachments) > 0 || req.EmailBodyHTML != "" {
		attachment = &notifications.AttachmentInput{EmailBodyHTML: req.EmailBodyHTML}
		if len(req.Attachments) > 0 {
			a := req.Attachments[0]
			content, decErr := base64.StdEncoding.DecodeString(a.ContentBase64)
			if decErr != nil {
				fail(w, http.StatusBadRequest, "attachments[0].contentBase64 is not valid base64.")
				return
			}
			attachment.FileName = a.FileName
			attachment.ContentType = a.ContentType
			attachment.Content = content
		}
	}

	// Validate every recipient up front so a bad entry rejects the whole
	// request instead of partially creating notifications for some
	// recipients but not others. Preferences are resolved per recipient
	// only after validation passes, so an invalid entry never triggers a
	// wasted preference lookup.
	type plannedCreate struct {
		input notifications.CreateInput
		prefs preferences.Preferences
	}
	planned := make([]plannedCreate, 0, len(req.Recipients))
	for _, recipient := range req.Recipients {
		in := notifications.CreateInput{
			TenantID:        req.TenantID,
			RecipientUserID: recipient.UserID,
			RecipientEmail:  recipient.Email,
			ActorUserID:     req.ActorUserID,
			EventType:       req.EventType,
			Resource:        req.Resource,
			ResourceID:      req.ResourceID,
			Title:           req.Title,
			Body:            req.Body,
			Link:            req.Link,
		}
		if req.IdempotencyKey != "" {
			in.IdempotencyKey = req.IdempotencyKey + ":" + recipient.UserID + ":" + strings.ToLower(recipient.Email)
		}
		if err := in.Validate(); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}

		var prefs preferences.Preferences
		if recipient.UserID == "" {
			// No StoneSuite account to resolve tenant/user preference
			// overrides against — an external recipient always gets
			// email (the only channel they can receive), never in-app
			// (no bell to show it in) or push (no device subscription
			// possible without a user).
			prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: false, PushEnabled: false}
		} else {
			var err error
			prefs, err = h.PreferenceStore.Resolve(r.Context(), req.TenantID, recipient.UserID)
			if err != nil {
				// Fail open: an outage in the preferences table must never
				// block the in-app row, which remains the source of truth.
				log.Printf("notifications: resolve preferences for %s: %v", recipient.UserID, err)
				prefs = preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}
			}
		}
		in.VisibleInApp = prefs.InAppEnabled

		planned = append(planned, plannedCreate{input: in, prefs: prefs})
	}

	wantsEmail := containsChannel(req.Channels, channelEmail)

	// A pure external email send — every recipient is email-only, so there
	// is no in-app row as a fallback — that this service cannot actually
	// deliver (a provider key but no EMAIL_FROM, or no provider at all) is
	// rejected here. The caller then fails loudly right now instead of the
	// customer silently never receiving the message while deliveries retry
	// and go terminal unseen. Mixed / internal requests still go through:
	// their in-app row is the source of truth and a doomed email attempt
	// surfaces in the delivery log.
	if wantsEmail && allExternalRecipients(req.Recipients) && !h.Config.EmailSendable() {
		log.Printf("notifications: rejecting external email request for tenant %s: email channel not sendable (EmailConfigured=%v EmailFrom set=%v)",
			req.TenantID, h.Config.EmailConfigured(), h.Config.EmailFrom != "")
		fail(w, http.StatusServiceUnavailable, "Email delivery is not configured on the notify service.")
		return
	}

	created := make([]*notifications.Notification, 0, len(planned))
	for _, p := range planned {
		n, isNew, err := h.Store.CreateIdempotent(r.Context(), p.input)
		if err != nil {
			log.Printf("notifications: create for recipient %s: %v", p.input.RecipientUserID, err)
			continue
		}
		created = append(created, n)
		if !isNew {
			continue // replay: attachment, deliveries and audit came from the original request
		}
		if attachment != nil && wantsEmail && p.prefs.EmailEnabled {
			if err := h.Store.SaveAttachment(r.Context(), n.ID, *attachment); err != nil {
				log.Printf("notifications: save attachment for %s: %v", n.ID, err)
			}
		}
		h.enqueueDeliveries(r.Context(), *n, p.prefs, wantsEmail)

		// One entry per notification, not per request, so the trail can
		// answer "was this user ever notified about resource X" directly.
		// The actor is the user whose action raised the event, if the
		// calling service named one; the actor type stays "service"
		// because the call itself came over the internal secret.
		h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
			TenantID:    n.TenantID,
			ActorUserID: req.ActorUserID,
			ActorType:   audit.ActorService,
			Action:      audit.ActionNotificationCreated,
			Resource:    audit.ResourceNotification,
			ResourceID:  n.ID,
			Metadata: audit.Metadata(map[string]any{
				"recipientUserId": n.RecipientUserID,
				"recipientEmail":  n.RecipientEmail,
				"eventType":       n.EventType,
				"resource":        n.Resource,
				"resourceId":      n.ResourceID,
				"visibleInApp":    n.VisibleInApp,
				"emailEnabled":    p.prefs.EmailEnabled,
				"pushEnabled":     p.prefs.PushEnabled,
				"emailRequested":  wantsEmail,
			}),
		}))
	}

	if len(created) == 0 {
		fail(w, http.StatusInternalServerError, "Failed to create notification.")
		return
	}

	writeJSON(w, http.StatusCreated, models.APIResponse{
		Success: true,
		Data:    map[string]any{"notifications": created},
	})
}

// Deliveries handles GET /api/notifications/{id}/deliveries — the
// internal-secret-gated support/ops view of the delivery log (status
// history + provider response per channel) for one notification. The
// tenant is named explicitly because there is no JWT on this call path,
// and it is required: without it the lookup would be scoped by
// notification id alone and a leaked id would expose another tenant's log.
func (h *Handler) Deliveries(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenantId")
	if tenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}

	h.deliveries(w, r, tenantID, audit.Entry{
		TenantID:  tenantID,
		ActorType: audit.ActorService,
	})
}

// AdminDeliveries handles GET /api/admin/notifications/{id}/deliveries —
// the same view for a signed-in tenant administrator, gated by the
// notification:admin permission. The tenant comes from the caller's own
// token, never the request, so an admin can only ever read their own
// tenant's delivery log.
func (h *Handler) AdminDeliveries(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	h.deliveries(w, r, user.TenantID, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
	})
}

// deliveries serves both delivery-log routes. actor carries whichever
// identity the caller's route established; this function fills in the rest
// of the audit entry.
func (h *Handler) deliveries(w http.ResponseWriter, r *http.Request, tenantID string, actor audit.Entry) {
	id := r.PathValue("id")
	if id == "" {
		fail(w, http.StatusBadRequest, "Notification id is required.")
		return
	}

	list, err := h.DeliveryStore.ListForNotification(r.Context(), tenantID, id)
	if err != nil {
		log.Printf("notifications: list deliveries for %s: %v", id, err)
		fail(w, http.StatusInternalServerError, "Failed to load delivery log.")
		return
	}

	// Reading a delivery log exposes who was contacted on which channel, so
	// the read itself is auditable.
	actor.Action = audit.ActionDeliveryLogViewed
	actor.Resource = audit.ResourceNotification
	actor.ResourceID = id
	actor.Metadata = audit.Metadata(map[string]any{"deliveryCount": len(list)})
	h.Audit.Record(r.Context(), auditEntry(r, actor))

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"deliveries": list},
	})
}

// knownDeliveryStatuses bounds the ?status= filter on the by-status routes
// so an arbitrary string can't be pushed into the query.
var knownDeliveryStatuses = map[string]bool{
	deliveries.StatusPending: true, deliveries.StatusProcessing: true,
	deliveries.StatusSent: true, deliveries.StatusRetrying: true,
	deliveries.StatusFailed: true, deliveries.StatusSkipped: true,
}

// defaultByStatusWindow is how far back DeliveriesByStatus looks when the
// caller gives no ?since=.
const defaultByStatusWindow = 7 * 24 * time.Hour

// DeliveriesByStatus handles GET /api/deliveries?status=&tenantId=&since=&limit=
// — the internal-secret-gated "which of this tenant's deliveries are in
// state X" view (default: failed). tenantId is required and explicit, same
// as the per-notification internal route, since there is no JWT here.
func (h *Handler) DeliveriesByStatus(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenantId")
	if tenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}
	h.deliveriesByStatus(w, r, tenantID, audit.Entry{TenantID: tenantID, ActorType: audit.ActorService})
}

// AdminDeliveriesByStatus handles GET /api/admin/deliveries?status=&since=&limit=
// — the same view for a signed-in tenant administrator (notification:admin).
// The tenant comes from the caller's token, never the request.
func (h *Handler) AdminDeliveriesByStatus(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	h.deliveriesByStatus(w, r, user.TenantID, audit.Entry{
		TenantID: user.TenantID, ActorUserID: user.UserID, ActorType: audit.ActorUser,
	})
}

// deliveriesByStatus serves both by-status routes.
func (h *Handler) deliveriesByStatus(w http.ResponseWriter, r *http.Request, tenantID string, actor audit.Entry) {
	q := r.URL.Query()

	status := q.Get("status")
	if status == "" {
		status = deliveries.StatusFailed
	}
	if !knownDeliveryStatuses[status] {
		fail(w, http.StatusBadRequest, "Unknown delivery status.")
		return
	}

	since := time.Now().Add(-defaultByStatusWindow)
	if v := q.Get("since"); v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			fail(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp.")
			return
		}
		since = parsed
	}

	limit := 50
	if v := q.Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > deliveries.MaxListByStatusLimit {
		limit = deliveries.MaxListByStatusLimit
	}

	list, err := h.DeliveryStore.ListByStatus(r.Context(), tenantID, status, since, limit)
	if err != nil {
		log.Printf("notifications: list deliveries by status %s for tenant %s: %v", status, tenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to load deliveries.")
		return
	}

	// Same rationale as the per-notification log: this reveals who was
	// contacted on which channel, so the read is auditable.
	actor.Action = audit.ActionDeliveryLogViewed
	actor.Resource = audit.ResourceDelivery
	actor.Metadata = audit.Metadata(map[string]any{"status": status, "deliveryCount": len(list)})
	h.Audit.Record(r.Context(), auditEntry(r, actor))

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"deliveries": list, "status": status},
	})
}

func containsChannel(channelList []string, target string) bool {
	for _, c := range channelList {
		if c == target {
			return true
		}
	}
	return false
}

// allExternalRecipients reports whether every recipient is addressed by
// email only (no StoneSuite user id) — i.e. the request has no in-app
// delivery to fall back on, so a non-sendable email channel means the
// message is undeliverable outright. False for an empty list.
func allExternalRecipients(recipients []recipientTarget) bool {
	if len(recipients) == 0 {
		return false
	}
	for _, r := range recipients {
		if r.UserID != "" {
			return false
		}
	}
	return true
}

// enqueueDeliveries writes the queue/log rows for one already-persisted
// notification. in_app is recorded already-terminal (sent or skipped)
// since the notifications row's existence/visibility is itself the
// delivery; email and push are left pending for the workers package's
// QueueConsumer to pick up. Failures here are logged, never surfaced — the
// in-app row is already the source of truth.
func (h *Handler) enqueueDeliveries(ctx context.Context, n notifications.Notification, prefs preferences.Preferences, wantsEmail bool) {
	inAppStatus := deliveries.StatusSkipped
	if prefs.InAppEnabled {
		inAppStatus = deliveries.StatusSent
	}
	if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelInApp, inAppStatus); err != nil {
		log.Printf("notifications: enqueue in_app delivery log for %s: %v", n.ID, err)
	}

	if wantsEmail && prefs.EmailEnabled {
		if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelEmail, deliveries.StatusPending); err != nil {
			log.Printf("notifications: enqueue email delivery for %s: %v", n.ID, err)
		}
	}

	if prefs.PushEnabled && h.Config.PushConfigured() {
		if _, err := h.DeliveryStore.Enqueue(ctx, n.ID, n.TenantID, n.RecipientUserID, deliveries.ChannelPush, deliveries.StatusPending); err != nil {
			log.Printf("notifications: enqueue push delivery for %s: %v", n.ID, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body models.APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, models.APIResponse{Success: false, Message: message})
}
