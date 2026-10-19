package workers

import (
	"context"
	"testing"

	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/notifications"
)

// claimingDeliveries embeds fakeDeliveries and overrides ClaimPending to
// return a canned batch once, so QueueConsumer.pollOnce can be tested
// without a real database.
type claimingDeliveries struct {
	fakeDeliveries
	pending []deliveries.Delivery
}

func (f *claimingDeliveries) ClaimPending(_ context.Context, _ int) ([]deliveries.Delivery, error) {
	claimed := f.pending
	f.pending = nil
	return claimed, nil
}

func TestQueueConsumer_PollOnce_AttemptsClaimedDeliveries(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &claimingDeliveries{pending: []deliveries.Delivery{
		{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail},
	}}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail:     func(_ config.Config, _, _, _, _, _ string, _ *channels.EmailAttachment, _ channels.EmailOptions) (string, error) { return "", nil },
	}

	QueueConsumer{Deps: deps}.pollOnce(context.Background())

	if delivStore.sentID != "d1" {
		t.Fatalf("sentID = %q, want d1 (consumer must attempt every claimed delivery)", delivStore.sentID)
	}
}

func TestQueueConsumer_PollOnce_NoClaimedRows_DoesNothing(t *testing.T) {
	delivStore := &claimingDeliveries{}
	deps := Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{}},
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
	}

	QueueConsumer{Deps: deps}.pollOnce(context.Background())

	if delivStore.sentID != "" || delivStore.retryID != "" {
		t.Fatalf("expected no delivery activity, got sentID=%q retryID=%q", delivStore.sentID, delivStore.retryID)
	}
}
