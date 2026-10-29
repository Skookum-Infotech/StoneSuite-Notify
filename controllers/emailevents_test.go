package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"stonesuite-notify/emailevents"
)

var evtSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("test-signing-key-0123456789abcdef"))

// fakeEmailEvents is an in-memory emailevents.Store.
type fakeEmailEvents struct {
	applied    []emailevents.Event
	applyRes   emailevents.ApplyResult
	applyErr   error
	statusRows []emailevents.StatusRow
	statusErr  error
	gotTenant  string
	gotIDs     []string
}

func (f *fakeEmailEvents) Apply(_ context.Context, ev emailevents.Event) (emailevents.ApplyResult, error) {
	f.applied = append(f.applied, ev)
	return f.applyRes, f.applyErr
}

func (f *fakeEmailEvents) StatusForNotifications(_ context.Context, tenantID string, ids []string) ([]emailevents.StatusRow, error) {
	f.gotTenant, f.gotIDs = tenantID, ids
	return f.statusRows, f.statusErr
}

var evtNow = time.Unix(1_790_000_000, 0)

func newEvtHandler(store emailevents.Store, secret string) *EmailEventsHandler {
	h := NewEmailEventsHandler(store, secret)
	h.Now = func() time.Time { return evtNow }
	return h
}

// signedRequest builds a webhook POST signed with secret at the handler's "now".
func signedRequest(t *testing.T, secret string, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(evtNow.Unix(), 10)
	sig, err := emailevents.SignatureHeader(secret, "msg_1", ts, []byte(body))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(body))
	req.Header.Set("svix-id", "msg_1")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", sig)
	return req
}

const bounceBody = `{"type":"email.bounced","created_at":"2026-10-01T12:00:00.000Z","data":{"email_id":"em_1","to":["a@b.com"],"bounce":{"type":"Permanent","subType":"General","message":"mailbox does not exist"}}}`

func TestResendWebhook_ValidBounceIsApplied(t *testing.T) {
	store := &fakeEmailEvents{applyRes: emailevents.ApplyResult{Matched: true, Changed: true, Alerted: true}}
	h := newEvtHandler(store, evtSecret)

	rec := httptest.NewRecorder()
	h.Resend(rec, signedRequest(t, evtSecret, bounceBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.applied) != 1 {
		t.Fatalf("Apply called %d times, want 1", len(store.applied))
	}
	ev := store.applied[0]
	if ev.SvixID != "msg_1" || ev.ProviderEmailID != "em_1" || ev.Type != "email.bounced" {
		t.Fatalf("event = %+v", ev)
	}
	if !ev.OccurredAt.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("OccurredAt = %v, want the payload's created_at", ev.OccurredAt)
	}
	if !strings.Contains(ev.Detail, "mailbox does not exist") || !strings.Contains(ev.Detail, "Permanent") {
		t.Fatalf("Detail = %q, want the bounce type and message", ev.Detail)
	}
}

func TestResendWebhook_Rejections(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		req    func(t *testing.T) *http.Request
		want   int
	}{
		{"no secret configured", "", func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, bounceBody) }, http.StatusServiceUnavailable},
		{"secret is not a whsec_ value", "whsec_!!!", func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, bounceBody) }, http.StatusServiceUnavailable},
		{"missing headers", evtSecret, func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(bounceBody))
		}, http.StatusBadRequest},
		{"body tampered after signing", evtSecret, func(t *testing.T) *http.Request {
			req := signedRequest(t, evtSecret, bounceBody)
			req.Body = io.NopCloser(strings.NewReader(strings.Replace(bounceBody, "email.bounced", "email.delivered", 1)))
			return req
		}, http.StatusBadRequest},
		{"signed with another secret", evtSecret, func(t *testing.T) *http.Request {
			other := "whsec_" + base64.StdEncoding.EncodeToString([]byte("another-signing-key-0123456789ab"))
			return signedRequest(t, other, bounceBody)
		}, http.StatusBadRequest},
		{"stale timestamp", evtSecret, func(t *testing.T) *http.Request {
			ts := strconv.FormatInt(evtNow.Add(-10*time.Minute).Unix(), 10)
			sig, _ := emailevents.SignatureHeader(evtSecret, "msg_1", ts, []byte(bounceBody))
			req := httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(bounceBody))
			req.Header.Set("svix-id", "msg_1")
			req.Header.Set("svix-timestamp", ts)
			req.Header.Set("svix-signature", sig)
			return req
		}, http.StatusBadRequest},
		{"signed but not JSON", evtSecret, func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, "not json") }, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			h := newEvtHandler(store, tc.secret)
			rec := httptest.NewRecorder()
			h.Resend(rec, tc.req(t))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			if len(store.applied) != 0 {
				t.Fatalf("a rejected webhook must not reach the store, got %d Apply calls", len(store.applied))
			}
		})
	}
}

func TestResendWebhook_IgnoredAndUnmatchedAre200(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"opened event", `{"type":"email.opened","created_at":"2026-10-01T12:00:00Z","data":{"email_id":"em_1"}}`},
		{"no email id", `{"type":"email.bounced","created_at":"2026-10-01T12:00:00Z","data":{}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			rec := httptest.NewRecorder()
			newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, tc.body))
			if rec.Code != http.StatusOK || len(store.applied) != 0 {
				t.Fatalf("status = %d with %d Apply calls, want 200 and 0", rec.Code, len(store.applied))
			}
		})
	}

	t.Run("event for an email notify did not send", func(t *testing.T) {
		store := &fakeEmailEvents{applyRes: emailevents.ApplyResult{Matched: false}}
		rec := httptest.NewRecorder()
		newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, bounceBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 so Resend does not retry", rec.Code)
		}
	})
}

func TestResendWebhook_StoreErrorIs500(t *testing.T) {
	store := &fakeEmailEvents{applyErr: errors.New("db down")}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, bounceBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so Resend retries", rec.Code)
	}
}

const (
	notifA = "11111111-1111-1111-1111-111111111111"
	notifB = "22222222-2222-2222-2222-222222222222"
	notifC = "33333333-3333-3333-3333-333333333333"
)

func statusRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, "/api/deliveries/email-status", bytes.NewReader(b))
}

func TestEmailStatus_MapsRowsToClientStates(t *testing.T) {
	store := &fakeEmailEvents{statusRows: []emailevents.StatusRow{
		{NotificationID: notifA, Recipient: "a@b.com", QueueStatus: "sent", ProviderStatus: "delivery_delayed", UpdatedAt: evtNow},
		{NotificationID: notifB, Recipient: "c@d.com", QueueStatus: "pending", UpdatedAt: evtNow},
	}}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, map[string]any{
		"tenantId": "t-1", "notificationIds": []string{notifA, notifB, notifC},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Statuses map[string]struct {
				State     string `json:"state"`
				Recipient string `json:"recipient"`
			} `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Data.Statuses[notifA].State; got != "delayed" {
		t.Errorf("A state = %q, want delayed", got)
	}
	if got := resp.Data.Statuses[notifB].State; got != "queued" {
		t.Errorf("B state = %q, want queued", got)
	}
	if _, present := resp.Data.Statuses[notifC]; present {
		t.Error("an id with no email row must be absent from the map")
	}
	if store.gotTenant != "t-1" || len(store.gotIDs) != 3 {
		t.Errorf("store got tenant %q and %d ids, want t-1 and 3", store.gotTenant, len(store.gotIDs))
	}
}

func TestEmailStatus_Validation(t *testing.T) {
	tooMany := make([]string, 201)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}
	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing tenant", map[string]any{"notificationIds": []string{notifA}}},
		{"no ids", map[string]any{"tenantId": "t-1", "notificationIds": []string{}}},
		{"too many ids", map[string]any{"tenantId": "t-1", "notificationIds": tooMany}},
		{"id is not a uuid", map[string]any{"tenantId": "t-1", "notificationIds": []string{"not-a-uuid"}}},
		{"sql-looking id", map[string]any{"tenantId": "t-1", "notificationIds": []string{"'; DROP TABLE x;--"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			rec := httptest.NewRecorder()
			newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if store.gotIDs != nil {
				t.Fatal("an invalid request must not reach the store")
			}
		})
	}

	t.Run("not JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newEvtHandler(&fakeEmailEvents{}, evtSecret).EmailStatus(rec,
			httptest.NewRequest(http.MethodPost, "/api/deliveries/email-status", strings.NewReader("nope")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestEmailStatus_StoreErrorIs500(t *testing.T) {
	store := &fakeEmailEvents{statusErr: errors.New("db down")}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, map[string]any{
		"tenantId": "t-1", "notificationIds": []string{notifA},
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
