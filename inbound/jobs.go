package inbound

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// MaxJobAttempts is how many times a file is tried before its job gives up.
	MaxJobAttempts = 8

	jobStatusPending    = "pending"
	jobStatusProcessing = "processing"
	jobStatusFailed     = "failed"

	jobBackoffBase = 15 * time.Second
	jobBackoffMax  = 30 * time.Minute
)

// Job is one received file waiting to be copied to the backend.
type Job struct {
	ID           string
	EmailID      string
	AttachmentID string
	// UploadRef is the reply-address token the backend asked the file to be sent under.
	UploadRef string
	// Attempts is how many times the file has been tried so far.
	Attempts int
}

// JobStore is the durable queue of files to copy. The webhook only enqueues; a
// worker claims, copies and settles.
type JobStore interface {
	// Enqueue queues the files (the provider's attachment ids) of one received
	// mail. A file already queued, in progress or given up on is left alone, so a
	// retried webhook adds nothing twice.
	Enqueue(ctx context.Context, emailID, uploadRef string, attachmentIDs []string) error
	// ClaimDue takes up to batchSize jobs that are due, including ones left
	// "processing" for staleAfter or longer (a crash mid-copy).
	ClaimDue(ctx context.Context, batchSize int, staleAfter time.Duration) ([]Job, error)
	// Complete removes a job whose file was copied (or can never be).
	Complete(ctx context.Context, id string) error
	// Retry schedules another attempt after the backoff for the given attempt
	// count, or gives up once the count reaches MaxJobAttempts.
	Retry(ctx context.Context, id string, attempts int, lastError string) error
	// Fail gives up on a job at once: the error is permanent.
	Fail(ctx context.Context, id string, lastError string) error
}

// JobBackoff is the wait before the next attempt, given how many have been made:
// 15s, 30s, 1m, 2m ... capped at 30 minutes.
func JobBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := jobBackoffBase
	for i := 1; i < attempts && d < jobBackoffMax; i++ {
		d *= 2
	}
	return min(d, jobBackoffMax)
}

// PGJobStore implements JobStore against Postgres.
type PGJobStore struct {
	pool *pgxpool.Pool
}

// NewPGJobStore builds a PGJobStore backed by the given pool.
func NewPGJobStore(pool *pgxpool.Pool) *PGJobStore { return &PGJobStore{pool: pool} }

// Enqueue implements JobStore.
func (s *PGJobStore) Enqueue(ctx context.Context, emailID, uploadRef string, attachmentIDs []string) error {
	if len(attachmentIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO inbound_attachment_jobs (email_id, attachment_id, upload_ref)
		SELECT $1, id, $2 FROM unnest($3::text[]) AS id
		ON CONFLICT (email_id, attachment_id) DO NOTHING`,
		emailID, uploadRef, attachmentIDs)
	if err != nil {
		return fmt.Errorf("enqueue attachment jobs for %s: %w", emailID, err)
	}
	return nil
}

// ClaimDue implements JobStore.
func (s *PGJobStore) ClaimDue(ctx context.Context, batchSize int, staleAfter time.Duration) ([]Job, error) {
	staleBefore := time.Now().Add(-staleAfter)
	rows, err := s.pool.Query(ctx, `
		UPDATE inbound_attachment_jobs SET status = $3, updated_at = NOW()
		WHERE id IN (
			SELECT id FROM inbound_attachment_jobs
			WHERE (status = $4 AND next_attempt_at <= NOW())
			   OR (status = $3 AND updated_at <= $2)
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, email_id, attachment_id, upload_ref, attempts`,
		batchSize, staleBefore, jobStatusProcessing, jobStatusPending)
	if err != nil {
		return nil, fmt.Errorf("claim attachment jobs: %w", err)
	}
	defer rows.Close()

	out := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.EmailID, &j.AttachmentID, &j.UploadRef, &j.Attempts); err != nil {
			return nil, fmt.Errorf("scan claimed attachment job: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed attachment jobs: %w", err)
	}
	return out, nil
}

// Complete implements JobStore.
func (s *PGJobStore) Complete(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM inbound_attachment_jobs WHERE id = $1::uuid`, id); err != nil {
		return fmt.Errorf("complete attachment job %s: %w", id, err)
	}
	return nil
}

// Retry implements JobStore.
func (s *PGJobStore) Retry(ctx context.Context, id string, attempts int, lastError string) error {
	if attempts >= MaxJobAttempts {
		return s.giveUp(ctx, id, attempts, lastError)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE inbound_attachment_jobs
		SET status = $2, attempts = $3, next_attempt_at = $4, last_error = $5, updated_at = NOW()
		WHERE id = $1::uuid`,
		id, jobStatusPending, attempts, time.Now().Add(JobBackoff(attempts)), lastError)
	if err != nil {
		return fmt.Errorf("reschedule attachment job %s: %w", id, err)
	}
	return nil
}

// Fail implements JobStore.
func (s *PGJobStore) Fail(ctx context.Context, id string, lastError string) error {
	return s.giveUp(ctx, id, -1, lastError)
}

// giveUp marks a job failed and keeps it for whoever looks into it. The upload
// reference names a record, so it is cleared with the job's last chance. A
// negative attempts leaves the count as it was.
func (s *PGJobStore) giveUp(ctx context.Context, id string, attempts int, lastError string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE inbound_attachment_jobs
		SET status = $2, upload_ref = '', attempts = CASE WHEN $3 >= 0 THEN $3 ELSE attempts END,
		    last_error = $4, updated_at = NOW()
		WHERE id = $1::uuid`,
		id, jobStatusFailed, attempts, lastError)
	if err != nil {
		return fmt.Errorf("give up on attachment job %s: %w", id, err)
	}
	return nil
}
