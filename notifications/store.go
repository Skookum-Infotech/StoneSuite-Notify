package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a notification lookup matches no row for the
// given (tenant, recipient) — including when it exists but belongs to
// someone else, so callers can't distinguish "missing" from "not yours".
var ErrNotFound = errors.New("notification not found")

// Store is the persistence boundary for notifications. Handlers depend on
// this interface (not *PGStore) so they can be tested with a fake store.
type Store interface {
	Create(ctx context.Context, in CreateInput) (*Notification, error)
	// Get loads a single notification by id, scoped to tenant only (not
	// recipient) — used by the delivery workers to read title/body/link/
	// recipient_email fresh at send time via notification_id.
	Get(ctx context.Context, tenantID, id string) (*Notification, error)
	// ListForUser returns one page of the recipient's feed, newest first.
	// The bell dropdown passes offset 0; the "view all" screen pages
	// through with a non-zero offset. allowedResources restricts rows to
	// those whose `resource` is in the list; an empty list means
	// unrestricted (see UserContext.AccessibleResources).
	ListForUser(ctx context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool, limit, offset int) ([]Notification, error)
	// CountForUser returns how many rows ListForUser would return in total
	// under the same filter, ignoring paging — the denominator the "view
	// all" screen needs to render page controls.
	CountForUser(ctx context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool) (int, error)
	UnreadCount(ctx context.Context, tenantID, recipientUserID string, allowedResources []string) (int, error)
	MarkRead(ctx context.Context, tenantID, recipientUserID, id string) error
	// MarkAllRead marks every unread row within allowedResources as read,
	// so resource-restricted rows are never marked read without the
	// recipient having actually been able to see them.
	MarkAllRead(ctx context.Context, tenantID, recipientUserID string, allowedResources []string) error
	// SaveAttachment persists the single attachment for a notification's
	// email delivery. Called at most once per notification, from
	// controllers.Handler.Create, only when email delivery was actually
	// requested for that recipient.
	SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error
	// GetAttachment loads a notification's attachment, if any. Returns
	// (nil, nil) — not an error — when the notification has none, since
	// the overwhelming majority of notifications never do.
	GetAttachment(ctx context.Context, tenantID, notificationID string) (*AttachmentInput, error)
}

// PGStore implements Store against Postgres.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore builds a PGStore backed by the given pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const notificationColumns = `id, tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app, read_at, created_at`

func scanNotification(row pgx.Row) (*Notification, error) {
	var n Notification
	var recipientUserID *string
	var actorUserID *string
	if err := row.Scan(
		&n.ID, &n.TenantID, &recipientUserID, &n.RecipientEmail, &actorUserID,
		&n.EventType, &n.Resource, &n.ResourceID, &n.Title, &n.Body, &n.Link, &n.VisibleInApp,
		&n.ReadAt, &n.CreatedAt,
	); err != nil {
		return nil, err
	}
	if recipientUserID != nil {
		n.RecipientUserID = *recipientUserID
	}
	if actorUserID != nil {
		n.ActorUserID = *actorUserID
	}
	return &n, nil
}

// Create inserts one notification row. The row is always written
// regardless of in.VisibleInApp — that flag only gates feed/bell
// visibility, never row existence, since email/push delivery reads this
// row's content via Get regardless of the in-app toggle.
func (s *PGStore) Create(ctx context.Context, in CreateInput) (*Notification, error) {
	var actorUserIDArg any
	if in.ActorUserID != "" {
		actorUserIDArg = in.ActorUserID
	}
	var recipientUserIDArg any
	if in.RecipientUserID != "" {
		recipientUserIDArg = in.RecipientUserID
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app, status_link, status_resource)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+notificationColumns,
		in.TenantID, recipientUserIDArg, in.RecipientEmail, actorUserIDArg, in.EventType, in.Resource, in.ResourceID, in.Title, in.Body, in.Link, in.VisibleInApp, in.StatusLink, in.StatusResource)

	n, err := scanNotification(row)
	if err != nil {
		return nil, fmt.Errorf("create notification: %w", err)
	}
	return n, nil
}

// Get loads a single notification by id, scoped to tenant only — see the
// Store interface doc for why this is not additionally scoped to recipient.
func (s *PGStore) Get(ctx context.Context, tenantID, id string) (*Notification, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+notificationColumns+`
		FROM notifications WHERE id = $1 AND tenant_id = $2`,
		id, tenantID)

	n, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get notification %s: %w", id, err)
	}
	return n, nil
}

// nonNilResources returns allowedResources unchanged, except a nil slice
// becomes a non-nil empty one. pgx binds a nil Go slice as SQL NULL for a
// text[] parameter, and cardinality(NULL::text[]) is NULL, not 0 — so the
// query's "cardinality($n::text[]) = 0" unrestricted-fallback silently
// evaluates to NULL (falsy in a WHERE clause) instead of true, turning
// "no restriction" into "match nothing". A real empty array avoids that:
// cardinality('{}'::text[]) = 0. allowedResources is nil for every caller
// until StoneSuite-Backend mints a non-empty accessible_resources claim, so
// this affected every account, not just resource-restricted ones.
func nonNilResources(allowedResources []string) []string {
	if allowedResources == nil {
		return []string{}
	}
	return allowedResources
}

// ListForUser returns one page of the recipient's notifications, newest
// first.
func (s *PGStore) ListForUser(ctx context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool, limit, offset int) ([]Notification, error) {
	allowedResources = nonNilResources(allowedResources)
	limit, offset = NormalizePaging(limit, offset)

	query := `SELECT ` + notificationColumns + `
		FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true
		AND (cardinality($3::text[]) = 0 OR resource = ANY($3::text[]))`
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}
	// id breaks ties so paging is stable when several rows share a
	// created_at — without it a row can repeat on one page and be skipped
	// on the next.
	query += ` ORDER BY created_at DESC, id DESC LIMIT $4 OFFSET $5`

	rows, err := s.pool.Query(ctx, query, tenantID, recipientUserID, allowedResources, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	notifications := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan notification row: %w", err)
		}
		notifications = append(notifications, *n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notifications: %w", err)
	}
	return notifications, nil
}

// CountForUser returns the total number of feed rows matching the same
// filter ListForUser applies, ignoring paging.
func (s *PGStore) CountForUser(ctx context.Context, tenantID, recipientUserID string, allowedResources []string, unreadOnly bool) (int, error) {
	allowedResources = nonNilResources(allowedResources)
	query := `SELECT COUNT(*) FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true
		AND (cardinality($3::text[]) = 0 OR resource = ANY($3::text[]))`
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}

	var count int
	if err := s.pool.QueryRow(ctx, query, tenantID, recipientUserID, allowedResources).Scan(&count); err != nil {
		return 0, fmt.Errorf("count notifications: %w", err)
	}
	return count, nil
}

// UnreadCount returns the number of unread notifications for the recipient —
// the value the frontend bell polls.
func (s *PGStore) UnreadCount(ctx context.Context, tenantID, recipientUserID string, allowedResources []string) (int, error) {
	allowedResources = nonNilResources(allowedResources)
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM notifications
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND visible_in_app = true AND read_at IS NULL
		AND (cardinality($3::text[]) = 0 OR resource = ANY($3::text[]))`,
		tenantID, recipientUserID, allowedResources).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count unread notifications: %w", err)
	}
	return count, nil
}

// MarkRead marks a single notification read, scoped to the caller so no one
// can mark (or even probe the existence of) another user's notification.
func (s *PGStore) MarkRead(ctx context.Context, tenantID, recipientUserID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications SET read_at = NOW()
		WHERE id = $1 AND tenant_id = $2 AND recipient_user_id = $3 AND read_at IS NULL`,
		id, tenantID, recipientUserID)
	if err != nil {
		return fmt.Errorf("mark notification %s read: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead marks every unread notification within allowedResources for
// the recipient as read (empty allowedResources means unrestricted).
func (s *PGStore) MarkAllRead(ctx context.Context, tenantID, recipientUserID string, allowedResources []string) error {
	allowedResources = nonNilResources(allowedResources)
	if _, err := s.pool.Exec(ctx, `
		UPDATE notifications SET read_at = NOW()
		WHERE tenant_id = $1 AND recipient_user_id = $2 AND read_at IS NULL
		AND (cardinality($3::text[]) = 0 OR resource = ANY($3::text[]))`,
		tenantID, recipientUserID, allowedResources); err != nil {
		return fmt.Errorf("mark all notifications read: %w", err)
	}
	return nil
}

// SaveAttachment inserts (or replaces, on the rare case of a retry)
// notification_id's attachment row. content defaults to an empty (not
// NULL) byte slice when only a.EmailBodyHTML is set and there is no actual
// file, since the column is NOT NULL.
func (s *PGStore) SaveAttachment(ctx context.Context, notificationID string, a AttachmentInput) error {
	content := a.Content
	if content == nil {
		content = []byte{}
	}
	options, err := json.Marshal(a.EmailOptions)
	if err != nil {
		return fmt.Errorf("encode email options for notification %s: %w", notificationID, err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO notification_attachments (notification_id, file_name, content_type, content, email_body_html, email_options)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (notification_id) DO UPDATE
			SET file_name = EXCLUDED.file_name,
			    content_type = EXCLUDED.content_type,
			    content = EXCLUDED.content,
			    email_body_html = EXCLUDED.email_body_html,
			    email_options = EXCLUDED.email_options`,
		notificationID, a.FileName, a.ContentType, content, a.EmailBodyHTML, options)
	if err != nil {
		return fmt.Errorf("save notification attachment %s: %w", notificationID, err)
	}
	return nil
}

// GetAttachment loads notificationID's attachment, scoped to tenant like
// Get — defense in depth even though notification_id is already a
// globally-unique UUID, matching this store's existing scoping discipline.
// Returns (nil, nil) when the notification has no attachment row, which is
// the common case.
func (s *PGStore) GetAttachment(ctx context.Context, tenantID, notificationID string) (*AttachmentInput, error) {
	var a AttachmentInput
	var options []byte
	err := s.pool.QueryRow(ctx, `
		SELECT na.file_name, na.content_type, na.content, na.email_body_html, na.email_options
		FROM notification_attachments na
		JOIN notifications n ON n.id = na.notification_id
		WHERE na.notification_id = $1 AND n.tenant_id = $2`,
		notificationID, tenantID).Scan(&a.FileName, &a.ContentType, &a.Content, &a.EmailBodyHTML, &options)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification attachment %s: %w", notificationID, err)
	}
	if len(options) > 0 {
		if err := json.Unmarshal(options, &a.EmailOptions); err != nil {
			return nil, fmt.Errorf("decode email options for notification %s: %w", notificationID, err)
		}
	}
	return &a, nil
}
