package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"stonesuite-notify/audit"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/middleware"
	"stonesuite-notify/models"
	"stonesuite-notify/notifications"
	"stonesuite-notify/preferences"
	"stonesuite-notify/pushsubs"
)

const testJWTSecret = "test-secret"

// fakeStore is an in-memory notifications.Store used to test handlers
// without a real database.
type fakeStore struct {
	rows             []notifications.Notification
	nextID           int
	forceErr         error
	notFoundErr      bool
	savedAttachments map[string]notifications.AttachmentInput // keyed by notificationID
	byIdempotencyKey map[string]*notifications.Notification   // keyed by tenantID+"|"+key
}

func (f *fakeStore) CreateIdempotent(ctx context.Context, in notifications.CreateInput) (*notifications.Notification, bool, error) {
	if in.IdempotencyKey == "" {
		n, err := f.Create(ctx, in)
		return n, err == nil, err
	}
	k := in.TenantID + "|" + in.IdempotencyKey
	if existing, ok := f.byIdempotencyKey[k]; ok {
		return existing, false, nil
	}
	n, err := f.Create(ctx, in)
	if err != nil {
		return nil, false, err
	}
	if f.byIdempotencyKey == nil {
		f.byIdempotencyKey = map[string]*notifications.Notification{}
	}
	f.byIdempotencyKey[k] = n
	return n, true, nil
}

func (f *fakeStore) Create(_ context.Context, in notifications.CreateInput) (*notifications.Notification, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	f.nextID++
	n := notifications.Notification{
		ID:              itoa(f.nextID),
		TenantID:        in.TenantID,
		RecipientUserID: in.RecipientUserID,
		RecipientEmail:  in.RecipientEmail,
		ActorUserID:     in.ActorUserID,
		EventType:       in.EventType,
		Resource:        in.Resource,
		ResourceID:      in.ResourceID,
		Title:           in.Title,
		Body:            in.Body,
		Link:            in.Link,
		VisibleInApp:    in.VisibleInApp,
		CreatedAt:       time.Now(),
	}
	f.rows = append(f.rows, n)
	return &n, nil
}

func (f *fakeStore) Get(_ context.Context, tenantID, id string) (*notifications.Notification, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	for _, n := range f.rows {
		if n.ID == id && n.TenantID == tenantID {
			return &n, nil
		}
	}
	return nil, notifications.ErrNotFound
}

func (f *fakeStore) ListForUser(_ context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool, limit, offset int) ([]notifications.Notification, error) {
	if f.forceErr != nil {
		return nil, f.forceErr
	}
	matching := f.matching(tenantID, recipientUserID, allowedResources, unreadOnly)

	if offset >= len(matching) {
		return nil, nil
	}
	end := offset + limit
	if end > len(matching) {
		end = len(matching)
	}
	return matching[offset:end], nil
}

func (f *fakeStore) CountForUser(_ context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool) (int, error) {
	if f.forceErr != nil {
		return 0, f.forceErr
	}
	return len(f.matching(tenantID, recipientUserID, allowedResources, unreadOnly)), nil
}

// matching applies the same filter ListForUser and CountForUser share, so a
// paged read and its total can never disagree. An empty allowedResources
// means unrestricted, mirroring the real store's SQL.
func (f *fakeStore) matching(tenantID, recipientUserID string, allowedResources []string, unreadOnly bool) []notifications.Notification {
	var out []notifications.Notification
	for _, n := range f.rows {
		if n.TenantID != tenantID || n.RecipientUserID != recipientUserID {
			continue
		}
		if unreadOnly && n.ReadAt != nil {
			continue
		}
		if len(allowedResources) > 0 && !contains(allowedResources, n.Resource) {
			continue
		}
		out = append(out, n)
	}
	return out
}

func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func (f *fakeStore) UnreadCount(_ context.Context, tenantID, recipientUserID string, allowedResources []string) (int, error) {
	if f.forceErr != nil {
		return 0, f.forceErr
	}
	count := 0
	for _, n := range f.rows {
		if n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			if len(allowedResources) > 0 && !contains(allowedResources, n.Resource) {
				continue
			}
			count++
		}
	}
	return count, nil
}

func (f *fakeStore) MarkRead(_ context.Context, tenantID, recipientUserID, id string) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	for i := range f.rows {
		n := &f.rows[i]
		if n.ID == id && n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			now := time.Now()
			n.ReadAt = &now
			return nil
		}
	}
	return notifications.ErrNotFound
}

func (f *fakeStore) MarkAllRead(_ context.Context, tenantID, recipientUserID string, allowedResources []string) error {
	if f.forceErr != nil {
		return f.forceErr
	}
	now := time.Now()
	for i := range f.rows {
		n := &f.rows[i]
		if n.TenantID == tenantID && n.RecipientUserID == recipientUserID && n.ReadAt == nil {
			if len(allowedResources) > 0 && !contains(allowedResources, n.Resource) {
				continue
			}
			n.ReadAt = &now
		}
	}
	return nil
}

func (f *fakeStore) SaveAttachment(_ context.Context, notificationID string, a notifications.AttachmentInput) error {
	if f.savedAttachments == nil {
		f.savedAttachments = map[string]notifications.AttachmentInput{}
	}
	f.savedAttachments[notificationID] = a
	return nil
}
func (f *fakeStore) GetAttachment(_ context.Context, _, _ string) (*notifications.AttachmentInput, error) {
	return nil, nil
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

// fakePreferencesStore is an in-memory preferences.Store used to test
// Create's preference-gated enqueue behavior without a real database.
// resolved is returned by Resolve for every recipient; use
// newFakePreferencesStore for the common "everything enabled" default.
type fakePreferencesStore struct {
	resolved             preferences.Preferences
	tenantDefaults       preferences.Preferences
	resolveErr           error
	resolveCalls         int
	lastOverride         preferences.OverrideInput
	lastTenantDefaults   preferences.TenantDefaultsInput
	lastTenantDefaultsID string
	setOverrideHook      func(tenantID, userID string)
}

func newFakePreferencesStore() *fakePreferencesStore {
	return &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: true, InAppEnabled: true, PushEnabled: true}}
}

func (f *fakePreferencesStore) Resolve(_ context.Context, _, _ string) (preferences.Preferences, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return preferences.Preferences{}, f.resolveErr
	}
	return f.resolved, nil
}

func (f *fakePreferencesStore) GetUserOverride(_ context.Context, _, _ string) (preferences.OverrideInput, error) {
	return preferences.OverrideInput{}, nil
}

func (f *fakePreferencesStore) SetUserOverride(_ context.Context, tenantID, userID string, in preferences.OverrideInput) error {
	f.lastOverride = in
	if f.setOverrideHook != nil {
		f.setOverrideHook(tenantID, userID)
	}
	return nil
}

func (f *fakePreferencesStore) GetTenantDefaults(_ context.Context, _ string) (preferences.Preferences, error) {
	return f.tenantDefaults, nil
}

func (f *fakePreferencesStore) SetTenantDefaults(_ context.Context, tenantID string, in preferences.TenantDefaultsInput) error {
	f.lastTenantDefaultsID = tenantID
	f.lastTenantDefaults = in
	return nil
}

// fakeRecorder is a synchronous audit.Recorder that keeps entries in memory
// so tests can assert on what was audited without waiting on a goroutine.
type fakeRecorder struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (f *fakeRecorder) Record(_ context.Context, e audit.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
}

func (f *fakeRecorder) find(action string) (audit.Entry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.entries {
		if e.Action == action {
			return e, true
		}
	}
	return audit.Entry{}, false
}

// newTestHandler builds a Handler with a throwaway audit recorder, for the
// tests that don't assert on the audit trail.
func newTestHandler(store notifications.Store, prefStore preferences.Store, deliveryStore deliveries.Store, cfg config.Config) *Handler {
	return NewHandler(store, prefStore, deliveryStore, &fakeRecorder{}, cfg)
}

// fakeDeliveriesStore is an in-memory deliveries.Store used to test
// Create's enqueue behavior without a real database.
type fakeDeliveriesStore struct {
	rows []deliveries.Delivery
}

func (f *fakeDeliveriesStore) Enqueue(_ context.Context, notificationID, tenantID, recipientUserID, channel, status string) (*deliveries.Delivery, error) {
	d := deliveries.Delivery{
		ID: itoa(len(f.rows) + 1), NotificationID: notificationID, TenantID: tenantID,
		RecipientUserID: recipientUserID, Channel: channel, Status: status, MaxAttempts: deliveries.MaxAttempts,
	}
	f.rows = append(f.rows, d)
	return &d, nil
}

func (f *fakeDeliveriesStore) ClaimPending(_ context.Context, _ int) ([]deliveries.Delivery, error) {
	return nil, nil
}

func (f *fakeDeliveriesStore) ClaimDue(_ context.Context, _ int, _ time.Duration) ([]deliveries.Delivery, error) {
	return nil, nil
}

func (f *fakeDeliveriesStore) MarkSent(_ context.Context, _ string, _ []byte) error { return nil }

func (f *fakeDeliveriesStore) MarkSkipped(_ context.Context, _ string, _ []byte) error { return nil }

func (f *fakeDeliveriesStore) MarkRetrying(_ context.Context, _ string, _ int, _ string, _ []byte) error {
	return nil
}

func (f *fakeDeliveriesStore) ListForNotification(_ context.Context, tenantID, notificationID string) ([]deliveries.Delivery, error) {
	var out []deliveries.Delivery
	for _, d := range f.rows {
		if d.TenantID == tenantID && d.NotificationID == notificationID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDeliveriesStore) ListByStatus(_ context.Context, tenantID, status string, _ time.Time, limit int) ([]deliveries.Delivery, error) {
	out := []deliveries.Delivery{}
	for _, d := range f.rows {
		if d.TenantID == tenantID && d.Status == status {
			out = append(out, d)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeDeliveriesStore) find(channel string) (deliveries.Delivery, bool) {
	for _, d := range f.rows {
		if d.Channel == channel {
			return d, true
		}
	}
	return deliveries.Delivery{}, false
}

// fakePushStore is an in-memory pushsubs.Store used so Create/dispatch
// tests don't need a real database or network access.
type fakePushStore struct {
	subs []pushsubs.Subscription
}

func (f *fakePushStore) Save(_ context.Context, tenantID, userID string, in pushsubs.SaveInput) error {
	f.subs = append(f.subs, pushsubs.Subscription{
		TenantID: tenantID, UserID: userID,
		Endpoint: in.Endpoint, P256dh: in.P256dh, Auth: in.Auth,
	})
	return nil
}

func (f *fakePushStore) Delete(_ context.Context, tenantID, userID, endpoint string) error {
	out := f.subs[:0]
	for _, s := range f.subs {
		if s.TenantID == tenantID && s.UserID == userID && s.Endpoint == endpoint {
			continue
		}
		out = append(out, s)
	}
	f.subs = out
	return nil
}

func (f *fakePushStore) ListForUser(_ context.Context, tenantID, userID string) ([]pushsubs.Subscription, error) {
	var out []pushsubs.Subscription
	for _, s := range f.subs {
		if s.TenantID == tenantID && s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

// authedRequest builds a request carrying a valid JWT for the given tenant
// and user, run through the real RequireAuth middleware so tests exercise
// the same context-population path production traffic does.
func authedRequest(t *testing.T, method, target, tenantID, userID string, body []byte, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	return authedRequestWithPath(t, method, target, tenantID, userID, nil, body, next)
}

// authedRequestWithPath is authedRequest for routes with path wildcards
// (e.g. "/api/notifications/{id}/read"), which httptest does not populate
// on its own.
func authedRequestWithPath(t *testing.T, method, target, tenantID, userID string, pathValues map[string]string, body []byte, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	// Matches StoneSuite-Backend's real generateTenantJWT shape: "id" is the
	// only per-user identifier — there is no separate "user_id" claim.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id":        userID,
		"email":     "user@example.com",
		"tenant_id": tenantID,
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reqBody)
	req.Header.Set("Authorization", "Bearer "+signed)
	for key, value := range pathValues {
		req.SetPathValue(key, value)
	}

	rec := httptest.NewRecorder()
	middleware.RequireAuth(testJWTSecret)(next).ServeHTTP(rec, req)
	return rec
}

// authedRequestWithResources is authedRequest for tests exercising the
// "accessible_resources" claim (resource-scoped notification visibility). A
// nil/empty accessibleResources omits the claim entirely, matching a token
// minted before StoneSuite-Backend supports it.
func authedRequestWithResources(t *testing.T, method, target, tenantID, userID string, accessibleResources []string, next http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	claims := jwt.MapClaims{
		"id":        userID,
		"email":     "user@example.com",
		"tenant_id": tenantID,
		"exp":       time.Now().Add(time.Hour).Unix(),
	}
	if len(accessibleResources) > 0 {
		claims["accessible_resources"] = accessibleResources
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	req := httptest.NewRequest(method, target, bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+signed)

	rec := httptest.NewRecorder()
	middleware.RequireAuth(testJWTSecret)(next).ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) models.APIResponse {
	t.Helper()
	var resp models.APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func TestSummary_ReturnsUnreadCountForCaller(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1"},
		{ID: "3", TenantID: "t1", RecipientUserID: "u2"}, // different user — must not be counted
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications/summary", "t1", "u1", nil, h.Summary)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeResponse(t, rec)
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("data is not a map: %#v", resp.Data)
	}
	if data["unreadCount"] != float64(2) {
		t.Fatalf("unreadCount = %v, want 2", data["unreadCount"])
	}
}

func TestList_ScopesToCallerOnly(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Title: "mine"},
		{ID: "2", TenantID: "t1", RecipientUserID: "other-user", Title: "not mine"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications", "t1", "u1", nil, h.List)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["notifications"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d notifications, want 1 (must not leak other users' rows)", len(list))
	}
}

func TestList_UnrestrictedTokenSeesAllResources(t *testing.T) {
	// A token with no accessible_resources claim (StoneSuite-Backend not
	// yet minting it for this user/role) must keep seeing everything.
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Resource: "estimate"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequestWithResources(t, http.MethodGet, "/api/notifications", "t1", "u1", nil, h.List)

	resp := decodeResponse(t, rec)
	list := resp.Data.(map[string]any)["notifications"].([]any)
	if len(list) != 2 {
		t.Fatalf("got %d notifications, want 2 (unrestricted token must see every resource)", len(list))
	}
}

func TestList_RestrictedTokenOnlySeesAllowedResource(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Resource: "estimate"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequestWithResources(t, http.MethodGet, "/api/notifications", "t1", "u1", []string{"estimate"}, h.List)

	resp := decodeResponse(t, rec)
	list := resp.Data.(map[string]any)["notifications"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d notifications, want 1 (must not leak salesorder rows to an estimate-only caller)", len(list))
	}
	if got := list[0].(map[string]any)["resource"]; got != "estimate" {
		t.Fatalf("resource = %v, want estimate", got)
	}
}

func TestHistory_TotalMatchesRestrictedResource(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Resource: "estimate"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
		{ID: "3", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequestWithResources(t, http.MethodGet, "/api/notifications/history", "t1", "u1", []string{"salesorder"}, h.History)

	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["notifications"].([]any)
	pagination := data["pagination"].(map[string]any)
	if len(list) != 2 {
		t.Fatalf("got %d notifications, want 2", len(list))
	}
	if pagination["total"] != float64(2) {
		t.Fatalf("pagination total = %v, want 2 (must match the restricted row count, not all rows)", pagination["total"])
	}
}

func TestSummary_UnreadCountRespectsAccessibleResources(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Resource: "estimate"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequestWithResources(t, http.MethodGet, "/api/notifications/summary", "t1", "u1", []string{"estimate"}, h.Summary)

	resp := decodeResponse(t, rec)
	if got := resp.Data.(map[string]any)["unreadCount"]; got != float64(1) {
		t.Fatalf("unreadCount = %v, want 1 (salesorder row must not count for an estimate-only caller)", got)
	}
}

func TestMarkAllRead_DoesNotMarkRestrictedResourceRead(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "1", TenantID: "t1", RecipientUserID: "u1", Resource: "estimate"},
		{ID: "2", TenantID: "t1", RecipientUserID: "u1", Resource: "salesorder"},
	}}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	authedRequestWithResources(t, http.MethodPost, "/api/notifications/read-all", "t1", "u1", []string{"estimate"}, h.MarkAllRead)

	for _, n := range store.rows {
		wantRead := n.Resource == "estimate"
		if isRead := n.ReadAt != nil; isRead != wantRead {
			t.Fatalf("row %s (%s): read = %v, want %v", n.ID, n.Resource, isRead, wantRead)
		}
	}
}

func TestMarkRead_UnknownIDReturns404(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/notifications/does-not-exist/read", nil)
	req.SetPathValue("id", "does-not-exist")

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id": "u1", "email": "user@example.com",
		"tenant_id": "t1",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	signed, _ := token.SignedString([]byte(testJWTSecret))
	req.Header.Set("Authorization", "Bearer "+signed)

	rec := httptest.NewRecorder()
	middleware.RequireAuth(testJWTSecret)(http.HandlerFunc(h.MarkRead)).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreate_RejectsMissingRequiredFields(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(map[string]string{"tenantId": "t1"}) // missing everything else
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreate_PersistsValidNotification(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
}

func TestCreate_MultipleRecipients_CreatesOneRowEach(t *testing.T) {
	// Simulates a caller fanning a single business event out to every user
	// holding a role (e.g. all "Managers") in one request.
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID: "t1",
		Recipients: []recipientTarget{
			{UserID: "u1", Email: "u1@example.com"},
			{UserID: "u2", Email: "u2@example.com"},
		},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 2 {
		t.Fatalf("store has %d rows, want 2 (one per recipient)", len(store.rows))
	}
}

func TestCreate_RejectsRecipientMissingBothUserIDAndEmail(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{}},
		EventType:  "quote.sent",
		Resource:   "quote",
		ResourceID: "q-1",
		Title:      "Quote Q-1 sent",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 0 {
		t.Fatalf("store has %d rows, want 0 (invalid recipient must reject whole request)", len(store.rows))
	}
}

func TestCreate_AcceptsRecipientWithEmailOnly(t *testing.T) {
	store := &fakeStore{}
	delivStore := &fakeDeliveriesStore{}
	// A sendable email config: this test exercises email-only recipient
	// addressing, not the unsendable-channel guard.
	h := newTestHandler(store, newFakePreferencesStore(), delivStore,
		config.Config{ResendAPIKey: "re_x", EmailFrom: "no-reply@stonesuite.app"})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "customer@example.com"}},
		EventType:  "document.sent",
		Resource:   "salesorder",
		ResourceID: "so-1",
		Title:      "Sales Order SO-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
	n := store.rows[0]
	if n.RecipientUserID != "" {
		t.Fatalf("RecipientUserID = %q, want empty for an email-only recipient", n.RecipientUserID)
	}
	if n.RecipientEmail != "customer@example.com" {
		t.Fatalf("RecipientEmail = %q, want customer@example.com", n.RecipientEmail)
	}
	if n.VisibleInApp {
		t.Fatalf("VisibleInApp = true, want false (no user to show a bell to)")
	}
	emailDelivery, ok := delivStore.find(deliveries.ChannelEmail)
	if !ok {
		t.Fatalf("no email delivery row enqueued for the email-only recipient")
	}
	if emailDelivery.Status != deliveries.StatusPending {
		t.Fatalf("email delivery status = %q, want %q", emailDelivery.Status, deliveries.StatusPending)
	}
}

func TestCreate_EmailOnlyRecipient_SkipsPreferenceResolve(t *testing.T) {
	store := &fakeStore{}
	prefs := newFakePreferencesStore()
	h := newTestHandler(store, prefs, &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{Email: "customer@example.com"}},
		EventType:  "document.sent",
		Resource:   "salesorder",
		ResourceID: "so-1",
		Title:      "Sales Order SO-1 sent",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if prefs.resolveCalls != 0 {
		t.Fatalf("PreferenceStore.Resolve was called %d times for an email-only recipient, want 0", prefs.resolveCalls)
	}
}

func TestCreate_UserRecipient_StillResolvesPreferences(t *testing.T) {
	store := &fakeStore{}
	prefs := newFakePreferencesStore()
	h := newTestHandler(store, prefs, &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if prefs.resolveCalls != 1 {
		t.Fatalf("PreferenceStore.Resolve was called %d times for a real user, want 1", prefs.resolveCalls)
	}
}

func TestCreate_EmailBodyHTML_PersistedAsAttachment(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{},
		config.Config{ResendAPIKey: "re_x", EmailFrom: "no-reply@stonesuite.app"})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:      "t1",
		Recipients:    []recipientTarget{{Email: "customer@example.com"}},
		EventType:     "document.sent",
		Resource:      "salesorder",
		ResourceID:    "so-1",
		Title:         "Sales Order SO-1 sent",
		Channels:      []string{"email"},
		EmailBodyHTML: "<p>Branded body.</p>",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1", len(store.rows))
	}
	saved, ok := store.savedAttachments[store.rows[0].ID]
	if !ok {
		t.Fatalf("no attachment saved for notification %s", store.rows[0].ID)
	}
	if saved.EmailBodyHTML != "<p>Branded body.</p>" {
		t.Fatalf("saved EmailBodyHTML = %q, want %q", saved.EmailBodyHTML, "<p>Branded body.</p>")
	}
}

func TestCreate_InAppDisabled_RowStillWrittenButHiddenAndDeliveryLogSkipped(t *testing.T) {
	store := &fakeStore{}
	prefs := &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: true, InAppEnabled: false, PushEnabled: true}}
	delivStore := &fakeDeliveriesStore{}
	h := newTestHandler(store, prefs, delivStore, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("store has %d rows, want 1 (row must always be written, regardless of in-app preference)", len(store.rows))
	}
	if store.rows[0].VisibleInApp {
		t.Fatalf("VisibleInApp = true, want false")
	}
	inAppDelivery, ok := delivStore.find(deliveries.ChannelInApp)
	if !ok {
		t.Fatalf("no in_app delivery log row written")
	}
	if inAppDelivery.Status != deliveries.StatusSkipped {
		t.Fatalf("in_app delivery status = %q, want %q", inAppDelivery.Status, deliveries.StatusSkipped)
	}
}

func TestCreate_EmailRequestedButPreferenceDisabled_NotEnqueued(t *testing.T) {
	store := &fakeStore{}
	prefs := &fakePreferencesStore{resolved: preferences.Preferences{EmailEnabled: false, InAppEnabled: true, PushEnabled: true}}
	delivStore := &fakeDeliveriesStore{}
	h := newTestHandler(store, prefs, delivStore, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1", Email: "u1@example.com"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, ok := delivStore.find(deliveries.ChannelEmail); ok {
		t.Fatalf("email delivery was enqueued despite the recipient's preference disabling it")
	}
}

func TestCreate_EmailRequestedAndEnabled_EnqueuedPending(t *testing.T) {
	store := &fakeStore{}
	delivStore := &fakeDeliveriesStore{}
	h := newTestHandler(store, newFakePreferencesStore(), delivStore, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1", Email: "u1@example.com"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
		Channels:   []string{"email"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	emailDelivery, ok := delivStore.find(deliveries.ChannelEmail)
	if !ok {
		t.Fatalf("no email delivery row enqueued")
	}
	if emailDelivery.Status != deliveries.StatusPending {
		t.Fatalf("email delivery status = %q, want %q", emailDelivery.Status, deliveries.StatusPending)
	}
}

func TestCreate_PushEnabledAndConfigured_EnqueuedPending(t *testing.T) {
	store := &fakeStore{}
	delivStore := &fakeDeliveriesStore{}
	cfg := config.Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}
	h := newTestHandler(store, newFakePreferencesStore(), delivStore, cfg)

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	pushDelivery, ok := delivStore.find(deliveries.ChannelPush)
	if !ok {
		t.Fatalf("no push delivery row enqueued despite push being enabled and configured")
	}
	if pushDelivery.Status != deliveries.StatusPending {
		t.Fatalf("push delivery status = %q, want %q", pushDelivery.Status, deliveries.StatusPending)
	}
}

func TestCreate_PushNotConfigured_NotEnqueued(t *testing.T) {
	store := &fakeStore{}
	delivStore := &fakeDeliveriesStore{}
	h := newTestHandler(store, newFakePreferencesStore(), delivStore, config.Config{}) // no VAPID keys

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if _, ok := delivStore.find(deliveries.ChannelPush); ok {
		t.Fatalf("push delivery was enqueued despite push not being configured")
	}
}

func TestCreate_PreferenceResolveError_FailsOpenAndStillCreates(t *testing.T) {
	store := &fakeStore{}
	prefs := &fakePreferencesStore{resolveErr: errors.New("db down")}
	h := newTestHandler(store, prefs, &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a preferences outage must not block notification creation)", rec.Code)
	}
	if len(store.rows) != 1 || !store.rows[0].VisibleInApp {
		t.Fatalf("expected the row to be created and visible (fail-open default), got rows=%+v", store.rows)
	}
}

// deliveryLogFixture is the delivery-log set shared by the tests below: two
// rows for n1 in tenant t1, one for a different notification, and one for
// the same notification id in a different tenant.
func deliveryLogFixture() *fakeDeliveriesStore {
	return &fakeDeliveriesStore{rows: []deliveries.Delivery{
		{ID: "d1", NotificationID: "n1", TenantID: "t1", Channel: deliveries.ChannelEmail, Status: deliveries.StatusSent},
		{ID: "d2", NotificationID: "n1", TenantID: "t1", Channel: deliveries.ChannelPush, Status: deliveries.StatusFailed},
		{ID: "d3", NotificationID: "other-notification", TenantID: "t1", Channel: deliveries.ChannelEmail, Status: deliveries.StatusSent},
		{ID: "d4", NotificationID: "n1", TenantID: "other-tenant", Channel: deliveries.ChannelEmail, Status: deliveries.StatusSent},
	}}
}

func TestHandler_Create_WithAttachment_SavesIt(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body := `{
		"tenantId": "t1",
		"recipients": [{"userId": "u1", "email": "u1@example.com"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent",
		"channels": ["email"],
		"attachments": [{"fileName": "INV-1.pdf", "contentType": "application/pdf", "contentBase64": "JVBERi0xLjQ="}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(store.savedAttachments) != 1 {
		t.Fatalf("expected exactly 1 saved attachment, got %d", len(store.savedAttachments))
	}
	for _, a := range store.savedAttachments {
		if a.FileName != "INV-1.pdf" {
			t.Fatalf("expected fileName INV-1.pdf, got %q", a.FileName)
		}
		if string(a.Content) != "%PDF-1.4" {
			t.Fatalf("expected decoded content %q, got %q", "%PDF-1.4", a.Content)
		}
	}
}

func TestHandler_Create_ExternalEmailRequest_RejectedWhenEmailNotSendable(t *testing.T) {
	// The document-send-to-customer shape: email-only recipient, "email"
	// channel, and a service that can't actually deliver (provider key but
	// no EMAIL_FROM). Must fail loudly now, not queue a delivery that only
	// ever 422s while the sender sees "sent".
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{},
		config.Config{ResendAPIKey: "re_x"}) // no EmailFrom => not sendable

	body := `{
		"tenantId": "t1",
		"recipients": [{"email": "customer@example.com"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent",
		"channels": ["email"],
		"emailBodyHtml": "<p>branded</p>"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 0 {
		t.Fatalf("no notification row should be created on a hard reject, got %d", len(store.rows))
	}
}

func TestHandler_Create_ExternalEmailRequest_AllowedWhenSendable(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{},
		config.Config{ResendAPIKey: "re_x", EmailFrom: "StoneSuite <no-reply@stonesuite.app>"})

	body := `{
		"tenantId": "t1",
		"recipients": [{"email": "customer@example.com"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent",
		"channels": ["email"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Create_InternalRecipient_NotBlockedByUnsendableEmail(t *testing.T) {
	// A recipient with a userId has an in-app row as the source of truth, so
	// an unsendable email channel must not sink the whole notification —
	// only the external-only shape is hard-rejected.
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{},
		config.Config{ResendAPIKey: "re_x"}) // not sendable

	body := `{
		"tenantId": "t1",
		"recipients": [{"userId": "u1", "email": "u1@example.com"}],
		"eventType": "invoice.approved",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 approved",
		"channels": ["email"]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandler_Create_NoAttachment_SavesNothing(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body := `{
		"tenantId": "t1",
		"recipients": [{"userId": "u1"}],
		"eventType": "document.sent",
		"resource": "invoice",
		"resourceId": "inv-1",
		"title": "Invoice INV-1 sent"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(store.savedAttachments) != 0 {
		t.Fatalf("expected no saved attachments, got %d", len(store.savedAttachments))
	}
}

func TestDeliveries_ReturnsLogForNotification(t *testing.T) {
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/n1/deliveries?tenantId=t1", nil)
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()

	h.Deliveries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["deliveries"].([]any)
	if len(list) != 2 {
		t.Fatalf("got %d deliveries, want 2 (must not leak other notifications' or tenants' rows)", len(list))
	}
}

func TestDeliveries_RequiresTenantID(t *testing.T) {
	// Without a tenant the lookup would be scoped by notification id alone,
	// so a leaked id would expose another tenant's delivery log.
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/notifications/n1/deliveries", nil)
	req.SetPathValue("id", "n1")
	rec := httptest.NewRecorder()

	h.Deliveries(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAdminDeliveries_ScopesToCallersOwnTenant(t *testing.T) {
	// The admin route takes the tenant from the caller's token, so a
	// notification id belonging to another tenant returns nothing even
	// though a row with that id exists.
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	rec := authedRequestWithPath(t, http.MethodGet, "/api/admin/notifications/n1/deliveries",
		"other-tenant", "admin-user", map[string]string{"id": "n1"}, nil, h.AdminDeliveries)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["deliveries"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d deliveries, want 1 (only other-tenant's own row)", len(list))
	}
}

func TestDeliveriesByStatus_Internal_DefaultsToFailedAndScopesToTenant(t *testing.T) {
	// deliveryLogFixture has one failed row (d2, tenant t1) and one failed-
	// looking row in another tenant is absent — d4 is 'sent'. Only t1's
	// failed row should come back.
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/deliveries?tenantId=t1", nil)
	rec := httptest.NewRecorder()
	h.DeliveriesByStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	if data["status"] != deliveries.StatusFailed {
		t.Fatalf("status echoed = %v, want failed (the default)", data["status"])
	}
	list := data["deliveries"].([]any)
	if len(list) != 1 {
		t.Fatalf("got %d deliveries, want 1 (t1's single failed row)", len(list))
	}
}

func TestDeliveriesByStatus_Internal_RequiresTenantID(t *testing.T) {
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/api/deliveries?status=failed", nil)
	rec := httptest.NewRecorder()
	h.DeliveriesByStatus(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (tenantId is mandatory on the internal route)", rec.Code)
	}
}

func TestDeliveriesByStatus_RejectsUnknownStatusAndBadSince(t *testing.T) {
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	for _, tc := range []struct{ name, query string }{
		{"unknown status", "/api/deliveries?tenantId=t1&status=exploded"},
		{"bad since", "/api/deliveries?tenantId=t1&since=last-tuesday"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			rec := httptest.NewRecorder()
			h.DeliveriesByStatus(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestAdminDeliveriesByStatus_ScopesToCallersOwnTenant(t *testing.T) {
	// The admin route ignores any tenant in the request and uses the token's.
	h := newTestHandler(&fakeStore{}, newFakePreferencesStore(), deliveryLogFixture(), config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/admin/deliveries?status=sent&tenantId=t1",
		"other-tenant", "admin-user", nil, h.AdminDeliveriesByStatus)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["deliveries"].([]any)
	// deliveryLogFixture: only d4 is (tenant other-tenant, status sent).
	if len(list) != 1 {
		t.Fatalf("got %d deliveries, want 1 (other-tenant's own sent row, not t1's)", len(list))
	}
}

func TestHistory_PagesThroughFeedWithTotal(t *testing.T) {
	store := &fakeStore{}
	for i := 0; i < 25; i++ {
		store.rows = append(store.rows, notifications.Notification{
			ID: itoa(i + 1), TenantID: "t1", RecipientUserID: "u1", VisibleInApp: true,
		})
	}
	// A row belonging to someone else must not affect the caller's total.
	store.rows = append(store.rows, notifications.Notification{ID: "x", TenantID: "t1", RecipientUserID: "u2"})

	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications/history?page=2&pageSize=10", "t1", "u1", nil, h.History)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)

	list := data["notifications"].([]any)
	if len(list) != 10 {
		t.Fatalf("got %d notifications, want 10 (page 2 of 25 at pageSize 10)", len(list))
	}

	page := data["pagination"].(map[string]any)
	if page["total"] != float64(25) {
		t.Errorf("total = %v, want 25", page["total"])
	}
	if page["totalPages"] != float64(3) {
		t.Errorf("totalPages = %v, want 3", page["totalPages"])
	}
	if page["hasMore"] != true {
		t.Errorf("hasMore = %v, want true (page 2 of 3)", page["hasMore"])
	}
}

func TestHistory_LastPageReportsNoMore(t *testing.T) {
	store := &fakeStore{}
	for i := 0; i < 15; i++ {
		store.rows = append(store.rows, notifications.Notification{
			ID: itoa(i + 1), TenantID: "t1", RecipientUserID: "u1", VisibleInApp: true,
		})
	}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	rec := authedRequest(t, http.MethodGet, "/api/notifications/history?page=2&pageSize=10", "t1", "u1", nil, h.History)

	resp := decodeResponse(t, rec)
	data := resp.Data.(map[string]any)
	list := data["notifications"].([]any)
	if len(list) != 5 {
		t.Fatalf("got %d notifications, want 5 (remainder of 15 at pageSize 10)", len(list))
	}
	if data["pagination"].(map[string]any)["hasMore"] != false {
		t.Fatalf("hasMore = true on the last page, want false")
	}
}

func TestMarkRead_RecordsAuditEntry(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "n1", TenantID: "t1", RecipientUserID: "u1", VisibleInApp: true},
	}}
	recorder := &fakeRecorder{}
	h := NewHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, recorder, config.Config{})

	rec := authedRequestWithPath(t, http.MethodPost, "/api/notifications/n1/read",
		"t1", "u1", map[string]string{"id": "n1"}, nil, h.MarkRead)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	entry, ok := recorder.find(audit.ActionNotificationRead)
	if !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionNotificationRead)
	}
	if entry.TenantID != "t1" || entry.ActorUserID != "u1" || entry.ResourceID != "n1" {
		t.Fatalf("audit entry = %+v, want tenant t1, actor u1, resource n1", entry)
	}
	if entry.ActorType != audit.ActorUser {
		t.Fatalf("ActorType = %q, want %q", entry.ActorType, audit.ActorUser)
	}
}

func TestMarkRead_FailureRecordsNoAuditEntry(t *testing.T) {
	// An action that didn't happen must not appear in the trail.
	recorder := &fakeRecorder{}
	h := NewHandler(&fakeStore{}, newFakePreferencesStore(), &fakeDeliveriesStore{}, recorder, config.Config{})

	rec := authedRequestWithPath(t, http.MethodPost, "/api/notifications/missing/read",
		"t1", "u1", map[string]string{"id": "missing"}, nil, h.MarkRead)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if _, ok := recorder.find(audit.ActionNotificationRead); ok {
		t.Fatal("audit entry recorded for a notification that was never marked read")
	}
}

func TestMarkAllRead_RecordsAuditEntry(t *testing.T) {
	store := &fakeStore{rows: []notifications.Notification{
		{ID: "n1", TenantID: "t1", RecipientUserID: "u1"},
	}}
	recorder := &fakeRecorder{}
	h := NewHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, recorder, config.Config{})

	authedRequest(t, http.MethodPost, "/api/notifications/read-all", "t1", "u1", nil, h.MarkAllRead)

	if _, ok := recorder.find(audit.ActionNotificationReadAll); !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionNotificationReadAll)
	}
}

func TestCreate_RecordsAuditEntryPerNotification(t *testing.T) {
	recorder := &fakeRecorder{}
	h := NewHandler(&fakeStore{}, newFakePreferencesStore(), &fakeDeliveriesStore{}, recorder, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID: "t1",
		Recipients: []recipientTarget{
			{UserID: "u1", Email: "u1@example.com"},
			{UserID: "u2", Email: "u2@example.com"},
		},
		ActorUserID: "actor-1",
		EventType:   "quote.sent",
		Resource:    "quote",
		ResourceID:  "q-1",
		Title:       "Quote Q-1 sent",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.entries) != 2 {
		t.Fatalf("recorded %d audit entries, want 2 (one per recipient)", len(recorder.entries))
	}
	for _, entry := range recorder.entries {
		if entry.ActorType != audit.ActorService {
			t.Errorf("ActorType = %q, want %q (the call arrives over the internal secret)", entry.ActorType, audit.ActorService)
		}
		if entry.ActorUserID != "actor-1" {
			t.Errorf("ActorUserID = %q, want actor-1 (the user whose action raised the event)", entry.ActorUserID)
		}
	}
}

func TestCreate_IdempotencyKeyReplayReturnsSameIDsAndEnqueuesOnce(t *testing.T) {
	store := &fakeStore{}
	deliv := &fakeDeliveriesStore{}
	h := newTestHandler(store, newFakePreferencesStore(), deliv, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID: "t1",
		Recipients: []recipientTarget{
			{UserID: "u1", Email: "U1@example.com"},
			{UserID: "u2", Email: "u2@example.com"},
		},
		EventType:      "automation.reminder",
		Resource:       "invoice",
		ResourceID:     "inv-1",
		Title:          "Invoice due",
		IdempotencyKey: "rule:invoice:42:2026-10-20:0",
	})
	post := func() []string {
		req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data struct {
				Notifications []notifications.Notification `json:"notifications"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var ids []string
		for _, n := range resp.Data.Notifications {
			ids = append(ids, n.ID)
		}
		return ids
	}

	first := post()
	enqueuedAfterFirst := len(deliv.rows)
	if len(first) != 2 || enqueuedAfterFirst == 0 {
		t.Fatalf("first: ids=%v enqueued=%d", first, enqueuedAfterFirst)
	}
	second := post()
	if len(second) != 2 || second[0] != first[0] || second[1] != first[1] {
		t.Fatalf("replay ids = %v, want %v", second, first)
	}
	if len(store.rows) != 2 {
		t.Fatalf("store rows = %d, want 2", len(store.rows))
	}
	if len(deliv.rows) != enqueuedAfterFirst {
		t.Fatalf("deliveries = %d after replay, want %d (no re-enqueue)", len(deliv.rows), enqueuedAfterFirst)
	}
}

func TestCreate_InvalidIdempotencyKeyReturns400AndCreatesNothing(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"oversized", strings.Repeat("k", maxIdempotencyKeyBytes+1)},
		{"control character", "rule:inv\x00oice"},
		{"newline", "rule:\ninvoice"},
		{"del", "rule:\x7finvoice"},
		{"composed key over column width", strings.Repeat("k", maxIdempotencyKeyBytes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			deliv := &fakeDeliveriesStore{}
			h := newTestHandler(store, newFakePreferencesStore(), deliv, config.Config{})

			email := "u1@example.com"
			if tc.name == "composed key over column width" {
				email = strings.Repeat("a", 100) + "@example.com"
			}
			body, _ := json.Marshal(createNotificationRequest{
				TenantID:       "t1",
				Recipients:     []recipientTarget{{UserID: "00000000-0000-4000-8000-000000000002", Email: email}},
				EventType:      "automation.reminder",
				Resource:       "invoice",
				ResourceID:     "inv-1",
				Title:          "Invoice due",
				IdempotencyKey: tc.key,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			h.Create(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			if len(store.rows) != 0 || len(deliv.rows) != 0 {
				t.Fatalf("rows=%d deliveries=%d, want nothing created", len(store.rows), len(deliv.rows))
			}
		})
	}
}

func TestCreate_MaxLengthIdempotencyKeyAccepted(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})
	body, _ := json.Marshal(createNotificationRequest{
		TenantID:       "t1",
		Recipients:     []recipientTarget{{UserID: "00000000-0000-4000-8000-000000000002", Email: "u1@example.com"}},
		EventType:      "automation.reminder",
		Resource:       "invoice",
		ResourceID:     "inv-1",
		Title:          "Invoice due",
		IdempotencyKey: strings.Repeat("k", maxIdempotencyKeyBytes),
	})
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}
