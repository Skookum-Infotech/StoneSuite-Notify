package controllers

import (
	"encoding/json"
	"log"
	"net/http"

	"stonesuite-notify/audit"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/pushsubs"
)

// PushHandler exposes the Web Push subscription endpoints.
type PushHandler struct {
	Store          pushsubs.Store
	Audit          audit.Recorder
	VAPIDPublicKey string
}

// NewPushHandler builds a PushHandler backed by the given store.
func NewPushHandler(store pushsubs.Store, recorder audit.Recorder, vapidPublicKey string) *PushHandler {
	return &PushHandler{Store: store, Audit: recorder, VAPIDPublicKey: vapidPublicKey}
}

// VAPIDKey handles GET /api/push/vapid-public-key. The public key is not
// secret — the frontend needs it to call PushManager.subscribe() — so this
// route intentionally has no auth requirement.
func (h *PushHandler) VAPIDKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"publicKey": h.VAPIDPublicKey},
	})
}

type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Subscribe handles POST /api/push/subscribe — the frontend calls this
// right after PushManager.subscribe() succeeds, forwarding the browser's
// subscription object verbatim.
func (h *PushHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	var req subscribeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBodyBytes)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if req.Endpoint == "" || req.Keys.P256dh == "" || req.Keys.Auth == "" {
		fail(w, http.StatusBadRequest, "endpoint and keys.p256dh and keys.auth are required.")
		return
	}
	if err := pushsubs.Validate(req.Endpoint, req.Keys.P256dh, req.Keys.Auth); err != nil {
		fail(w, http.StatusBadRequest, "Invalid push subscription: "+err.Error()+".")
		return
	}

	err = h.Store.Save(r.Context(), user.TenantID, user.UserID, pushsubs.SaveInput{
		Endpoint: req.Endpoint,
		P256dh:   req.Keys.P256dh,
		Auth:     req.Keys.Auth,
	})
	if err != nil {
		log.Printf("push: subscribe for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to save push subscription.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionPushSubscribed,
		Resource:    audit.ResourcePushSub,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

type unsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// Unsubscribe handles DELETE /api/push/subscribe.
func (h *PushHandler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	var req unsubscribeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBodyBytes)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if req.Endpoint == "" {
		fail(w, http.StatusBadRequest, "endpoint is required.")
		return
	}

	if err := h.Store.Delete(r.Context(), user.TenantID, user.UserID, req.Endpoint); err != nil {
		log.Printf("push: unsubscribe for user %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to remove push subscription.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionPushUnsubscribed,
		Resource:    audit.ResourcePushSub,
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}
