# stonesuite-notify

Standalone notifications microservice for StoneSuite. It persists in-app notifications, serves them to the frontend bell, and reliably fans events out over email and Web Push via a Postgres-backed delivery queue with retries. It does not own user identity — it trusts sessions minted by StoneSuite-Backend (same JWT secret) rather than handling login itself, and it does not own role definitions: permissions are read from the caller's token (see [Authentication and RBAC](#authentication-and-rbac)).

Every notification flows through four stages: **preferences** (tenant defaults + per-user overrides decide which channels a recipient actually gets), **queueing** (a delivery row per channel, claimed by a background consumer), **retries** (exponential backoff, with recovery of work stranded by a crash), and **logging** (every channel's outcome recorded per notification, queryable for support/debugging). Every state-changing action is additionally written to a tenant-scoped **audit trail**.

## Requirements

- Go 1.25+
- PostgreSQL

## Configuration

The service reads all configuration from environment variables (see [config/config.go](config/config.go)).

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | Postgres connection string |
| `JWT_SECRET` | yes | Must match StoneSuite-Backend's `JWT_SECRET` |
| `INTERNAL_SERVICE_SECRET` | yes | Shared secret for service-to-service notification creation |
| `PORT` | no | Default `8090` |
| `CORS_ORIGIN` | no | Comma-separated allowlist of browser origins |
| `RESEND_API_KEY` | no | Email via Resend (tried first) |
| `RESEND_WEBHOOK_SECRET` | no | Signing secret (`whsec_…`) for Resend's webhooks, posted to `POST /api/webhooks/resend`. Unset ⇒ that route answers 503 and what happens to an email after Resend accepts it (delivered / delayed / bounced / …) is not tracked, and customer replies are not received. |
| `REPLY_EMAIL_DOMAIN` | no | The domain Resend *receives* mail on (e.g. `reply.yourdomain.com`) — **must equal StoneSuite-Backend's `REPLY_EMAIL_DOMAIN`**. Mail addressed to it (a customer replying to an email about a record) arrives as an `email.received` event on the same Resend webhook; notify fetches the mail from Resend and forwards it to the backend. Needs `BACKEND_URL` and `RESEND_API_KEY` (a full-access key; sending-only keys cannot read received mail). Either unset ⇒ `email.received` is acknowledged and dropped. |
| `BACKEND_URL` | with `REPLY_EMAIL_DOMAIN` | StoneSuite-Backend's public base URL, e.g. `https://stonesuite-backend.fly.dev`. Received replies are POSTed to `/api/internal/email/inbound`, HMAC-signed with `INTERNAL_SERVICE_SECRET` (the backend's `NOTIFY_API_KEY`). Must be an absolute http(s) URL. |
| `EMAIL_FROM` | with any email provider | Sender address for **both** Resend and SMTP — e.g. `StoneSuite <notifications@yourdomain.com>`. For Resend the domain must be verified in the Resend account, otherwise every send fails Resend validation with HTTP 422. If a provider is configured and this is unset, the email channel returns an error naming this variable rather than attempting a doomed send. |
| `EMAIL_REPLY_TO` | no | Sets the `Reply-To` header on every outbound email — e.g. `StoneSuite Support <support@yourdomain.com>`. Transactional mail with a real reply address is a positive signal to spam filters. Unset ⇒ no header. |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USERNAME`, `SMTP_PASSWORD` | no | Email via SMTP (fallback if no Resend key) |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT` | no | Web Push (VAPID) |

Email and push are optional channels — if unconfigured, the service still runs and simply skips that channel (logged); in-app notifications keep working. But a *partly* configured email provider (a `RESEND_API_KEY` or `SMTP_HOST` with no `EMAIL_FROM`) is a misconfiguration, not a skip — it fails every delivery with a clear error.

## Running

```bash
go run .
```

The database schema (`database/migrations/schema.sql`) is embedded and applied automatically on every boot — every statement is idempotent, so this is safe to run repeatedly.

## Testing

```bash
go test ./...
```

### Local end-to-end testing

`scripts/dev-up.sh` brings up a self-contained dev environment — Postgres
and [Mailpit](https://mailpit.axllent.org/) (an SMTP catcher, so the email
channel actually sends somewhere you can see it: http://localhost:8025) via
`docker-compose.dev.yml`, generates a gitignored `.env.local` with dev-only
secrets and a real VAPID keypair on first run, then runs the service:

```bash
scripts/dev-up.sh
```

With that running, `scripts/smoke-test.sh` drives every route — in-app
feed, email delivery, push subscribe + a deliberately unreachable endpoint
to exercise the retry/backoff path, preferences, admin routes, the audit
trail, and the auth/permission/validation boundaries — and reports
pass/fail:

```bash
scripts/smoke-test.sh
```

`scripts/mint-token` mints a JWT against `.env.local`'s `JWT_SECRET` for
manual `curl` poking (`--admin` for the elevated tenant-wide routes).
`scripts/dev-down.sh` stops Postgres/Mailpit (`-v` also drops the data
volume).

## API

All responses are wrapped in `{"success": bool, "message"?: string, "data"?: object}`.

### Notification bell (user)

| Method | Path | Permission | Description |
|---|---|---|---|
| GET | `/api/notifications/summary` | `notification:read` | Unread count for the bell badge |
| GET | `/api/notifications` | `notification:read` | Latest notifications for the dropdown (`unreadOnly`, `limit`) |
| GET | `/api/notifications/history` | `notification:read` | Paged "view all" screen (`unreadOnly`, `page`, `pageSize`) — returns a `pagination` object |
| POST | `/api/notifications/{id}/read` | `notification:update` | Mark one notification read |
| POST | `/api/notifications/read-all` | `notification:update` | Mark all notifications read |

`/api/notifications` and `/api/notifications/history` return the same rows (newest first, in-app-visible only, scoped to the caller); history adds paging and a total:

```json
{"success": true, "data": {
  "notifications": [ ... ],
  "pagination": {"page": 2, "pageSize": 20, "total": 57, "totalPages": 3, "hasMore": true}
}}
```

### Preferences and push (user)

| Method | Path | Permission | Description |
|---|---|---|---|
| GET | `/api/preferences` | `preference:read` | Caller's effective preferences (merged) plus their raw overrides |
| PUT | `/api/preferences` | `preference:update` | Partial update to the caller's own email/in-app/push overrides |
| GET | `/api/push/vapid-public-key` | none | Public VAPID key for `PushManager.subscribe()` |
| POST | `/api/push/subscribe` | `push:manage` | Save a browser's push subscription |
| DELETE | `/api/push/subscribe` | `push:manage` | Remove a push subscription |

### Tenant administration (user, elevated)

These act across a whole tenant and require an elevated permission that is never implicitly granted. Each takes its tenant from the caller's own token — never from the request body — so an administrator can only ever read or change their own tenant.

| Method | Path | Permission | Description |
|---|---|---|---|
| GET | `/api/admin/tenant-defaults` | `preference:admin` | Read the tenant's org-wide default preferences |
| PUT | `/api/admin/tenant-defaults` | `preference:admin` | Set the tenant's org-wide defaults (full replace) |
| GET | `/api/admin/notifications/{id}/deliveries` | `notification:admin` | Delivery log for one notification in the caller's tenant |
| GET | `/api/admin/deliveries?status=&since=&limit=` | `notification:admin` | The caller's tenant's deliveries in a given `status` (default `failed`), newest first — the "what didn't send?" view. `since` is RFC 3339 (default 7 days ago), `limit` caps at 200. |
| GET | `/api/admin/audit-logs` | `audit:read` | Query the tenant's audit trail |

### Service-to-service (internal secret)

No end-user session exists on these calls, so each names its tenant explicitly and is gated by the `X-Internal-Secret` header.

| Method | Path | Description |
|---|---|---|
| POST | `/api/notifications/internal` | Create notification(s) for one or more recipients (always writes the in-app row; email/push enqueued per resolved preference) |
| GET | `/api/notifications/{id}/deliveries?tenantId=` | Delivery log for one notification — `tenantId` is **required** |
| GET | `/api/deliveries?tenantId=&status=&since=&limit=` | A tenant's deliveries in a given `status` (default `failed`), newest first — `tenantId` **required**; `since` RFC 3339 (default 7 days ago); `limit` caps at 200. Lets an ops job pull terminal failures without already knowing the notification ids. |
| PUT | `/api/tenant-defaults` | Set a tenant's org-wide defaults (tenant named in the body) |
| GET | `/api/audit-logs?tenantId=` | Query a tenant's audit trail — `tenantId` is **required** |

### Health

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness check |
| GET | `/readyz` | Readiness check (pings the database) |

### Audit log query parameters

Both audit routes accept `action`, `actorUserId`, `resource`, `resourceId`, `from`/`to` (RFC 3339), `page`, and `pageSize` (max 200, default 50), and return `{entries, pagination}`.

## Authentication and RBAC

User routes accept a JWT via `Authorization: Bearer <token>` or the `auth_token` cookie, exactly as StoneSuite-Backend issues it. Every user route additionally declares a permission, checked by `middleware.RequirePermission`:

- If the token carries a **`permissions` claim** (a JSON array, or a space/comma-delimited string), it is authoritative: only what it lists is granted. `*` grants everything and `notification:*` grants every `notification:` action.
- If the token carries **no such claim** — which is what StoneSuite-Backend mints today — the caller implicitly holds the *self-service* permissions (`notification:read`, `notification:update`, `preference:read`, `preference:update`, `push:manage`). These only ever act on the caller's own rows: every query behind them is scoped to `(tenant_id, recipient_user_id)`.
- The *elevated* permissions (`notification:admin`, `preference:admin`, `audit:read`) are **never** implicitly granted. Until the backend mints them in the claim, the `/api/admin/*` routes return `403`.

This keeps today's tokens working for the notification bell while leaving tenant-wide access deny-by-default. Tenant isolation does not depend on the permission layer: every store query filters by `tenant_id` regardless.

Internal endpoints trust that the calling StoneSuite service already authorized its own caller, the same way the notification-create endpoint always has.

## Reliability layer

- **Preferences** (`preferences/`): a tenant-wide default (`tenant_notification_defaults`) plus per-user overrides (`user_notification_preferences`) resolve to an effective `{emailEnabled, inAppEnabled, pushEnabled}` per recipient. A missing row at either level means "inherit" (ultimately defaulting to all-enabled). Resolution failures fail open so an outage never blocks the in-app row.
- **Delivery queue + log** (`deliveries/`): one `notification_deliveries` row per (notification, channel). `in_app` is written already-terminal (`sent`/`skipped`) since the notification row itself is the delivery; `email`/`push` start `pending`. No address or push endpoint is stored on this table — email comes from `notifications.recipient_email`, push subscriptions are looked up fresh at send time, so a retry never acts on stale addressing.
- **Workers** (`workers/`): `QueueConsumer` polls every 2s for `pending` rows; `RetryWorker` polls every 15s for `retrying` rows due for another attempt (backoff: `min(30m, 1m*2^(n-1))`, 5 max attempts) and also recovers rows stuck `processing` for over 5 minutes (e.g. a crash mid-send). Both claim rows via `SELECT ... FOR UPDATE SKIP LOCKED` so multiple instances never double-send, and both call the same `attemptDelivery` to actually send.
- **Terminal-failure signal:** when a delivery exhausts its retries the worker logs a single stable line, `event=delivery_permanently_failed …`, alongside the `delivery.failed` audit row. Point a Fly/Axiom log alert at that key — it is the only active notification that a message will never be delivered. `GET /api/deliveries?status=failed` (or `/api/admin/deliveries`) lists the rows behind it.
- **In-app visibility vs. existence:** a notification row is always created for every recipient, regardless of their in-app preference — only `visible_in_app` (and therefore whether it shows up in `/api/notifications` or counts toward the unread badge) is gated, so email/push still have title/body/link/recipient_email to send even when in-app is off for that recipient.

## Audit trail

Every state-changing action is appended to `notification_audit_logs` (`audit/`), tenant-scoped and queryable via the two audit routes above. The store exposes only `Record` and `List` — nothing in this service updates or deletes an entry.

| Action | Actor | Recorded when |
|---|---|---|
| `notification.created` | service | One per notification created by the internal endpoint |
| `notification.read` | user | A notification is actually marked read (not on a 404) |
| `notification.read_all` | user | The bulk mark-all-read succeeds |
| `preference.updated` | user | A user changes their own overrides — metadata lists only the fields they set |
| `tenant_defaults.updated` | user / service | Org-wide defaults change, via either the admin or internal route |
| `push.subscribed` / `push.unsubscribed` | user | A device subscription is saved or removed |
| `delivery.sent` | worker | A queued email/push delivery succeeds |
| `delivery.failed` | worker | A delivery exhausts its attempt budget and goes terminal |
| `delivery_log.viewed` | user / service | Someone reads a delivery log (it reveals who was contacted on which channel) |

Retries between the first attempt and the terminal outcome are deliberately not audited — the delivery row already carries `attempts` and `last_error`, so auditing each one would bury the outcomes that matter.

Writes are detached from the request that triggered them (`audit.AsyncRecorder`), so auditing never adds latency to, or fails, the action being audited; a failure to record is logged instead. On shutdown the server flushes in-flight entries before exiting.
