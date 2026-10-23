//go:build dbtest

package inbound_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"stonesuite-notify/database"
	"stonesuite-notify/inbound"
)

// The queue of received files that still have to be copied to the backend, run
// against a real database: what is claimed is claimed once, a retry waits its
// backoff, a job that gives up is kept without its record reference, and a retried
// webhook queues nothing twice.

func jobFixture(t *testing.T) (*inbound.PGJobStore, *pgxpool.Pool, string) {
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
	emailID := fmt.Sprintf("em_jobs_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM inbound_attachment_jobs WHERE email_id = $1`, emailID)
	})
	return inbound.NewPGJobStore(pool), pool, emailID
}

func countJobs(t *testing.T, pool *pgxpool.Pool, emailID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM inbound_attachment_jobs WHERE email_id = $1`, emailID).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// claimAll takes every due job of this mail (the table may hold other tests' jobs).
func claimAll(t *testing.T, store *inbound.PGJobStore, emailID string) []inbound.Job {
	t.Helper()
	claimed, err := store.ClaimDue(context.Background(), 1000, 5*time.Minute)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	var mine []inbound.Job
	for _, j := range claimed {
		if j.EmailID == emailID {
			mine = append(mine, j)
		}
	}
	return mine
}

func TestJobs_EnqueueIsIdempotent(t *testing.T) {
	store, pool, emailID := jobFixture(t)
	ctx := context.Background()

	if err := store.Enqueue(ctx, emailID, "r-abc", []string{"a", "b"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Resend retried the webhook: the backend still wants b, and now c as well.
	if err := store.Enqueue(ctx, emailID, "r-abc", []string{"b", "c"}); err != nil {
		t.Fatalf("Enqueue again: %v", err)
	}
	if err := store.Enqueue(ctx, emailID, "r-abc", nil); err != nil {
		t.Fatalf("Enqueue nothing: %v", err)
	}

	if n := countJobs(t, pool, emailID); n != 3 {
		t.Fatalf("jobs = %d, want 3 (a, b, c; b only once)", n)
	}
}

func TestJobs_ClaimDueTakesEachJobOnce(t *testing.T) {
	store, _, emailID := jobFixture(t)
	if err := store.Enqueue(context.Background(), emailID, "r-abc", []string{"a", "b"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	first := claimAll(t, store, emailID)
	second := claimAll(t, store, emailID)

	if len(first) != 2 {
		t.Fatalf("first claim = %d jobs, want 2", len(first))
	}
	if j := first[0]; j.UploadRef != "r-abc" || j.Attempts != 0 || j.ID == "" || (j.AttachmentID != "a" && j.AttachmentID != "b") {
		t.Fatalf("claimed job = %+v", j)
	}
	if len(second) != 0 {
		t.Fatalf("second claim = %d jobs, want none: they are already being processed", len(second))
	}
}

func TestJobs_ARetryWaitsItsBackoff(t *testing.T) {
	store, pool, emailID := jobFixture(t)
	ctx := context.Background()
	if err := store.Enqueue(ctx, emailID, "r-abc", []string{"a"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job := claimAll(t, store, emailID)[0]

	if err := store.Retry(ctx, job.ID, 1, "resend is down"); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	if due := claimAll(t, store, emailID); len(due) != 0 {
		t.Fatalf("a rescheduled job was due at once: %+v", due)
	}
	var status, lastError string
	var attempts int
	var next time.Time
	if err := pool.QueryRow(ctx, `SELECT status, attempts, last_error, next_attempt_at FROM inbound_attachment_jobs WHERE id = $1::uuid`, job.ID).
		Scan(&status, &attempts, &lastError, &next); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "pending" || attempts != 1 || lastError != "resend is down" {
		t.Fatalf("job = %s, %d attempts, %q", status, attempts, lastError)
	}
	if wait := time.Until(next); wait < 5*time.Second || wait > time.Minute {
		t.Fatalf("next attempt in %v, want about the first backoff step (15s)", wait)
	}

	// Once its time has come, it is claimed again, with the attempt count carried.
	if _, err := pool.Exec(ctx, `UPDATE inbound_attachment_jobs SET next_attempt_at = NOW() - interval '1 second' WHERE id = $1::uuid`, job.ID); err != nil {
		t.Fatalf("make due: %v", err)
	}
	again := claimAll(t, store, emailID)
	if len(again) != 1 || again[0].Attempts != 1 {
		t.Fatalf("claimed again = %+v, want the job with 1 attempt", again)
	}
}

func TestJobs_GivingUpKeepsTheJobWithoutItsRecordReference(t *testing.T) {
	for name, giveUp := range map[string]func(store *inbound.PGJobStore, id string) error{
		"the last allowed attempt": func(s *inbound.PGJobStore, id string) error {
			return s.Retry(context.Background(), id, inbound.MaxJobAttempts, "still down")
		},
		"a permanent failure": func(s *inbound.PGJobStore, id string) error {
			return s.Fail(context.Background(), id, "the backend rejected it")
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, pool, emailID := jobFixture(t)
			ctx := context.Background()
			if err := store.Enqueue(ctx, emailID, "r-secret-token", []string{"a"}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			job := claimAll(t, store, emailID)[0]

			if err := giveUp(store, job.ID); err != nil {
				t.Fatalf("giving up: %v", err)
			}

			var status, ref, lastError string
			if err := pool.QueryRow(ctx, `SELECT status, upload_ref, last_error FROM inbound_attachment_jobs WHERE id = $1::uuid`, job.ID).
				Scan(&status, &ref, &lastError); err != nil {
				t.Fatalf("read job: %v", err)
			}
			if status != "failed" || ref != "" || lastError == "" {
				t.Fatalf("job = %s, ref %q, error %q; want failed, no ref, the reason kept", status, ref, lastError)
			}
			if due := claimAll(t, store, emailID); len(due) != 0 {
				t.Fatalf("a job that gave up was claimed again: %+v", due)
			}
		})
	}
}

func TestJobs_CompleteRemovesTheJob(t *testing.T) {
	store, pool, emailID := jobFixture(t)
	ctx := context.Background()
	if err := store.Enqueue(ctx, emailID, "r-abc", []string{"a"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job := claimAll(t, store, emailID)[0]

	if err := store.Complete(ctx, job.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if n := countJobs(t, pool, emailID); n != 0 {
		t.Fatalf("jobs = %d, want 0 after completing", n)
	}
}

func TestJobs_AnAbandonedJobIsClaimedAgain(t *testing.T) {
	store, pool, emailID := jobFixture(t)
	ctx := context.Background()
	if err := store.Enqueue(ctx, emailID, "r-abc", []string{"a"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	job := claimAll(t, store, emailID)[0]

	// The machine stopped mid-copy: the job has sat "processing" for ten minutes.
	if _, err := pool.Exec(ctx, `UPDATE inbound_attachment_jobs SET updated_at = NOW() - interval '10 minutes' WHERE id = $1::uuid`, job.ID); err != nil {
		t.Fatalf("age job: %v", err)
	}

	again := claimAll(t, store, emailID)
	if len(again) != 1 || again[0].ID != job.ID {
		t.Fatalf("reclaimed = %+v, want the abandoned job", again)
	}
}
