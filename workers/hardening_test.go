package workers

import (
	"context"
	"testing"

	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/notifications"
)

func hardeningDeps(send func(config.Config, string, string, string, string, string, []channels.EmailAttachment, channels.EmailOptions) (string, error)) (Deps, *fakeDeliveries) {
	dv := &fakeDeliveries{}
	return Deps{
		Notifications: &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}},
		Deliveries:    dv,
		PushSubs:      &fakePushSubs{},
		SendEmail:     send,
	}, dv
}

func TestAttemptDelivery_PanicIsRecordedAsFailedAttempt(t *testing.T) {
	deps, dv := hardeningDeps(func(config.Config, string, string, string, string, string, []channels.EmailAttachment, channels.EmailOptions) (string, error) {
		panic("boom")
	})
	attemptDelivery(context.Background(), deps, deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", Channel: deliveries.ChannelEmail})
	if dv.retryID != "d1" || dv.retryAtt != 1 {
		t.Fatalf("retry = (%q, %d), want (d1, 1)", dv.retryID, dv.retryAtt)
	}
}

func TestAttemptDelivery_UnsupportedChannelFailsTerminally(t *testing.T) {
	deps, dv := hardeningDeps(nil)
	attemptDelivery(context.Background(), deps, deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", Channel: "sms"})
	if dv.retryID != "d1" || dv.retryAtt != deliveries.MaxAttempts {
		t.Fatalf("retry = (%q, %d), want (d1, %d)", dv.retryID, dv.retryAtt, deliveries.MaxAttempts)
	}
}

func TestAttemptDelivery_MissingNotificationFailsWithoutRetries(t *testing.T) {
	deps, dv := hardeningDeps(nil)
	attemptDelivery(context.Background(), deps, deliveries.Delivery{ID: "d1", NotificationID: "gone", TenantID: "t1", Channel: deliveries.ChannelEmail})
	if dv.retryAtt != deliveries.MaxAttempts {
		t.Fatalf("attempts = %d, want %d", dv.retryAtt, deliveries.MaxAttempts)
	}
}

func TestAttemptDelivery_OutcomeWrittenAfterShutdownCancel(t *testing.T) {
	deps, _ := hardeningDeps(nil)
	ctxCheck := &ctxRecordingDeliveries{}
	ctx, cancel := context.WithCancel(context.Background())
	deps.SendEmail = func(config.Config, string, string, string, string, string, []channels.EmailAttachment, channels.EmailOptions) (string, error) {
		cancel() // SIGTERM arrives while the send is in flight
		return "re_1", nil
	}
	ctxCheck.fakeDeliveries = *deps.Deliveries.(*fakeDeliveries)
	deps.Deliveries = ctxCheck
	attemptDelivery(ctx, deps, deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", Channel: deliveries.ChannelEmail})
	if ctxCheck.sentID != "d1" || ctxCheck.markSentErr != nil {
		t.Fatalf("MarkSent id=%q ctxErr=%v, want d1 and live context", ctxCheck.sentID, ctxCheck.markSentErr)
	}
}

type ctxRecordingDeliveries struct {
	fakeDeliveries
	markSentErr error
}

func (c *ctxRecordingDeliveries) MarkSent(ctx context.Context, id string, resp []byte, pid string) error {
	c.markSentErr = ctx.Err()
	return c.fakeDeliveries.MarkSent(ctx, id, resp, pid)
}
