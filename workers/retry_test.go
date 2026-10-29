package workers

import (
	"context"
	"testing"
	"time"

	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/notifications"
)

// dueDeliveries embeds fakeDeliveries and overrides ClaimDue to return a
// canned batch once, so RetryWorker.pollOnce can be tested without a real
// database.
type dueDeliveries struct {
	fakeDeliveries
	due []deliveries.Delivery
}

func (f *dueDeliveries) ClaimDue(_ context.Context, _ int, _ time.Duration) ([]deliveries.Delivery, error) {
	claimed := f.due
	f.due = nil
	return claimed, nil
}

func TestRetryWorker_PollOnce_AttemptsClaimedDeliveries(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &dueDeliveries{due: []deliveries.Delivery{
		{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail, Attempts: 1},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", nil
		},
	}

	RetryWorker{Deps: deps}.pollOnce(context.Background())

	if delivStore.sentID != "d1" {
		t.Fatalf("sentID = %q, want d1 (retry worker must attempt every claimed delivery)", delivStore.sentID)
	}
}

func TestRetryWorker_PollOnce_RepeatedFailure_IncrementsFromPriorAttempts(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &dueDeliveries{due: []deliveries.Delivery{
		{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail, Attempts: 3},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ []channels.EmailAttachment, _ channels.EmailOptions) (string, error) {
			return "", context.DeadlineExceeded
		},
	}

	RetryWorker{Deps: deps}.pollOnce(context.Background())

	if delivStore.retryID != "d1" || delivStore.retryAtt != 4 {
		t.Fatalf("retry = (%q, %d), want (d1, 4)", delivStore.retryID, delivStore.retryAtt)
	}
}
