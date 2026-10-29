package emailevents

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one verified provider webhook, reduced to what notify keeps.
type Event struct {
	// SvixID is the webhook's unique message id — the idempotency key.
	SvixID string
	// ProviderEmailID is the provider's id for the email (Resend's data.email_id).
	ProviderEmailID string
	// Type is the provider event type, e.g. "email.bounced".
	Type       string
	OccurredAt time.Time
	// Detail is a short provider diagnostic. It is stored server-side only.
	Detail string
}

// ApplyResult says what Apply did.
type ApplyResult struct {
	// Matched is false when no delivery carries the event's email id.
	Matched bool
	// Duplicate is true when this webhook (svix id) was already processed.
	Duplicate bool
	// Changed is true when the delivery's provider status moved forward.
	Changed bool
	// Alerted is true when an in-app alert was raised for the sender.
	Alerted bool
	// Status is the delivery's provider status after the event.
	Status string
}

// StatusRow is one email's raw status, before client-state derivation.
type StatusRow struct {
	NotificationID string
	Recipient      string
	QueueStatus    string
	ProviderStatus string
	Detail         string
	UpdatedAt      time.Time
}

// Store is the persistence boundary for provider email events. Handlers depend
// on this interface so they can be tested with a fake.
type Store interface {
	// Apply records one verified provider event: dedupe by svix id, advance the
	// delivery's provider status, and raise the sender alert on the first
	// problem state — all in one transaction.
	Apply(ctx context.Context, ev Event) (ApplyResult, error)
	// StatusForNotifications returns the email delivery status of each given
	// notification, scoped to the tenant. Ids with no email row are omitted.
	StatusForNotifications(ctx context.Context, tenantID string, notificationIDs []string) ([]StatusRow, error)
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// Apply implements Store.
func (s *PGStore) Apply(ctx context.Context, ev Event) (ApplyResult, error) {
	incoming, ok := StatusForEvent(ev.Type)
	if !ok {
		return ApplyResult{}, nil
	}
	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("begin email event tx: %w", err)
	}
	// Rollback after a successful Commit is a harmless ErrTxClosed; on every
	// early return it undoes the partial work.
	defer func() { _ = tx.Rollback(ctx) }()

	// FOR UPDATE serialises concurrent events for the same email.
	var deliveryID, tenantID, notificationID string
	var current *string
	err = tx.QueryRow(ctx, `
		SELECT id, tenant_id, notification_id, provider_status
		FROM notification_deliveries
		WHERE provider_email_id = $1
		FOR UPDATE`, ev.ProviderEmailID).Scan(&deliveryID, &tenantID, &notificationID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApplyResult{}, nil
	}
	if err != nil {
		return ApplyResult{}, fmt.Errorf("find delivery for provider email: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO email_events (svix_id, delivery_id, tenant_id, provider_email_id, event_type, occurred_at, detail)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
		ON CONFLICT (svix_id) DO NOTHING`,
		ev.SvixID, deliveryID, tenantID, ev.ProviderEmailID, ev.Type, occurredAt, ev.Detail)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("record email event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ApplyResult{Matched: true, Duplicate: true}, nil
	}

	currentStatus := ""
	if current != nil {
		currentStatus = *current
	}
	next, changed := Advance(currentStatus, incoming)
	res := ApplyResult{Matched: true, Changed: changed, Status: next}

	if changed {
		if _, err := tx.Exec(ctx, `
			UPDATE notification_deliveries
			SET provider_status = $2, provider_status_at = $3, provider_detail = NULLIF($4, ''), updated_at = NOW()
			WHERE id = $1`, deliveryID, next, occurredAt, ev.Detail); err != nil {
			return ApplyResult{}, fmt.Errorf("update provider status: %w", err)
		}
		if IsProblem(next) {
			res.Alerted, err = raiseAlert(ctx, tx, tenantID, notificationID, next)
			if err != nil {
				return ApplyResult{}, err
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ApplyResult{}, fmt.Errorf("commit email event: %w", err)
	}
	return res, nil
}

// raiseAlert inserts the in-app alert for the person who triggered the email,
// inside the caller's transaction. It writes the notifications and
// notification_deliveries rows directly (rather than through the notifications
// and deliveries stores) because the alert must commit or roll back together
// with the status change — otherwise a retried webhook could lose it.
// The alert's resource is what the bell's accessible_resources filter checks, so
// it is the RBAC resource that governs the status page (status_resource, set by
// the caller), falling back to the original's own resource when none was given.
// A system-originated email (no actor) raises nothing.
func raiseAlert(ctx context.Context, tx pgx.Tx, tenantID, notificationID, status string) (bool, error) {
	var actor *string
	var resource, resourceID, recipient, statusLink, statusResource string
	err := tx.QueryRow(ctx, `
		SELECT actor_user_id, resource, resource_id, recipient_email, status_link, status_resource
		FROM notifications
		WHERE id = $1 AND tenant_id = $2`, notificationID, tenantID).
		Scan(&actor, &resource, &resourceID, &recipient, &statusLink, &statusResource)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load original notification: %w", err)
	}
	if actor == nil || *actor == "" {
		return false, nil
	}
	alertResource := resource
	if statusResource != "" {
		alertResource = statusResource
	}

	var alertID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, event_type, resource, resource_id, title, body, link, visible_in_app)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
		RETURNING id`,
		tenantID, *actor, AlertEventType, alertResource, resourceID,
		AlertTitle(status, recipient), AlertBody(status), statusLink).Scan(&alertID); err != nil {
		return false, fmt.Errorf("insert delivery alert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries (notification_id, tenant_id, recipient_user_id, channel, status)
		VALUES ($1, $2, $3, 'in_app', 'sent')`, alertID, tenantID, *actor); err != nil {
		return false, fmt.Errorf("insert delivery alert log: %w", err)
	}
	return true, nil
}

// StatusForNotifications implements Store. The tenant predicate is part of the
// query, so a notification id from another tenant yields nothing.
func (s *PGStore) StatusForNotifications(ctx context.Context, tenantID string, notificationIDs []string) ([]StatusRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.notification_id::text, n.recipient_email, d.status,
		       COALESCE(d.provider_status, ''),
		       COALESCE(d.provider_detail, d.last_error, ''),
		       COALESCE(d.provider_status_at, d.updated_at)
		FROM notification_deliveries d
		JOIN notifications n ON n.id = d.notification_id AND n.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1
		  AND d.channel = 'email'
		  AND d.notification_id = ANY($2::text[]::uuid[])`, tenantID, notificationIDs)
	if err != nil {
		return nil, fmt.Errorf("query email status: %w", err)
	}
	defer rows.Close()

	out := []StatusRow{}
	for rows.Next() {
		var r StatusRow
		if err := rows.Scan(&r.NotificationID, &r.Recipient, &r.QueueStatus, &r.ProviderStatus, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan email status: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate email status: %w", err)
	}
	return out, nil
}
