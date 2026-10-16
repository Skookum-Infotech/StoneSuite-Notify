//go:build dbtest

package notifications_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"stonesuite-notify/database"
	"stonesuite-notify/notifications"
)

// Fixed UUIDs: tenant_id, recipient_user_id and resource_id are UUID NOT NULL.
const (
	testTenantID   = "00000000-0000-4000-8000-000000000001"
	testUserID     = "00000000-0000-4000-8000-000000000002"
	testResourceID = "00000000-0000-4000-8000-000000000003"
)

// newTestStore connects to TEST_DATABASE_URL (skipping when unset), applies the
// embedded schema and returns a PGStore on it.
func newTestStore(t *testing.T) (*notifications.PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := database.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM notifications WHERE tenant_id = $1`, testTenantID); err != nil {
		t.Fatalf("reset fixtures: %v", err)
	}
	return notifications.NewPGStore(pool), pool
}

func idempotentInput(key string) notifications.CreateInput {
	return notifications.CreateInput{
		TenantID: testTenantID, RecipientUserID: testUserID, RecipientEmail: "a@b.com",
		EventType: "automation.reminder", Resource: "invoice", ResourceID: testResourceID,
		Title: "Invoice due", IdempotencyKey: key,
	}
}

func TestCreateIdempotent_ReplayReturnsSameRow(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	in := idempotentInput("rule:invoice:42:2026-10-20:0:" + testUserID + ":a@b.com")
	first, created1, err := store.CreateIdempotent(ctx, in)
	if err != nil || !created1 {
		t.Fatalf("first create: created=%v err=%v", created1, err)
	}
	second, created2, err := store.CreateIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created2 {
		t.Fatal("replay must not create a second row")
	}
	if first.ID != second.ID {
		t.Fatalf("replay id = %s, want %s", second.ID, first.ID)
	}

	in.IdempotencyKey = ""
	a, _, err := store.CreateIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("keyless a: %v", err)
	}
	b, _, err := store.CreateIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("keyless b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("no key must keep today's behaviour: two distinct rows")
	}
}

func TestCreateIdempotent_EmailOnlyRecipient(t *testing.T) {
	ctx := context.Background()
	store, _ := newTestStore(t)
	in := idempotentInput("rule:invoice:43:email-only")
	in.RecipientUserID = ""
	first, created1, err := store.CreateIdempotent(ctx, in)
	if err != nil || !created1 {
		t.Fatalf("first create: created=%v err=%v", created1, err)
	}
	second, created2, err := store.CreateIdempotent(ctx, in)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if created2 || first.ID != second.ID {
		t.Fatalf("replay: created=%v ids %s/%s", created2, first.ID, second.ID)
	}
}

func TestCreateIdempotent_ConcurrentSameKeyYieldsOneRow(t *testing.T) {
	ctx := context.Background()
	store, pool := newTestStore(t)
	in := idempotentInput("rule:invoice:44:concurrent")

	const workers = 8
	var wg sync.WaitGroup
	ids := make([]string, workers)
	news := make([]bool, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, isNew, err := store.CreateIdempotent(ctx, in)
			errs[i], news[i] = err, isNew
			if n != nil {
				ids[i] = n.ID
			}
		}(i)
	}
	wg.Wait()

	newCount := 0
	for i := 0; i < workers; i++ {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("worker %d id = %s, want %s", i, ids[i], ids[0])
		}
		if news[i] {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("%d callers created the row, want exactly 1", newCount)
	}
	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE tenant_id = $1 AND idempotency_key = $2`,
		testTenantID, in.IdempotencyKey).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d, want 1", rows)
	}
}
