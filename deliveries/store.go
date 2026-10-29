package deliveries

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the persistence boundary for notification_deliveries. Both the
// controllers package (enqueue) and the workers package (claim + mark)
// depend on this interface, not *PGStore, so both are testable with fakes.
type Store interface {
	Enqueue(ctx context.Context, notificationID, tenantID, recipientUserID, channel, status string) (*Delivery, error)
	// ClaimPending atomically claims up to batchSize rows with
	// status='pending', flipping them to 'processing' in the same
	// statement (FOR UPDATE SKIP LOCKED) so concurrent workers never
	// double-claim.
	ClaimPending(ctx context.Context, batchSize int) ([]Delivery, error)
	// ClaimDue atomically claims rows due for retry (status='retrying'
	// past next_attempt_at) plus any row stuck in 'processing' longer
	// than staleProcessingAfter (a crash mid-send), recovering the latter
	// as if it were a failed attempt.
	ClaimDue(ctx context.Context, batchSize int, staleProcessingAfter time.Duration) ([]Delivery, error)
	// MarkSent finalizes a delivery as sent. providerEmailID is the email
	// provider's id for the message (Resend), recorded so its delivery
	// webhooks can be matched to this row; "" when there is none (push, SMTP).
	MarkSent(ctx context.Context, id string, providerResponse []byte, providerEmailID string) error
	// MarkSkipped finalizes a delivery as skipped — nothing to deliver to
	// (e.g. no push subscriptions) — which is not a failure and is never
	// retried.
	MarkSkipped(ctx context.Context, id string, providerResponse []byte) error
	// MarkRetrying records a failed attempt: increments attempts and
	// either schedules the next attempt with exponential backoff, or,
	// once attempts reaches MaxAttempts, terminates the row as failed.
	MarkRetrying(ctx context.Context, id string, attempts int, lastError string, providerResponse []byte) error
	// ListForNotification returns one notification's delivery rows. It is
	// scoped by tenant as well as notification id so holding a notification
	// id from one tenant can never surface another tenant's delivery log.
	ListForNotification(ctx context.Context, tenantID, notificationID string) ([]Delivery, error)
	// ListByStatus returns a tenant's delivery rows in the given status,
	// updated at or after `since`, newest first, capped at `limit`. Backs
	// the "show me what failed" ops/admin view — the counterpart to
	// ListForNotification for when the notification id isn't already known.
	ListByStatus(ctx context.Context, tenantID, status string, since time.Time, limit int) ([]Delivery, error)
}

// MaxListByStatusLimit caps one ListByStatus page so no caller can pull an
// unbounded delivery history in a single request.
const MaxListByStatusLimit = 200

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const deliveryColumns = `id, notification_id, tenant_id, recipient_user_id, channel, status, attempts, max_attempts, next_attempt_at, last_error, provider_response, provider_status, provider_status_at, created_at, updated_at`

func scanDelivery(row pgx.Row) (*Delivery, error) {
	var d Delivery
	var recipientUserID *string
	var lastError *string
	var providerStatus *string
	if err := row.Scan(
		&d.ID, &d.NotificationID, &d.TenantID, &recipientUserID, &d.Channel, &d.Status,
		&d.Attempts, &d.MaxAttempts, &d.NextAttemptAt, &lastError, &d.ProviderResponse,
		&providerStatus, &d.ProviderStatusAt,
		&d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if providerStatus != nil {
		d.ProviderStatus = *providerStatus
	}
	if recipientUserID != nil {
		d.RecipientUserID = *recipientUserID
	}
	if lastError != nil {
		d.LastError = *lastError
	}
	return &d, nil
}

// Enqueue inserts one delivery row.
func (s *PGStore) Enqueue(ctx context.Context, notificationID, tenantID, recipientUserID, channel, status string) (*Delivery, error) {
	var recipientUserIDArg any
	if recipientUserID != "" {
		recipientUserIDArg = recipientUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notification_deliveries (notification_id, tenant_id, recipient_user_id, channel, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+deliveryColumns,
		notificationID, tenantID, recipientUserIDArg, channel, status)

	d, err := scanDelivery(row)
	if err != nil {
		return nil, fmt.Errorf("enqueue delivery: %w", err)
	}
	return d, nil
}

func (s *PGStore) ClaimPending(ctx context.Context, batchSize int) ([]Delivery, error) {
	return s.claim(ctx, `
		UPDATE notification_deliveries SET status = 'processing', updated_at = NOW()
		WHERE id IN (
			SELECT id FROM notification_deliveries
			WHERE status = 'pending' AND next_attempt_at <= NOW()
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+deliveryColumns, batchSize)
}

func (s *PGStore) ClaimDue(ctx context.Context, batchSize int, staleProcessingAfter time.Duration) ([]Delivery, error) {
	staleBefore := time.Now().Add(-staleProcessingAfter)

	// A row stuck in 'processing' was an attempt that never reported back
	// (crash, panic). Count it, and terminate rows that have used their whole
	// budget so a poison delivery cannot be reclaimed forever.
	if _, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = 'failed', attempts = attempts + 1,
		    last_error = 'abandoned while processing (worker crashed or timed out)', updated_at = NOW()
		WHERE status = 'processing' AND updated_at <= $1 AND attempts + 1 >= $2`,
		staleBefore, MaxAttempts); err != nil {
		return nil, fmt.Errorf("fail exhausted stale deliveries: %w", err)
	}

	return s.claim(ctx, `
		UPDATE notification_deliveries
		SET attempts = CASE WHEN status = 'processing' THEN attempts + 1 ELSE attempts END,
		    status = 'processing', updated_at = NOW()
		WHERE id IN (
			SELECT id FROM notification_deliveries
			WHERE (status = 'retrying' AND next_attempt_at <= NOW())
			   OR (status = 'processing' AND updated_at <= $2)
			ORDER BY created_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+deliveryColumns, batchSize, staleBefore)
}

func (s *PGStore) claim(ctx context.Context, query string, args ...any) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	defer rows.Close()

	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("scan claimed delivery: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed deliveries: %w", err)
	}
	return out, nil
}

func (s *PGStore) MarkSent(ctx context.Context, id string, providerResponse []byte, providerEmailID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = 'sent', provider_response = $2, provider_email_id = NULLIF($3, ''), updated_at = NOW()
		WHERE id = $1 AND status = 'processing'`, id, providerResponse, providerEmailID); err != nil {
		return fmt.Errorf("mark delivery %s sent: %w", id, err)
	}
	return nil
}

func (s *PGStore) MarkSkipped(ctx context.Context, id string, providerResponse []byte) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = 'skipped', provider_response = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'processing'`, id, providerResponse); err != nil {
		return fmt.Errorf("mark delivery %s skipped: %w", id, err)
	}
	return nil
}

func (s *PGStore) MarkRetrying(ctx context.Context, id string, attempts int, lastError string, providerResponse []byte) error {
	status := StatusRetrying
	if attempts >= MaxAttempts {
		status = StatusFailed
	}
	nextAttempt := time.Now().Add(backoff(attempts))

	if _, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = $2, attempts = $3, next_attempt_at = $4, last_error = $5, provider_response = COALESCE($6, provider_response), updated_at = NOW()
		WHERE id = $1 AND status = 'processing'`,
		id, status, attempts, nextAttempt, lastError, providerResponse); err != nil {
		return fmt.Errorf("mark delivery %s retrying: %w", id, err)
	}
	return nil
}

// backoff computes the delay before the next attempt: min(30m, 1m*2^(n-1))
// where n is the attempt count after increment.
func backoff(attempts int) time.Duration {
	d := time.Minute * time.Duration(math.Pow(2, float64(attempts-1)))
	if d > 30*time.Minute {
		return 30 * time.Minute
	}
	return d
}

// ListByStatus returns a tenant's deliveries in `status` updated since
// `since`, newest first, capped at min(limit, MaxListByStatusLimit).
func (s *PGStore) ListByStatus(ctx context.Context, tenantID, status string, since time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > MaxListByStatusLimit {
		limit = MaxListByStatusLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+deliveryColumns+`
		FROM notification_deliveries
		WHERE tenant_id = $1 AND status = $2 AND updated_at >= $3
		ORDER BY updated_at DESC
		LIMIT $4`, tenantID, status, since, limit)
	if err != nil {
		return nil, fmt.Errorf("list deliveries by status %s: %w", status, err)
	}
	defer rows.Close()

	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("scan delivery row: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return out, nil
}

// ListForNotification returns every delivery row for one notification,
// oldest first — backs the support/debugging delivery-log endpoint.
func (s *PGStore) ListForNotification(ctx context.Context, tenantID, notificationID string) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+deliveryColumns+`
		FROM notification_deliveries
		WHERE tenant_id = $1 AND notification_id = $2
		ORDER BY created_at`, tenantID, notificationID)
	if err != nil {
		return nil, fmt.Errorf("list deliveries for notification %s: %w", notificationID, err)
	}
	defer rows.Close()

	out := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("scan delivery row: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return out, nil
}
