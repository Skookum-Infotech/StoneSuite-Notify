//go:build dbtest

package emailevents_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stonesuite-notify/database"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/emailevents"
	"stonesuite-notify/notifications"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Twice on purpose: the schema must be idempotent (it runs on every boot).
	for i := 0; i < 2; i++ {
		if err := database.ApplySchema(ctx, pool); err != nil {
			t.Fatalf("ApplySchema run %d: %v", i+1, err)
		}
	}
	return pool
}

type seeded struct {
	tenantID       string
	actorID        string
	notificationID string
	providerID     string
}

// seed inserts an email notification (optionally with an actor) and its email
// delivery already marked sent by the queue, carrying a provider email id —
// exactly the state a delivered-by-Resend email is in when a webhook arrives.
func seed(t *testing.T, pool *pgxpool.Pool, withActor bool) seeded {
	t.Helper()
	ctx := context.Background()
	var s seeded
	var resourceID string
	if err := pool.QueryRow(ctx,
		`SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text`).
		Scan(&s.tenantID, &s.actorID, &resourceID); err != nil {
		t.Fatalf("ids: %v", err)
	}
	t.Cleanup(func() {
		// Deleting the notifications cascades to deliveries and email_events;
		// it also removes any alert rows (same tenant).
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE tenant_id = $1`, s.tenantID)
	})

	var actorArg any
	if withActor {
		actorArg = s.actorID
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO notifications (tenant_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, status_link)
		VALUES ($1, 'cust@example.com', $2, 'document.sent', 'invoice', $3, 'Invoice sent', '/sales/invoices/1')
		RETURNING id::text`, s.tenantID, actorArg, resourceID).Scan(&s.notificationID); err != nil {
		t.Fatalf("insert notification: %v", err)
	}
	s.providerID = "em_" + s.notificationID
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_deliveries (notification_id, tenant_id, channel, status, provider_email_id)
		VALUES ($1, $2, 'email', 'sent', $3)`, s.notificationID, s.tenantID, s.providerID); err != nil {
		t.Fatalf("insert delivery: %v", err)
	}
	return s
}

func event(s seeded, svixID, eventType string) emailevents.Event {
	return emailevents.Event{
		SvixID: svixID, ProviderEmailID: s.providerID, Type: eventType,
		OccurredAt: time.Now(), Detail: "provider says: " + eventType,
	}
}

func countAlerts(t *testing.T, pool *pgxpool.Pool, s seeded) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND event_type = $3`,
		s.tenantID, s.actorID, emailevents.AlertEventType).Scan(&n); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	return n
}

func TestApply_LifecycleAlertsOncePerProblemState(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	ctx := context.Background()
	s := seed(t, pool, true)

	// sent → delivered: state moves, no alert.
	for i, evt := range []string{"email.sent", "email.delivered"} {
		res, err := store.Apply(ctx, event(s, s.providerID+"-a"+string(rune('0'+i)), evt))
		if err != nil || !res.Matched || !res.Changed || res.Alerted {
			t.Fatalf("Apply(%s) = %+v, %v; want matched+changed, no alert", evt, res, err)
		}
	}
	if got := countAlerts(t, pool, s); got != 0 {
		t.Fatalf("alerts after sent/delivered = %d, want 0", got)
	}

	// delivered → bounced: state moves and exactly one alert is raised.
	res, err := store.Apply(ctx, event(s, s.providerID+"-b", "email.bounced"))
	if err != nil || !res.Changed || !res.Alerted || res.Status != emailevents.StatusBounced {
		t.Fatalf("Apply(bounced) = %+v, %v; want changed+alerted bounced", res, err)
	}
	if got := countAlerts(t, pool, s); got != 1 {
		t.Fatalf("alerts after bounce = %d, want 1", got)
	}

	// A second terminal event of equal rank, and a late lower-rank event, change nothing.
	for i, evt := range []string{"email.failed", "email.delivered", "email.sent"} {
		res, err := store.Apply(ctx, event(s, s.providerID+"-c"+string(rune('0'+i)), evt))
		if err != nil || !res.Matched || res.Changed || res.Alerted {
			t.Fatalf("Apply(%s) after bounce = %+v, %v; want matched, unchanged, no alert", evt, res, err)
		}
	}
	if got := countAlerts(t, pool, s); got != 1 {
		t.Fatalf("alerts after later events = %d, want still 1", got)
	}

	// The queue column is never touched by a webhook.
	var queueStatus, providerStatus string
	if err := pool.QueryRow(ctx, `
		SELECT status, provider_status FROM notification_deliveries WHERE provider_email_id = $1`,
		s.providerID).Scan(&queueStatus, &providerStatus); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if queueStatus != "sent" || providerStatus != emailevents.StatusBounced {
		t.Fatalf("status/provider_status = %q/%q, want sent/bounced", queueStatus, providerStatus)
	}
}

func TestApply_AlertContent(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	ctx := context.Background()
	s := seed(t, pool, true)

	if _, err := store.Apply(ctx, event(s, s.providerID+"-x", "email.bounced")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var title, body, link, resource string
	var visible bool
	if err := pool.QueryRow(ctx, `
		SELECT title, body, link, resource, visible_in_app FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND event_type = $3`,
		s.tenantID, s.actorID, emailevents.AlertEventType).Scan(&title, &body, &link, &resource, &visible); err != nil {
		t.Fatalf("read alert: %v", err)
	}
	if !strings.Contains(title, "cust@example.com") || !strings.Contains(title, "bounced") {
		t.Errorf("title = %q, want it to name the recipient and the outcome", title)
	}
	if strings.Contains(body, "provider says") || strings.Contains(title, "provider says") {
		t.Error("provider detail leaked into the alert")
	}
	if link != "/sales/invoices/1" {
		t.Errorf("link = %q, want the original status_link", link)
	}
	if resource != "invoice" || !visible {
		t.Errorf("resource/visible = %q/%v, want invoice/true", resource, visible)
	}

	// The sender can read it through the real feed query, scoped like their token.
	feed := notifications.NewPGStore(pool)
	for _, tc := range []struct {
		allowed []string
		want    int
	}{{[]string{"invoice"}, 1}, {[]string{"quote"}, 0}, {nil, 1}} {
		list, err := feed.ListForUser(ctx, s.tenantID, s.actorID, tc.allowed, false, 20, 0)
		if err != nil || len(list) != tc.want {
			t.Errorf("ListForUser(allowed=%v) = %d rows, %v; want %d", tc.allowed, len(list), err, tc.want)
		}
	}

	// A delivery log row records the alert as already in-app delivered.
	var inApp int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM notification_deliveries d JOIN notifications n ON n.id = d.notification_id
		WHERE n.tenant_id = $1 AND n.event_type = $2 AND d.channel = 'in_app' AND d.status = 'sent'`,
		s.tenantID, emailevents.AlertEventType).Scan(&inApp); err != nil || inApp != 1 {
		t.Errorf("in_app delivery rows for the alert = %d, %v; want 1", inApp, err)
	}
}

// TestApply_AlertUsesStatusResourceWhenGiven: a portal invite's own resource
// ("portal_user") is not an RBAC resource, so the alert must carry the resource
// the caller named for the status page ("portal_access") or the sender's bell
// filter would hide it.
func TestApply_AlertUsesStatusResourceWhenGiven(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	ctx := context.Background()
	s := seed(t, pool, true)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status_resource = 'portal_access' WHERE id = $1`, s.notificationID); err != nil {
		t.Fatalf("set status_resource: %v", err)
	}

	if _, err := store.Apply(ctx, event(s, s.providerID+"-sr", "email.bounced")); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var resource string
	if err := pool.QueryRow(ctx, `
		SELECT resource FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND event_type = $3`,
		s.tenantID, s.actorID, emailevents.AlertEventType).Scan(&resource); err != nil {
		t.Fatalf("read alert: %v", err)
	}
	if resource != "portal_access" {
		t.Fatalf("alert resource = %q, want the status_resource portal_access (not the original's invoice)", resource)
	}

	feed := notifications.NewPGStore(pool)
	for _, tc := range []struct {
		allowed []string
		want    int
	}{{[]string{"portal_access"}, 1}, {[]string{"invoice"}, 0}} {
		list, err := feed.ListForUser(ctx, s.tenantID, s.actorID, tc.allowed, false, 20, 0)
		if err != nil || len(list) != tc.want {
			t.Errorf("ListForUser(allowed=%v) = %d rows, %v; want %d", tc.allowed, len(list), err, tc.want)
		}
	}
}

func TestApply_DuplicateSvixIDIsANoop(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	ctx := context.Background()
	s := seed(t, pool, true)

	first, err := store.Apply(ctx, event(s, s.providerID+"-dup", "email.bounced"))
	if err != nil || !first.Alerted {
		t.Fatalf("first Apply = %+v, %v; want alerted", first, err)
	}
	again, err := store.Apply(ctx, event(s, s.providerID+"-dup", "email.bounced"))
	if err != nil || !again.Matched || !again.Duplicate || again.Changed || again.Alerted {
		t.Fatalf("replayed Apply = %+v, %v; want matched duplicate, no change, no alert", again, err)
	}
	if got := countAlerts(t, pool, s); got != 1 {
		t.Fatalf("alerts after replay = %d, want 1", got)
	}
}

func TestApply_UnknownProviderEmailIsIgnored(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	res, err := store.Apply(context.Background(), emailevents.Event{
		SvixID: "msg-unknown", ProviderEmailID: "em_not_ours", Type: "email.bounced", OccurredAt: time.Now(),
	})
	if err != nil || res.Matched {
		t.Fatalf("Apply(unknown email) = %+v, %v; want unmatched, no error", res, err)
	}
}

func TestApply_NoActorMeansNoAlert(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	s := seed(t, pool, false)

	res, err := store.Apply(context.Background(), event(s, s.providerID+"-na", "email.bounced"))
	if err != nil || !res.Changed || res.Alerted {
		t.Fatalf("Apply(bounced, no actor) = %+v, %v; want changed but not alerted", res, err)
	}
}

func TestStatusForNotifications(t *testing.T) {
	pool := newPool(t)
	store := emailevents.NewPGStore(pool)
	ctx := context.Background()
	s := seed(t, pool, true)

	// Before any webhook: only the queue status is known.
	rows, err := store.StatusForNotifications(ctx, s.tenantID, []string{s.notificationID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("StatusForNotifications = %d rows, %v; want 1", len(rows), err)
	}
	if rows[0].QueueStatus != "sent" || rows[0].ProviderStatus != "" || rows[0].Recipient != "cust@example.com" {
		t.Fatalf("row before webhook = %+v", rows[0])
	}

	if _, err := store.Apply(ctx, event(s, s.providerID+"-st", "email.bounced")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rows, err = store.StatusForNotifications(ctx, s.tenantID, []string{s.notificationID})
	if err != nil || len(rows) != 1 || rows[0].ProviderStatus != emailevents.StatusBounced || rows[0].Detail == "" {
		t.Fatalf("row after bounce = %+v, %v; want bounced with detail", rows, err)
	}

	// Another tenant, or an unknown id, sees nothing.
	var otherTenant string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&otherTenant); err != nil {
		t.Fatalf("uuid: %v", err)
	}
	rows, err = store.StatusForNotifications(ctx, otherTenant, []string{s.notificationID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("cross-tenant read returned %d rows, %v; want 0", len(rows), err)
	}
}

// TestRoundTrip_CreateMarkSentApplyAndReadBack drives the paths the running
// service uses: notifications.Create with statusLink/statusResource, the
// worker's MarkSent(providerEmailID), a webhook Apply, and the delivery-log read.
func TestRoundTrip_CreateMarkSentApplyAndReadBack(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	var tenantID, actorID, resourceID string
	if err := pool.QueryRow(ctx,
		`SELECT gen_random_uuid()::text, gen_random_uuid()::text, gen_random_uuid()::text`).
		Scan(&tenantID, &actorID, &resourceID); err != nil {
		t.Fatalf("ids: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE tenant_id = $1`, tenantID)
	})

	n, err := notifications.NewPGStore(pool).Create(ctx, notifications.CreateInput{
		TenantID: tenantID, RecipientEmail: "cust@example.com", ActorUserID: actorID,
		EventType: "portal_user.invited", Resource: "portal_user", ResourceID: resourceID,
		Title: "Portal invite", StatusLink: "/crm/customer/c-1", StatusResource: "portal_access",
	})
	if err != nil {
		t.Fatalf("notifications.Create: %v", err)
	}

	deliveryStore := deliveries.NewPGStore(pool)
	d, err := deliveryStore.Enqueue(ctx, n.ID, tenantID, "", deliveries.ChannelEmail, deliveries.StatusPending)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	providerID := "em_rt_" + n.ID
	if err := deliveryStore.MarkSent(ctx, d.ID, nil, providerID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}

	res, err := emailevents.NewPGStore(pool).Apply(ctx, emailevents.Event{
		SvixID: providerID + "-1", ProviderEmailID: providerID, Type: "email.bounced",
		OccurredAt: time.Now(), Detail: "provider text",
	})
	if err != nil || !res.Matched || !res.Alerted {
		t.Fatalf("Apply = %+v, %v; want matched and alerted", res, err)
	}

	var resource, link string
	if err := pool.QueryRow(ctx, `
		SELECT resource, link FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND event_type = $3`,
		tenantID, actorID, emailevents.AlertEventType).Scan(&resource, &link); err != nil {
		t.Fatalf("read alert: %v", err)
	}
	if resource != "portal_access" || link != "/crm/customer/c-1" {
		t.Fatalf("alert resource/link = %q/%q, want portal_access and /crm/customer/c-1", resource, link)
	}

	rows, err := deliveryStore.ListForNotification(ctx, tenantID, n.ID)
	if err != nil {
		t.Fatalf("ListForNotification: %v", err)
	}
	var email *deliveries.Delivery
	for i := range rows {
		if rows[i].Channel == deliveries.ChannelEmail {
			email = &rows[i]
		}
	}
	if email == nil {
		t.Fatal("no email delivery row in the delivery log")
	}
	if email.Status != deliveries.StatusSent || email.ProviderStatus != emailevents.StatusBounced || email.ProviderStatusAt == nil {
		t.Fatalf("delivery = status %q providerStatus %q at %v; want sent/bounced with a timestamp",
			email.Status, email.ProviderStatus, email.ProviderStatusAt)
	}
}
