//go:build dbtest

package notifications_test

import (
	"context"
	"os"
	"testing"

	"stonesuite-notify/database"
	"stonesuite-notify/notifications"
)

// The per-message email options (Reply-To, sender name, thread headers) ride in
// the notification's attachment row from the create request to the delivery
// worker, which reads them back. These prove that round trip against a real
// database, and that rows written before the column existed read as "no
// override".

func newStore(t *testing.T) (store *notifications.PGStore, notificationID, tenantID string) {
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
	var resourceID string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&tenantID, &resourceID); err != nil {
		t.Fatalf("ids: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM notifications WHERE tenant_id = $1`, tenantID)
	})
	store = notifications.NewPGStore(pool)
	n, err := store.Create(ctx, notifications.CreateInput{
		TenantID: tenantID, RecipientEmail: "customer@example.com", EventType: "document.sent",
		Resource: "sales_order", ResourceID: resourceID, Title: "Your Sales Order SO-1",
	})
	if err != nil {
		t.Fatalf("create notification: %v", err)
	}
	return store, n.ID, tenantID
}

func TestAttachment_EmailOptionsRoundTrip(t *testing.T) {
	store, id, tenantID := newStore(t)
	ctx := context.Background()

	want := notifications.EmailOptions{
		ReplyTo:    "r-abcdefghijklmnopqrst@reply.example.com",
		FromName:   `Harish "H" Narne (Acme, Stone)`,
		InReplyTo:  "<cust-1@mail.gmail.com>",
		References: "<a@x> <cust-1@mail.gmail.com>",
	}
	if err := store.SaveAttachment(ctx, id, notifications.AttachmentInput{EmailBodyHTML: "<p>hi</p>", EmailOptions: want}); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}

	got, err := store.GetAttachment(ctx, tenantID, id)
	if err != nil || got == nil {
		t.Fatalf("GetAttachment = %v, %v", got, err)
	}
	if got.EmailOptions != want {
		t.Fatalf("options did not round trip:\n got %+v\nwant %+v", got.EmailOptions, want)
	}
	if got.EmailBodyHTML != "<p>hi</p>" {
		t.Fatalf("email body lost: %q", got.EmailBodyHTML)
	}

	// A retry replaces the row, including the options.
	if err := store.SaveAttachment(ctx, id, notifications.AttachmentInput{EmailBodyHTML: "<p>v2</p>"}); err != nil {
		t.Fatalf("SaveAttachment (replace): %v", err)
	}
	got, err = store.GetAttachment(ctx, tenantID, id)
	if err != nil || got == nil {
		t.Fatalf("GetAttachment = %v, %v", got, err)
	}
	if !got.EmailOptions.IsZero() {
		t.Fatalf("a replaced row must not keep the old options: %+v", got.EmailOptions)
	}
}

func TestAttachment_RowWithoutOptionsReadsAsNoOverride(t *testing.T) {
	store, id, tenantID := newStore(t)

	if err := store.SaveAttachment(context.Background(), id, notifications.AttachmentInput{FileName: "SO-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}
	got, err := store.GetAttachment(context.Background(), tenantID, id)
	if err != nil || got == nil {
		t.Fatalf("GetAttachment = %v, %v", got, err)
	}
	if !got.EmailOptions.IsZero() {
		t.Fatalf("expected no overrides, got %+v", got.EmailOptions)
	}
	if got.FileName != "SO-1.pdf" {
		t.Fatalf("file lost: %q", got.FileName)
	}
}

func TestAttachment_NotReadableUnderAnotherTenant(t *testing.T) {
	store, id, _ := newStore(t)
	if err := store.SaveAttachment(context.Background(), id, notifications.AttachmentInput{EmailBodyHTML: "x"}); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}
	got, err := store.GetAttachment(context.Background(), "00000000-0000-0000-0000-000000000000", id)
	if err != nil || got != nil {
		t.Fatalf("another tenant must see nothing, got %v, %v", got, err)
	}
}
