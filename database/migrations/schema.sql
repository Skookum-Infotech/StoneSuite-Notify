-- Notifications schema. Editing this file IS the migration: every statement
-- is idempotent (CREATE ... IF NOT EXISTS) so ApplySchema can run on every
-- boot against any existing DB state, mirroring StoneSuite-Backend's
-- tenant-schema convention.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS notifications (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    recipient_user_id UUID NOT NULL,
    actor_user_id     UUID,
    event_type        VARCHAR(64)  NOT NULL,
    resource          VARCHAR(64)  NOT NULL,
    resource_id       UUID         NOT NULL,
    title             VARCHAR(200) NOT NULL,
    body              VARCHAR(500) NOT NULL DEFAULT '',
    link              VARCHAR(300) NOT NULL DEFAULT '',
    read_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Added for the reliability layer: recipient_email is captured once, at
-- creation time, so email/push delivery workers can read it fresh via
-- notification_id without duplicating (and risking staleness of) an
-- address on notification_deliveries. visible_in_app controls only the
-- feed/bell visibility of a row that always exists — see the design doc —
-- so email/push still have title/body/link to send even when a recipient
-- has disabled in-app notifications.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS recipient_email VARCHAR(320) NOT NULL DEFAULT '';
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS visible_in_app BOOLEAN NOT NULL DEFAULT true;

-- Serves both the unread-count poll and the dropdown list, which always
-- filter by (tenant_id, recipient_user_id) and sort newest first.
CREATE INDEX IF NOT EXISTS idx_notifications_recipient
    ON notifications (tenant_id, recipient_user_id, created_at DESC);

-- Partial index for the unread-count poll specifically — smaller and cheaper
-- to maintain than indexing every row, since read notifications never
-- appear in that query.
CREATE INDEX IF NOT EXISTS idx_notifications_unread
    ON notifications (tenant_id, recipient_user_id)
    WHERE read_at IS NULL;

-- One row per browser/device the user has granted push permission on.
-- endpoint is the full Push API subscription URL and is unique per
-- browser+device, so re-subscribing (e.g. after clearing site data)
-- naturally replaces the stale row via ON CONFLICT upsert.
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL,
    user_id     UUID NOT NULL,
    endpoint    TEXT NOT NULL UNIQUE,
    p256dh_key  TEXT NOT NULL,
    auth_key    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_push_subscriptions_user
    ON push_subscriptions (tenant_id, user_id);

-- Tenant-wide notification channel defaults. A missing row means "all
-- channels enabled" — tenants only get a row once someone changes a
-- default away from that.
CREATE TABLE IF NOT EXISTS tenant_notification_defaults (
    tenant_id      UUID PRIMARY KEY,
    email_enabled  BOOLEAN NOT NULL DEFAULT true,
    in_app_enabled BOOLEAN NOT NULL DEFAULT true,
    push_enabled   BOOLEAN NOT NULL DEFAULT true,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-user overrides on top of tenant_notification_defaults. A NULL column
-- means "inherit the tenant default" — only columns the user has actually
-- touched are non-NULL.
CREATE TABLE IF NOT EXISTS user_notification_preferences (
    tenant_id      UUID NOT NULL,
    user_id        UUID NOT NULL,
    email_enabled  BOOLEAN,
    in_app_enabled BOOLEAN,
    push_enabled   BOOLEAN,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, user_id)
);

-- Combined delivery queue and delivery log: one row per (notification,
-- channel). email/push start 'pending' and are claimed by the workers in
-- the workers package; in_app is written already-terminal ('sent' or
-- 'skipped') since the notifications row itself is the delivery. No
-- address/endpoint is stored here (deliberately) — email comes from
-- notifications.recipient_email, push subscriptions are looked up fresh
-- from push_subscriptions at send time, so a retry never acts on stale
-- addressing data.
CREATE TABLE IF NOT EXISTS notification_deliveries (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    notification_id    UUID NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
    tenant_id          UUID NOT NULL,
    recipient_user_id  UUID NOT NULL,
    channel            VARCHAR(16) NOT NULL,
    status             VARCHAR(16) NOT NULL DEFAULT 'pending',
    attempts           INT NOT NULL DEFAULT 0,
    max_attempts       INT NOT NULL DEFAULT 5,
    next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error         TEXT,
    provider_response  JSONB,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Serves both workers' claim queries: QueueConsumer scans for
-- status='pending', RetryWorker for status='retrying' (plus a stale-
-- processing sweep), both ordered/filtered by next_attempt_at.
CREATE INDEX IF NOT EXISTS idx_deliveries_claim
    ON notification_deliveries (status, next_attempt_at);

-- Serves the per-notification delivery-log endpoint, which additionally
-- filters by tenant_id so one tenant can never read another's log.
CREATE INDEX IF NOT EXISTS idx_deliveries_notification
    ON notification_deliveries (tenant_id, notification_id);

-- Append-only audit trail: who did what, per tenant. Nothing in this
-- service ever updates or deletes a row here — the store exposes only
-- Record and List. actor_user_id is NULL for entries raised by another
-- StoneSuite service (actor_type='service') or by this service's own
-- delivery workers (actor_type='worker'), neither of which has an end user
-- behind it.
CREATE TABLE IF NOT EXISTS notification_audit_logs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    actor_user_id UUID,
    actor_type    VARCHAR(16)  NOT NULL DEFAULT 'user',
    action        VARCHAR(64)  NOT NULL,
    resource      VARCHAR(64)  NOT NULL DEFAULT '',
    resource_id   VARCHAR(64)  NOT NULL DEFAULT '',
    metadata      JSONB,
    ip_address    VARCHAR(64)  NOT NULL DEFAULT '',
    user_agent    VARCHAR(300) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The default audit query: one tenant's trail, newest first.
CREATE INDEX IF NOT EXISTS idx_audit_tenant_created
    ON notification_audit_logs (tenant_id, created_at DESC);

-- Serves the two common investigative filters — "what happened of type X"
-- and "what did user Y do" — without scanning a whole tenant's history.
CREATE INDEX IF NOT EXISTS idx_audit_tenant_action
    ON notification_audit_logs (tenant_id, action, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_tenant_actor
    ON notification_audit_logs (tenant_id, actor_user_id, created_at DESC);

-- Replaces the single global INTERNAL_SERVICE_SECRET (middleware/auth.go).
-- Each row is one scoped, environment-bound credential: key_hash is
-- SHA-256(secret) — not a slow password KDF, deliberately, since the
-- secret itself carries 256 bits of entropy and is verified on every
-- request (see CLAUDE.md/design notes on this trade-off). scopes is a
-- JSON array of strings like ["messages:create"] or ["admin:*"], checked
-- by middleware.RequireInternalSecret. environment is read from this row
-- and never from the request body — the primary control that makes a dev
-- key physically unable to write a production row.
CREATE TABLE IF NOT EXISTS service_api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          VARCHAR(64)  NOT NULL,
    environment   VARCHAR(16)  NOT NULL,
    key_hash      BYTEA        NOT NULL UNIQUE,
    scopes        JSONB        NOT NULL DEFAULT '[]',
    active        BOOLEAN      NOT NULL DEFAULT true,
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The auth hot path looks up by hash directly (key_hash is already UNIQUE,
-- so Postgres already maintains a btree for it); this index instead serves
-- admin/ops listing and the rotation runbook ("all active keys for this
-- environment").
CREATE INDEX IF NOT EXISTS idx_service_api_keys_environment
    ON service_api_keys (environment, active);

-- notification_attachments holds at most one binary attachment per
-- notification — currently used only for the "document sent" owner
-- notification, which carries the same PDF the customer received. This is
-- deliberately its own table, not a column on notifications: notifications
-- is read in bulk by the feed/list/summary endpoints backing the in-app
-- bell UI, and this table is read only by the email delivery worker, one
-- row at a time, by notification_id.
CREATE TABLE IF NOT EXISTS notification_attachments (
    notification_id UUID PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
    file_name        TEXT NOT NULL,
    content_type     TEXT NOT NULL,
    content          BYTEA NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Email-only recipients (no StoneSuite users.id) — added so a notification
-- can address someone with no account yet (e.g. a document-send customer
-- email), routing that email through this service's queue/retry/audit
-- layer instead of a direct Resend/SMTP call from the caller. Existing
-- internal-recipient rows are unaffected: this only widens what the column
-- allows, nothing narrows it.
ALTER TABLE notifications ALTER COLUMN recipient_user_id DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'notifications_recipient_identified'
    ) THEN
        ALTER TABLE notifications ADD CONSTRAINT notifications_recipient_identified
            CHECK (recipient_user_id IS NOT NULL OR recipient_email <> '');
    END IF;
END $$;

ALTER TABLE notification_deliveries ALTER COLUMN recipient_user_id DROP NOT NULL;

-- Optional per-notification override of the generic email template
-- (<h2>title</h2><p>body</p>), used when a caller needs its own branded
-- HTML — e.g. the document-send customer email. Empty string means "use
-- the generic template"; see channels.SendNotificationEmail.
ALTER TABLE notification_attachments ADD COLUMN IF NOT EXISTS email_body_html TEXT NOT NULL DEFAULT '';

-- Serves the "list a tenant's deliveries by status since T" query
-- (deliveries.Store.ListByStatus) behind the admin/ops "what failed?"
-- view. Distinct from idx_deliveries_claim (status, next_attempt_at),
-- which is workers-only and not tenant-scoped.
CREATE INDEX IF NOT EXISTS idx_deliveries_tenant_status
    ON notification_deliveries (tenant_id, status, updated_at DESC);

-- Idempotency for callers that may retry after a timeout (automation engine):
-- the same (tenant, key) never creates a second notification or delivery.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(255);
CREATE UNIQUE INDEX IF NOT EXISTS uq_notifications_tenant_idempotency
    ON notifications (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
