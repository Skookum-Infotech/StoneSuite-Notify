package controllers

import (
	"encoding/json"
	"log"
	"net/http"

	"stonesuite-notify/audit"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/preferences"
)

// PreferencesHandler exposes the Preference Manager's HTTP endpoints.
type PreferencesHandler struct {
	Store preferences.Store
	Audit audit.Recorder
}

// NewPreferencesHandler builds a PreferencesHandler backed by the given
// store.
func NewPreferencesHandler(store preferences.Store, recorder audit.Recorder) *PreferencesHandler {
	return &PreferencesHandler{Store: store, Audit: recorder}
}

// Get handles GET /api/preferences — the calling user's effective
// preferences (overrides merged onto tenant defaults) plus their raw
// overrides, so a settings page can distinguish "explicitly set" from
// "inherited".
func (h *PreferencesHandler) Get(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	effective, err := h.Store.Resolve(r.Context(), user.TenantID, user.UserID)
	if err != nil {
		log.Printf("preferences: resolve for %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load preferences.")
		return
	}
	overrides, err := h.Store.GetUserOverride(r.Context(), user.TenantID, user.UserID)
	if err != nil {
		log.Printf("preferences: get overrides for %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to load preferences.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"effective": effective, "overrides": overrides},
	})
}

// Update handles PUT /api/preferences — a partial patch to the calling
// user's own overrides; omitted fields keep inheriting the tenant default.
func (h *PreferencesHandler) Update(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	var in preferences.OverrideInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBodyBytes)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	if err := h.Store.SetUserOverride(r.Context(), user.TenantID, user.UserID, in); err != nil {
		log.Printf("preferences: set override for %s: %v", user.UserID, err)
		fail(w, http.StatusInternalServerError, "Failed to update preferences.")
		return
	}

	h.Audit.Record(r.Context(), auditEntry(r, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
		Action:      audit.ActionPreferenceUpdated,
		Resource:    audit.ResourcePreference,
		ResourceID:  user.UserID,
		// Only the fields the request actually set are recorded, so the
		// trail distinguishes "turned email off" from "left it inherited".
		Metadata: audit.Metadata(changedPreferenceFields(in)),
	}))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

// changedPreferenceFields reduces a partial override to just the fields the
// caller set, for the audit entry's metadata.
func changedPreferenceFields(in preferences.OverrideInput) map[string]any {
	changed := map[string]any{}
	if in.EmailEnabled != nil {
		changed["emailEnabled"] = *in.EmailEnabled
	}
	if in.InAppEnabled != nil {
		changed["inAppEnabled"] = *in.InAppEnabled
	}
	if in.PushEnabled != nil {
		changed["pushEnabled"] = *in.PushEnabled
	}
	return changed
}

// tenantDefaultsRequest additionally carries tenantId since this endpoint
// is called service-to-service (by StoneSuite-Backend after it has
// already checked the caller is an admin), not by a logged-in user — there
// is no JWT here to derive tenant scope from.
type tenantDefaultsRequest struct {
	TenantID string `json:"tenantId"`
	preferences.TenantDefaultsInput
}

// SetTenantDefaults handles PUT /api/tenant-defaults — internal-secret
// gated, for StoneSuite-Backend to push an org-wide setting change in.
// The tenant is named in the body because there is no JWT on this call
// path; the caller is trusted to have authorized the change, the same way
// the internal notification-create endpoint is.
func (h *PreferencesHandler) SetTenantDefaults(w http.ResponseWriter, r *http.Request) {
	var req tenantDefaultsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBodyBytes)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if req.TenantID == "" {
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	}

	h.setTenantDefaults(w, r, req.TenantID, req.TenantDefaultsInput, audit.Entry{
		TenantID:  req.TenantID,
		ActorType: audit.ActorService,
	})
}

// AdminGetTenantDefaults handles GET /api/admin/tenant-defaults — a
// signed-in tenant administrator reading their own org's defaults. Gated by
// the preference:admin permission.
func (h *PreferencesHandler) AdminGetTenantDefaults(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	defaults, err := h.Store.GetTenantDefaults(r.Context(), user.TenantID)
	if err != nil {
		log.Printf("preferences: get tenant defaults for %s: %v", user.TenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to load tenant defaults.")
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"defaults": defaults},
	})
}

// AdminSetTenantDefaults handles PUT /api/admin/tenant-defaults — the same
// write as SetTenantDefaults, but for a signed-in administrator. The tenant
// comes from the caller's token rather than the body, so an admin can only
// ever change their own org's defaults.
func (h *PreferencesHandler) AdminSetTenantDefaults(w http.ResponseWriter, r *http.Request) {
	user, err := middleware.GetUserFromContext(r.Context())
	if err != nil {
		fail(w, http.StatusUnauthorized, "Authentication required.")
		return
	}

	var in preferences.TenantDefaultsInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBodyBytes)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	h.setTenantDefaults(w, r, user.TenantID, in, audit.Entry{
		TenantID:    user.TenantID,
		ActorUserID: user.UserID,
		ActorType:   audit.ActorUser,
	})
}

// setTenantDefaults serves both tenant-defaults writers. actor carries
// whichever identity the caller's route established.
func (h *PreferencesHandler) setTenantDefaults(w http.ResponseWriter, r *http.Request, tenantID string, in preferences.TenantDefaultsInput, actor audit.Entry) {
	if err := h.Store.SetTenantDefaults(r.Context(), tenantID, in); err != nil {
		log.Printf("preferences: set tenant defaults for %s: %v", tenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to update tenant defaults.")
		return
	}

	// A tenant-wide default change silently affects every user who has not
	// overridden that channel, so the new value is recorded in full.
	actor.Action = audit.ActionTenantDefaultsUpdated
	actor.Resource = audit.ResourceTenantDefaults
	actor.ResourceID = tenantID
	actor.Metadata = audit.Metadata(map[string]any{
		"emailEnabled": in.EmailEnabled,
		"inAppEnabled": in.InAppEnabled,
		"pushEnabled":  in.PushEnabled,
	})
	h.Audit.Record(r.Context(), auditEntry(r, actor))

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}
