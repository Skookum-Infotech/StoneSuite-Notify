package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"stonesuite-notify/audit"
	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/notifications"
	"stonesuite-notify/pushsubs"
)

// fakeNotifications is an in-memory notifications.Store used so
// attemptDelivery tests don't need a real database. Only Get is
// meaningfully implemented — the rest of the interface is unused here.
type fakeNotifications struct {
	byID        map[string]notifications.Notification
	attachments map[string]notifications.AttachmentInput // keyed by notification id
}

func (f *fakeNotifications) Create(_ context.Context, _ notifications.CreateInput) (*notifications.Notification, error) {
	return nil, nil
}
func (f *fakeNotifications) Get(_ context.Context, _, id string) (*notifications.Notification, error) {
	n, ok := f.byID[id]
	if !ok {
		return nil, notifications.ErrNotFound
	}
	return &n, nil
}
func (f *fakeNotifications) CountForUser(_ context.Context, _, _ string, _ []string, _ bool) (int, error) {
	return 0, nil
}

func (f *fakeNotifications) ListForUser(_ context.Context, _, _ string, _ []string, _ bool, _, _ int) ([]notifications.Notification, error) {
	return nil, nil
}
func (f *fakeNotifications) UnreadCount(_ context.Context, _, _ string, _ []string) (int, error) {
	return 0, nil
}
func (f *fakeNotifications) MarkRead(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeNotifications) MarkAllRead(_ context.Context, _, _ string, _ []string) error {
	return nil
}
func (f *fakeNotifications) SaveAttachment(_ context.Context, _ string, _ notifications.AttachmentInput) error {
	return nil
}
func (f *fakeNotifications) GetAttachment(_ context.Context, _, id string) (*notifications.AttachmentInput, error) {
	a, ok := f.attachments[id]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

// fakeDeliveries is an in-memory deliveries.Store recording the last
// MarkSent/MarkSkipped/MarkRetrying call so tests can assert on outcome.
type fakeDeliveries struct {
	sentID         string
	sentResp       []byte
	sentProviderID string
	skipID         string
	skipResp       []byte
	retryID        string
	retryAtt       int
	retryErr       string
	retryResp      []byte
}

func (f *fakeDeliveries) Enqueue(_ context.Context, _, _, _, _, _ string) (*deliveries.Delivery, error) {
	return nil, nil
}
func (f *fakeDeliveries) ClaimPending(_ context.Context, _ int) ([]deliveries.Delivery, error) {
	return nil, nil
}
func (f *fakeDeliveries) ClaimDue(_ context.Context, _ int, _ time.Duration) ([]deliveries.Delivery, error) {
	return nil, nil
}
func (f *fakeDeliveries) MarkSent(_ context.Context, id string, resp []byte, providerEmailID string) error {
	f.sentID, f.sentResp, f.sentProviderID = id, resp, providerEmailID
	return nil
}
func (f *fakeDeliveries) MarkSkipped(_ context.Context, id string, resp []byte) error {
	f.skipID, f.skipResp = id, resp
	return nil
}
func (f *fakeDeliveries) MarkRetrying(_ context.Context, id string, attempts int, lastErr string, resp []byte) error {
	f.retryID, f.retryAtt, f.retryErr, f.retryResp = id, attempts, lastErr, resp
	return nil
}
func (f *fakeDeliveries) ListForNotification(_ context.Context, _, _ string) ([]deliveries.Delivery, error) {
	return nil, nil
}
func (f *fakeDeliveries) ListByStatus(_ context.Context, _, _ string, _ time.Time, _ int) ([]deliveries.Delivery, error) {
	return nil, nil
}

// fakePushSubs is an in-memory pushsubs.Store recording pruned endpoints.
type fakePushSubs struct {
	subs    []pushsubs.Subscription
	deleted []string
}

func (f *fakePushSubs) Save(_ context.Context, _, _ string, _ pushsubs.SaveInput) error { return nil }
func (f *fakePushSubs) Delete(_ context.Context, _, _, endpoint string) error {
	f.deleted = append(f.deleted, endpoint)
	return nil
}
func (f *fakePushSubs) ListForUser(_ context.Context, _, _ string) ([]pushsubs.Subscription, error) {
	return f.subs, nil
}

func testNotification(id string) notifications.Notification {
	return notifications.Notification{
		ID: id, TenantID: "t1", RecipientUserID: "u1",
		RecipientEmail: "u1@example.com", Title: "Title", Body: "Body", Link: "https://example.com",
	}
}

func TestAttemptDelivery_Email_Success_MarksSent(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.sentID != "d1" {
		t.Fatalf("sentID = %q, want d1", delivStore.sentID)
	}
}

func TestAttemptDelivery_Email_Failure_MarksRetryingWithIncrementedAttempts(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", errors.New("smtp down")
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail, Attempts: 0}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.retryID != "d1" || delivStore.retryAtt != 1 {
		t.Fatalf("retry = (%q, %d), want (d1, 1)", delivStore.retryID, delivStore.retryAtt)
	}
}

func TestAttemptDelivery_Push_AllEndpointsSucceed_MarksSent(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	pushSubs := &fakePushSubs{subs: []pushsubs.Subscription{
		{ID: "sub1", TenantID: "t1", UserID: "u1", Endpoint: "https://push/1"},
		{ID: "sub2", TenantID: "t1", UserID: "u1", Endpoint: "https://push/2"},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      pushSubs,
		SendPush:      func(_ config.Config, _ channels.PushSubscription, _, _, _ string) (bool, error) { return false, nil },
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelPush}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.sentID != "d1" {
		t.Fatalf("sentID = %q, want d1", delivStore.sentID)
	}
	var resp pushProviderResponse
	if err := json.Unmarshal(delivStore.sentResp, &resp); err != nil {
		t.Fatalf("decode provider response: %v", err)
	}
	if len(resp.Endpoints) != 2 || resp.Endpoints["sub1"].Status != "sent" || resp.Endpoints["sub2"].Status != "sent" {
		t.Fatalf("endpoints = %+v, want both sent", resp.Endpoints)
	}
}

func TestAttemptDelivery_Push_PartialFailure_MarksRetryingWithOnlyFailedEndpoint(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	pushSubs := &fakePushSubs{subs: []pushsubs.Subscription{
		{ID: "sub1", TenantID: "t1", UserID: "u1", Endpoint: "https://push/1"},
		{ID: "sub2", TenantID: "t1", UserID: "u1", Endpoint: "https://push/2"},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      pushSubs,
		SendPush: func(_ config.Config, sub channels.PushSubscription, _, _, _ string) (bool, error) {
			if sub.Endpoint == "https://push/2" {
				return false, errors.New("gone")
			}
			return false, nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelPush, Attempts: 0}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.retryID != "d1" || delivStore.retryAtt != 1 {
		t.Fatalf("retry = (%q, %d), want (d1, 1)", delivStore.retryID, delivStore.retryAtt)
	}
	var resp pushProviderResponse
	if err := json.Unmarshal(delivStore.retryResp, &resp); err != nil {
		t.Fatalf("decode provider response: %v", err)
	}
	if resp.Endpoints["sub1"].Status != "sent" || resp.Endpoints["sub2"].Status != "failed" {
		t.Fatalf("endpoints = %+v, want sub1 sent + sub2 failed", resp.Endpoints)
	}
}

func TestAttemptDelivery_Push_RetryDoesNotResendAlreadySucceededEndpoint(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	pushSubs := &fakePushSubs{subs: []pushsubs.Subscription{
		{ID: "sub1", TenantID: "t1", UserID: "u1", Endpoint: "https://push/1"},
		{ID: "sub2", TenantID: "t1", UserID: "u1", Endpoint: "https://push/2"},
	}}
	var calledEndpoints []string
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      pushSubs,
		SendPush: func(_ config.Config, sub channels.PushSubscription, _, _, _ string) (bool, error) {
			calledEndpoints = append(calledEndpoints, sub.Endpoint)
			return false, nil
		},
	}

	priorResponse, _ := json.Marshal(pushProviderResponse{Endpoints: map[string]endpointOutcome{
		"sub1": {Status: "sent"},
		"sub2": {Status: "failed", Error: "gone"},
	}})
	d := deliveries.Delivery{
		ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1",
		Channel: deliveries.ChannelPush, Attempts: 1, ProviderResponse: priorResponse,
	}
	attemptDelivery(context.Background(), deps, d)

	if len(calledEndpoints) != 1 || calledEndpoints[0] != "https://push/2" {
		t.Fatalf("called endpoints = %v, want only https://push/2 (sub1 already sent)", calledEndpoints)
	}
}

func TestAttemptDelivery_Push_NoSubscriptions_MarksSkipped(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelPush}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.skipID != "d1" {
		t.Fatalf("skipID = %q, want d1", delivStore.skipID)
	}
}

func TestAttemptDelivery_Push_StaleSubscriptionPrunedAndCountsAsSent(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	pushSubs := &fakePushSubs{subs: []pushsubs.Subscription{
		{ID: "sub1", TenantID: "t1", UserID: "u1", Endpoint: "https://push/gone"},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      pushSubs,
		SendPush:      func(_ config.Config, _ channels.PushSubscription, _, _, _ string) (bool, error) { return true, nil },
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelPush}
	attemptDelivery(context.Background(), deps, d)

	if len(pushSubs.deleted) != 1 || pushSubs.deleted[0] != "https://push/gone" {
		t.Fatalf("deleted = %v, want [https://push/gone]", pushSubs.deleted)
	}
	if delivStore.sentID != "d1" {
		t.Fatalf("sentID = %q, want d1 (pruned-only should count as fully sent)", delivStore.sentID)
	}
}

func TestAttemptDelivery_NotificationLoadFailure_MarksRetrying(t *testing.T) {
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{}}, // n1 missing
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.retryID != "d1" {
		t.Fatalf("retryID = %q, want d1 (missing notification must not panic, must be treated as a failed attempt)", delivStore.retryID)
	}
}

// fakeAuditRecorder is a synchronous audit.Recorder capturing what the
// workers audited, so tests can assert on terminal outcomes without a
// database or a goroutine.
type fakeAuditRecorder struct {
	entries []audit.Entry
}

func (f *fakeAuditRecorder) Record(_ context.Context, e audit.Entry) {
	f.entries = append(f.entries, e)
}

func (f *fakeAuditRecorder) find(action string) (audit.Entry, bool) {
	for _, e := range f.entries {
		if e.Action == action {
			return e, true
		}
	}
	return audit.Entry{}, false
}

func TestAttemptDelivery_Email_Success_AuditsSent(t *testing.T) {
	recorder := &fakeAuditRecorder{}
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    &fakeDeliveries{},
		PushSubs:      &fakePushSubs{},
		Audit:         recorder,
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)

	entry, ok := recorder.find(audit.ActionDeliverySent)
	if !ok {
		t.Fatalf("no %s audit entry recorded", audit.ActionDeliverySent)
	}
	if entry.TenantID != "t1" || entry.ResourceID != "d1" {
		t.Fatalf("audit entry = %+v, want tenant t1, resource d1", entry)
	}
	if entry.ActorType != audit.ActorWorker {
		t.Fatalf("ActorType = %q, want %q", entry.ActorType, audit.ActorWorker)
	}
}

func TestAttemptDelivery_RetryableFailure_RecordsNoAuditEntry(t *testing.T) {
	// Intermediate attempts stay off the trail — the delivery row already
	// carries attempts and last_error — so only outcomes are audited.
	recorder := &fakeAuditRecorder{}
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    &fakeDeliveries{},
		PushSubs:      &fakePushSubs{},
		Audit:         recorder,
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", errors.New("smtp down")
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail, Attempts: 0}
	attemptDelivery(context.Background(), deps, d)

	if len(recorder.entries) != 0 {
		t.Fatalf("recorded %d audit entries, want 0 (attempt 1 of %d is not terminal)", len(recorder.entries), deliveries.MaxAttempts)
	}
}

func TestAttemptDelivery_FinalFailure_AuditsFailed(t *testing.T) {
	recorder := &fakeAuditRecorder{}
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    &fakeDeliveries{},
		PushSubs:      &fakePushSubs{},
		Audit:         recorder,
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", errors.New("smtp down")
		},
	}

	// One attempt short of the budget: this attempt exhausts it.
	d := deliveries.Delivery{
		ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1",
		Channel: deliveries.ChannelEmail, Attempts: deliveries.MaxAttempts - 1,
	}
	attemptDelivery(context.Background(), deps, d)

	entry, ok := recorder.find(audit.ActionDeliveryFailed)
	if !ok {
		t.Fatalf("no %s audit entry recorded for an exhausted delivery", audit.ActionDeliveryFailed)
	}
	if entry.ActorType != audit.ActorWorker || entry.ResourceID != "d1" {
		t.Fatalf("audit entry = %+v, want a worker-actor entry for d1", entry)
	}
}

func TestAttemptDelivery_FinalFailure_LogsStableAlertLine(t *testing.T) {
	// The audit row is the durable record; this greppable log line is the
	// active signal an ops alert can match. Keep the key stable.
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    &fakeDeliveries{},
		PushSubs:      &fakePushSubs{},
		Audit:         &fakeAuditRecorder{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", errors.New("resend: unexpected status 422: the domain is not verified")
		},
	}
	d := deliveries.Delivery{
		ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1",
		Channel: deliveries.ChannelEmail, Attempts: deliveries.MaxAttempts - 1,
	}
	attemptDelivery(context.Background(), deps, d)

	got := buf.String()
	for _, want := range []string{
		"event=delivery_permanently_failed", "delivery_id=d1", "notification_id=n1",
		"tenant_id=t1", "channel=email", "the domain is not verified",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("terminal-failure log missing %q\ngot: %s", want, got)
		}
	}
}

func TestAttemptDelivery_NilRecorder_DoesNotPanic(t *testing.T) {
	// Deps built by hand in other tests leave Audit nil; delivery must not
	// depend on auditing being wired up.
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    &fakeDeliveries{},
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)
}

func TestAttemptEmail_WithAttachment_PassesItToSendEmail(t *testing.T) {
	n := notifications.Notification{
		ID: "n-1", TenantID: "t-1",
		RecipientEmail: "user@example.com", Title: "Invoice sent",
	}
	notifStore := &fakeNotifications{
		byID:        map[string]notifications.Notification{"n-1": n},
		attachments: map[string]notifications.AttachmentInput{"n-1": {FileName: "INV-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")}},
	}
	deliveryStore := &fakeDeliveries{}

	var gotAttachments []channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Audit:         nil,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, attachments []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			gotAttachments = attachments
			return "", nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-1", NotificationID: "n-1", Channel: deliveries.ChannelEmail,
	})

	if len(gotAttachments) != 1 {
		t.Fatalf("expected one attachment to be passed to SendEmail, got %d", len(gotAttachments))
	}
	if gotAttachments[0].FileName != "INV-1.pdf" {
		t.Fatalf("expected fileName INV-1.pdf, got %q", gotAttachments[0].FileName)
	}
	if deliveryStore.sentID != "d-1" {
		t.Fatalf("expected delivery d-1 to be marked sent, got %q", deliveryStore.sentID)
	}
}

func TestAttemptEmail_PassesEmailBodyHTMLFromAttachment(t *testing.T) {
	n := notifications.Notification{
		ID: "n-3", TenantID: "t-1",
		RecipientEmail: "customer@example.com", Title: "Sales Order SO-1", Body: "Document sent.",
	}
	notifStore := &fakeNotifications{
		byID:        map[string]notifications.Notification{"n-3": n},
		attachments: map[string]notifications.AttachmentInput{"n-3": {EmailBodyHTML: "<p>Branded body.</p>"}},
	}
	deliveryStore := &fakeDeliveries{}

	var gotEmailBodyHTML string
	var gotAttachments []channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, emailBodyHTML string, attachments []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			gotEmailBodyHTML = emailBodyHTML
			gotAttachments = attachments
			return "", nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-3", NotificationID: "n-3", Channel: deliveries.ChannelEmail,
	})

	if gotEmailBodyHTML != "<p>Branded body.</p>" {
		t.Fatalf("emailBodyHTML = %q, want %q", gotEmailBodyHTML, "<p>Branded body.</p>")
	}
	if len(gotAttachments) != 0 {
		t.Fatalf("expected no file attachment (attachment row has no FileName), got %+v", gotAttachments)
	}
}

func TestAttemptEmail_NoAttachment_PassesNil(t *testing.T) {
	n := notifications.Notification{ID: "n-2", TenantID: "t-1", RecipientEmail: "user@example.com"}
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n-2": n}}
	deliveryStore := &fakeDeliveries{}

	var gotAttachments []channels.EmailAttachment
	called := false
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    deliveryStore,
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, attachments []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			called = true
			gotAttachments = attachments
			return "", nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{
		ID: "d-2", NotificationID: "n-2", Channel: deliveries.ChannelEmail,
	})

	if !called {
		t.Fatal("expected SendEmail to be called")
	}
	if len(gotAttachments) != 0 {
		t.Fatalf("expected no attachments, got %+v", gotAttachments)
	}
}

func TestAttemptEmail_MoreFiles_FollowTheFirstInOrder(t *testing.T) {
	n := notifications.Notification{ID: "n-4", TenantID: "t-1", RecipientEmail: "customer@example.com", Title: "Re: Your order"}
	notifStore := &fakeNotifications{
		byID: map[string]notifications.Notification{"n-4": n},
		attachments: map[string]notifications.AttachmentInput{"n-4": {
			FileName: "plan.pdf", ContentType: "application/pdf", Content: []byte("one"),
			MoreFiles: []notifications.AttachmentFile{
				{FileName: "kitchen.jpg", ContentType: "image/jpeg", Content: []byte("two")},
				{FileName: "notes.txt", ContentType: "text/plain", Content: []byte("three")},
			},
		}},
	}

	var got []channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    &fakeDeliveries{},
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, attachments []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			got = attachments
			return "", nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{ID: "d-4", NotificationID: "n-4", Channel: deliveries.ChannelEmail})

	if len(got) != 3 || got[0].FileName != "plan.pdf" || got[1].FileName != "kitchen.jpg" || got[2].FileName != "notes.txt" {
		t.Fatalf("attachments = %+v, want plan.pdf, kitchen.jpg, notes.txt in that order", got)
	}
	if got[1].ContentType != "image/jpeg" || string(got[2].Content) != "three" {
		t.Fatalf("a later file lost its type or content: %+v", got)
	}
}

func TestAttemptEmail_FilesWithoutAFirstFile_StillSendsTheOthers(t *testing.T) {
	// An attachment row that carries only more files (no first file name) must not
	// drop them.
	n := notifications.Notification{ID: "n-5", TenantID: "t-1", RecipientEmail: "customer@example.com", Title: "Re"}
	notifStore := &fakeNotifications{
		byID: map[string]notifications.Notification{"n-5": n},
		attachments: map[string]notifications.AttachmentInput{"n-5": {
			MoreFiles: []notifications.AttachmentFile{{FileName: "only.pdf", ContentType: "application/pdf", Content: []byte("x")}},
		}},
	}
	var got []channels.EmailAttachment
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    &fakeDeliveries{},
		Config:        config.Config{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, attachments []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			got = attachments
			return "", nil
		},
	}

	attemptDelivery(context.Background(), deps, deliveries.Delivery{ID: "d-5", NotificationID: "n-5", Channel: deliveries.ChannelEmail})

	if len(got) != 1 || got[0].FileName != "only.pdf" {
		t.Fatalf("attachments = %+v, want only.pdf", got)
	}
}

func TestAttemptDelivery_Email_Success_StoresProviderEmailID(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "re_email_123", nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.sentID != "d1" || delivStore.sentProviderID != "re_email_123" {
		t.Fatalf("MarkSent(id=%q, providerEmailID=%q), want (d1, re_email_123)", delivStore.sentID, delivStore.sentProviderID)
	}
}
