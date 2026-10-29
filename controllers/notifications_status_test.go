package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stonesuite-notify/config"
)

func postCreate(t *testing.T, h *Handler, req createNotificationRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Create(rec, r)
	return rec
}

func baseCreate() createNotificationRequest {
	return createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	}
}

func TestCreate_PersistsStatusLinkAndResource(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	req := baseCreate()
	req.StatusLink = "/sales/invoices/inv-1"
	req.StatusResource = "invoice"
	rec := postCreate(t, h, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.inputs) != 1 || store.inputs[0].StatusLink != "/sales/invoices/inv-1" || store.inputs[0].StatusResource != "invoice" {
		t.Fatalf("CreateInput = %+v, want StatusLink and StatusResource to be passed through", store.inputs)
	}
}

func TestCreate_RejectsOverlongStatusFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*createNotificationRequest)
	}{
		{"statusLink over 300 characters", func(r *createNotificationRequest) { r.StatusLink = strings.Repeat("x", 301) }},
		{"statusResource over 64 characters", func(r *createNotificationRequest) { r.StatusResource = strings.Repeat("x", 65) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})
			req := baseCreate()
			tc.mutate(&req)

			rec := postCreate(t, h, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if len(store.rows) != 0 {
				t.Fatalf("store has %d rows, want 0 — nothing is created for a rejected request", len(store.rows))
			}
		})
	}
}

func TestCreate_AcceptsStatusFieldsAtTheLimit(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})
	req := baseCreate()
	req.StatusLink = strings.Repeat("x", 300)
	req.StatusResource = strings.Repeat("y", 64)

	if rec := postCreate(t, h, req); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 at exactly the limits (body: %s)", rec.Code, rec.Body.String())
	}
}
