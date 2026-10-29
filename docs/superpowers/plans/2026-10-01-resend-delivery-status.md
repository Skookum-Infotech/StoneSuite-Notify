# Resend Delivery Status (notify side) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `stonesuite-notify` capture Resend's email id at send time, receive Resend's signed delivery webhooks, keep the real provider status per email, alert the sender in-app when an email goes wrong, and expose a batched status read for the backend.

**Architecture:** A new dependency-light `emailevents` package owns the status state machine, Svix signature verification, and one transactional store (`Apply`: dedupe → advance state → raise alert). Two thin handlers in `controllers/` expose a public, signature-gated `POST /api/webhooks/resend` and an internal-secret `POST /api/deliveries/email-status`. The send path (`channels` → `workers` → `deliveries`) is changed only to carry Resend's email id into `notification_deliveries.provider_email_id`. The queue column `notification_deliveries.status` is never touched by webhooks.

**Tech Stack:** Go 1.25, `net/http` ServeMux, pgx v5, stdlib `testing` (no testify in this repo), stdlib `crypto/hmac` (no new dependencies).

**Spec:** `StoneSuite-Backend/docs/superpowers/specs/2026-10-01-resend-delivery-status-design.md` (sibling repo, `P:\WORKSPACE-SKOOKUM\StoneSuite-Backend`). §4 is this plan; §3, §7, §8 apply throughout.

**Companion plans (separate, written after this one):** backend (`services.EmailStatuses`, persisted ids, API fields) and WebUI (`EmailStatusBadge`). They depend on the wire contract in Task 6 below; do not change it without updating them.

## Global Constraints

- The webhook route `POST /api/webhooks/resend` is **public**: no `RequireAuth`, no `RequireInternalSecret`. Its only gate is the Svix signature (HMAC-SHA256 over `{svix-id}.{svix-timestamp}.{raw body}`, key = base64 of the secret after `whsec_`, header is a space-separated list of `v1,<base64>`, accept if **any** matches, `hmac.Equal`, timestamp within **5 minutes**).
- Webhook body is capped at **1 MiB** and read as **raw bytes** before any JSON decoding.
- Webhook responses: bad/missing signature or headers → **400**; `RESEND_WEBHOOK_SECRET` unset or not a valid `whsec_` secret → **503**; ignored event types, missing `email_id`, and unknown `email_id` → **200**; DB error → **500** (so Resend retries); success → **200**.
- `notification_deliveries.status` is the **send-queue** state the workers claim on — webhooks never write it. Provider lifecycle lives only in `provider_email_id`, `provider_status`, `provider_status_at`, `provider_detail`, and `email_events`.
- State only moves **forward**. Ranks: `sent` 10 · `delivery_delayed` 20 · `delivered` 30 · `complained` 40 · `bounced`/`failed`/`suppressed` 50. An incoming status changes the row only if its rank is **strictly greater** than the current one (equal rank keeps the first).
- Problem states (raise an alert): `delivery_delayed`, `complained`, `bounced`, `failed`, `suppressed`. `sent`/`delivered` never alert.
- Alert text is fixed and client-safe. **Provider detail (`bounce.message`, etc.) never goes into an alert, a log line, or any response a browser can read.** It is stored in `provider_detail` and returned only by the internal-secret status endpoint.
- The webhook's tenant comes from the matched `notification_deliveries` row, **never** from the payload.
- Tenant isolation on the status read: SQL always has `tenant_id = $1`.
- Schema changes are idempotent appended `ALTER TABLE … ADD COLUMN IF NOT EXISTS` / `CREATE … IF NOT EXISTS` in `database/migrations/schema.sql` — never edit an existing `CREATE TABLE` body. No down-migrations.
- `RESEND_WEBHOOK_SECRET` is **optional** config (like the other channel settings): unset ⇒ boot notice, route answers 503, nothing else changes.
- Logging in this repo is stdlib `log.Printf` (keep that), with stable `key=value` tokens for greppable events. Never log the request body, signature headers, or the secret.
- Tests use stdlib `testing` only, table-driven where the unit is pure. No new third-party dependencies.

## How to run Go here

There is no `go` on PATH on this machine. Define this helper at the top of **every** Bash call (shell state does not persist between calls) and prefix Go commands with it:

```bash
dgo() { MSYS_NO_PATHCONV=1 docker run --rm -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -v "P:\WORKSPACE-SKOOKUM\stonesuite-notify:/app" -v ss-go-build:/root/.cache/go-build \
  -v ss-go-mod:/go/pkg/mod -w /app golang:1.25.12-alpine "$@"; }
# e.g.  dgo go test ./config/ -run TestResendWebhookConfigured -count=1
```

Module downloads need network (no `vendor/`); the `ss-go-mod` volume caches them after the first run. The working tree is CRLF (`core.autocrlf=true`): if a multi-line `Edit` fails to match, anchor it on a single line; check formatting with `tr -d '\r' < file | dgo gofmt -l` only after stripping CRs (otherwise `gofmt -l` lists everything).

**Checkpoints:** each task ends with a checkpoint listing the files to stage and a suggested Conventional-Commit message. **Do not commit unless the user has asked you to** — leave the tree dirty and report it.

## File Structure

| File | Responsibility |
|---|---|
| `config/config.go` (+`_test.go`) | `ResendWebhookSecret` setting + `ResendWebhookConfigured()` |
| `database/migrations/schema.sql` | new columns, `email_events` table, indexes |
| `emailevents/status.go` (+`_test.go`) | status constants, rank/`Advance`, `StatusForEvent`, `IsProblem`, `DeriveState`, alert text — **pure** |
| `emailevents/svix.go` (+`_test.go`) | `VerifySignature`, `SignatureHeader`, `ErrInvalidSignature`, `ErrBadSecret` — **pure** |
| `emailevents/store.go` | `Store` interface, `Event`, `ApplyResult`, `StatusRow`, `PGStore` (`Apply` in one tx, `StatusForNotifications`) |
| `emailevents/store_dbtest_test.go` | `dbtest`-tagged tests against real Postgres |
| `controllers/emailevents.go` (+`_test.go`) | `EmailEventsHandler.Resend` (webhook) and `.EmailStatus` (internal read) |
| `channels/email.go` (+test) | `sendViaResend` / `SendNotificationEmail` return Resend's email id |
| `workers/dispatch.go` (+tests) | pass the id to `MarkSent` |
| `deliveries/store.go`, `types.go` (+tests) | `MarkSent(…, providerEmailID)`; `providerStatus`/`providerStatusAt` in JSON |
| `notifications/types.go`, `store.go`; `controllers/notifications.go` (+test) | optional `statusLink` on create |
| `main.go` | wiring, routes, boot notice |
| `scripts/signwebhook/main.go` | prints Svix headers for a body (local testing) |
| `CLAUDE.md`, `README.md` | document the feature and the env var |

---

### Task 1: Config setting and schema

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Modify: `database/migrations/schema.sql` (append at end)

**Interfaces:**
- Produces: `config.Config.ResendWebhookSecret string`; `func (c Config) ResendWebhookConfigured() bool`; the SQL objects listed in Step 4 that later tasks read and write.

- [ ] **Step 1: Write the failing test** — append to `config/config_test.go`:

```go
func TestResendWebhookConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"unset", Config{}, false},
		{"set", Config{ResendWebhookSecret: "whsec_abc"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResendWebhookConfigured(); got != tc.want {
				t.Fatalf("ResendWebhookConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `dgo go test ./config/ -run TestResendWebhookConfigured -count=1`
Expected: FAIL to compile — `unknown field ResendWebhookSecret` / `undefined: ResendWebhookConfigured`.

- [ ] **Step 3: Implement** — in `config/config.go`, add the field after `EmailReplyTo` in the `Config` struct:

```go
	// ResendWebhookSecret is the signing secret ("whsec_…") Resend shows when a
	// webhook endpoint is created. It authenticates POST /api/webhooks/resend,
	// which has no other gate. Optional: unset ⇒ the route answers 503.
	ResendWebhookSecret string
```

In `Load()`, add to the struct literal next to `EmailReplyTo`:

```go
		ResendWebhookSecret: os.Getenv("RESEND_WEBHOOK_SECRET"),
```

Add the method after `PushConfigured`:

```go
// ResendWebhookConfigured reports whether provider delivery webhooks can be
// verified (a signing secret is set).
func (c Config) ResendWebhookConfigured() bool {
	return c.ResendWebhookSecret != ""
}
```

- [ ] **Step 4: Append the schema** to the end of `database/migrations/schema.sql`:

```sql
-- ── Provider (Resend) delivery lifecycle ──────────────────────────────
-- notification_deliveries.status is the SEND-QUEUE state the workers claim on
-- (pending → sent/retrying/failed). What the email provider reports AFTER
-- accepting a message (delivered / delayed / bounced / complained / …) is a
-- different thing and lives in its own columns so the two can never be
-- confused or race. provider_email_id is Resend's id for the sent email — the
-- key the webhook uses to find its row. NULL for SMTP sends (no webhooks).
ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS provider_email_id  TEXT;
ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS provider_status    VARCHAR(24);
ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS provider_status_at TIMESTAMPTZ;
ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS provider_detail    TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_deliveries_provider_email
    ON notification_deliveries (provider_email_id) WHERE provider_email_id IS NOT NULL;

-- Where the sender should land when an in-app delivery-problem alert is
-- clicked (the staff-side page that shows the email's status badge). Distinct
-- from link, which for a customer document email is the CUSTOMER's URL.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS status_link VARCHAR(300) NOT NULL DEFAULT '';

-- The RBAC resource that governs the page status_link opens ("invoice", "user",
-- "portal_access", …). The in-app bell only shows a notification whose resource
-- the reader may Read, so a delivery-problem alert must carry THIS resource, not
-- the original email's: e.g. a portal invite's resource "portal_user" is not an
-- RBAC resource at all, and an alert copied from it would be invisible to every
-- sender who is not a super admin. Empty ⇒ fall back to the original's resource.
ALTER TABLE notifications ADD COLUMN IF NOT EXISTS status_resource VARCHAR(64) NOT NULL DEFAULT '';

-- Append-only log of provider webhook events that matched a delivery.
-- svix_id is Resend's per-message id: UNIQUE makes a redelivered webhook a
-- no-op. The raw payload is deliberately NOT stored (it carries the recipient
-- address and subject); only the event type, time and a short detail string.
CREATE TABLE IF NOT EXISTS email_events (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    svix_id           TEXT        NOT NULL UNIQUE,
    delivery_id       UUID        NOT NULL REFERENCES notification_deliveries(id) ON DELETE CASCADE,
    tenant_id         UUID        NOT NULL,
    provider_email_id TEXT        NOT NULL,
    event_type        VARCHAR(32) NOT NULL,
    occurred_at       TIMESTAMPTZ NOT NULL,
    detail            TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_email_events_delivery
    ON email_events (delivery_id, occurred_at);
```

- [ ] **Step 5: Run the test to verify it passes, and the package still builds**

Run: `dgo sh -c 'go test ./config/ -count=1 && go build ./...'`
Expected: PASS, build OK. (The SQL itself is exercised for real — twice, to prove idempotency — by Task 4's `dbtest`.)

- [ ] **Step 6: Checkpoint** — stage `config/config.go config/config_test.go database/migrations/schema.sql`; suggested message `feat(notify): add RESEND_WEBHOOK_SECRET setting and provider-status schema`.

---

### Task 2: Status state machine (pure logic)

**Files:**
- Create: `emailevents/status.go`
- Create: `emailevents/status_test.go`

**Interfaces:**
- Consumes: `deliveries.Status*` string constants (`stonesuite-notify/deliveries`).
- Produces (later tasks rely on these exact names):
  - consts `StatusSent, StatusDelayed, StatusDelivered, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed` (provider statuses; `StatusDelayed == "delivery_delayed"`)
  - consts `StateQueued, StateRetrying, StateDelayed, StateSkipped, StateUnknown`; const `AlertEventType = "email.delivery_problem"`
  - `func StatusForEvent(eventType string) (status string, ok bool)`
  - `func Advance(current, incoming string) (next string, changed bool)`
  - `func IsProblem(status string) bool`
  - `func DeriveState(queueStatus, providerStatus string) string`
  - `func AlertTitle(status, recipient string) string`, `func AlertBody(status string) string` (both `""` for a non-problem status)

- [ ] **Step 1: Write the failing test** — `emailevents/status_test.go`:

```go
package emailevents

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStatusForEvent(t *testing.T) {
	tests := []struct {
		event  string
		want   string
		wantOK bool
	}{
		{"email.sent", StatusSent, true},
		{"email.delivery_delayed", StatusDelayed, true},
		{"email.delivered", StatusDelivered, true},
		{"email.complained", StatusComplained, true},
		{"email.bounced", StatusBounced, true},
		{"email.failed", StatusFailed, true},
		{"email.suppressed", StatusSuppressed, true},
		{"email.opened", "", false},
		{"email.clicked", "", false},
		{"email.scheduled", "", false},
		{"email.received", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.event, func(t *testing.T) {
			got, ok := StatusForEvent(tc.event)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("StatusForEvent(%q) = (%q, %v), want (%q, %v)", tc.event, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestAdvance(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		incoming    string
		want        string
		wantChanged bool
	}{
		{"first event", "", StatusSent, StatusSent, true},
		{"sent to delivered", StatusSent, StatusDelivered, StatusDelivered, true},
		{"late sent after delivered", StatusDelivered, StatusSent, StatusDelivered, false},
		{"late delayed after delivered", StatusDelivered, StatusDelayed, StatusDelivered, false},
		{"delayed to delivered", StatusDelayed, StatusDelivered, StatusDelivered, true},
		{"delivered to complained", StatusDelivered, StatusComplained, StatusComplained, true},
		{"late delivered after complained", StatusComplained, StatusDelivered, StatusComplained, false},
		{"delivered to bounced", StatusDelivered, StatusBounced, StatusBounced, true},
		{"equal rank keeps the first", StatusBounced, StatusFailed, StatusBounced, false},
		{"equal rank keeps the first (suppressed)", StatusFailed, StatusSuppressed, StatusFailed, false},
		{"same status again", StatusBounced, StatusBounced, StatusBounced, false},
		{"unknown incoming ignored", StatusSent, "bogus", StatusSent, false},
		{"unknown incoming on empty", "", "bogus", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := Advance(tc.current, tc.incoming)
			if got != tc.want || changed != tc.wantChanged {
				t.Fatalf("Advance(%q, %q) = (%q, %v), want (%q, %v)", tc.current, tc.incoming, got, changed, tc.want, tc.wantChanged)
			}
		})
	}
}

func TestIsProblem(t *testing.T) {
	problems := []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed}
	fine := []string{StatusSent, StatusDelivered, "", "bogus"}
	for _, s := range problems {
		if !IsProblem(s) {
			t.Errorf("IsProblem(%q) = false, want true", s)
		}
	}
	for _, s := range fine {
		if IsProblem(s) {
			t.Errorf("IsProblem(%q) = true, want false", s)
		}
	}
}

func TestDeriveState(t *testing.T) {
	tests := []struct {
		name     string
		queue    string
		provider string
		want     string
	}{
		{"provider status wins", "sent", StatusDelivered, "delivered"},
		{"provider delayed is spelled delayed", "sent", StatusDelayed, StateDelayed},
		{"provider bounced", "sent", StatusBounced, "bounced"},
		{"no provider status, pending", "pending", "", StateQueued},
		{"no provider status, processing", "processing", "", StateQueued},
		{"no provider status, sent", "sent", "", "sent"},
		{"no provider status, retrying", "retrying", "", StateRetrying},
		{"no provider status, failed", "failed", "", "failed"},
		{"no provider status, skipped", "skipped", "", StateSkipped},
		{"unrecognised provider status falls back to queue", "sent", "bogus", "sent"},
		{"nothing known", "", "", StateUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveState(tc.queue, tc.provider); got != tc.want {
				t.Fatalf("DeriveState(%q, %q) = %q, want %q", tc.queue, tc.provider, got, tc.want)
			}
		})
	}
}

func TestAlertText(t *testing.T) {
	for _, status := range []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed} {
		t.Run(status, func(t *testing.T) {
			title := AlertTitle(status, "alice@example.com")
			if !strings.Contains(title, "alice@example.com") {
				t.Fatalf("title %q must name the recipient", title)
			}
			if AlertBody(status) == "" {
				t.Fatalf("body for %q is empty", status)
			}
		})
	}

	for _, status := range []string{StatusSent, StatusDelivered, "", "bogus"} {
		if AlertTitle(status, "a@b.com") != "" || AlertBody(status) != "" {
			t.Errorf("non-problem status %q must produce no alert text", status)
		}
	}

	t.Run("empty recipient uses a fallback", func(t *testing.T) {
		if got := AlertTitle(StatusBounced, "  "); !strings.Contains(got, fallbackRecipient) {
			t.Fatalf("title %q must use the fallback recipient", got)
		}
	})

	t.Run("a long recipient keeps the title inside the column width", func(t *testing.T) {
		long := strings.Repeat("x", 400) + "@example.com"
		title := AlertTitle(StatusBounced, long)
		if n := utf8.RuneCountInString(title); n > 200 { // notifications.title is VARCHAR(200)
			t.Fatalf("title is %d characters, want <= 200", n)
		}
	})

	t.Run("body fits its column", func(t *testing.T) {
		for _, status := range []string{StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed} {
			if n := utf8.RuneCountInString(AlertBody(status)); n > 500 { // notifications.body is VARCHAR(500)
				t.Fatalf("body for %q is %d characters, want <= 500", status, n)
			}
		}
	})
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `dgo go test ./emailevents/ -count=1`
Expected: FAIL — `no non-test Go files` / undefined identifiers.

- [ ] **Step 3: Implement** — `emailevents/status.go`:

```go
// Package emailevents records what the email provider (Resend) reports about
// an email after stonesuite-notify has handed it over — delivered, delayed,
// bounced, complained, … — and raises an in-app alert for the sender when it
// goes wrong. The send-queue state lives in package deliveries and is never
// touched here; this package owns the provider lifecycle only.
package emailevents

import (
	"fmt"
	"strings"

	"stonesuite-notify/deliveries"
)

// Provider statuses, stored in notification_deliveries.provider_status.
const (
	StatusSent       = "sent"
	StatusDelayed    = "delivery_delayed"
	StatusDelivered  = "delivered"
	StatusComplained = "complained"
	StatusBounced    = "bounced"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
)

// Client-facing states with no provider status of their own. The rest of the
// client vocabulary reuses the Status* strings; StateDelayed is the client
// spelling of StatusDelayed.
const (
	StateQueued   = "queued"
	StateRetrying = "retrying"
	StateDelayed  = "delayed"
	StateSkipped  = "skipped"
	StateUnknown  = "unknown"
)

// AlertEventType is the notifications.event_type of the in-app alert raised for
// the sender of a problem email.
const AlertEventType = "email.delivery_problem"

const (
	maxAlertRecipientRunes = 120
	fallbackRecipient      = "a recipient"
)

// eventStatuses maps a Resend webhook event type to the provider status it sets.
// Event types not listed (opened, clicked, scheduled, received, …) are ignored.
var eventStatuses = map[string]string{
	"email.sent":             StatusSent,
	"email.delivery_delayed": StatusDelayed,
	"email.delivered":        StatusDelivered,
	"email.complained":       StatusComplained,
	"email.bounced":          StatusBounced,
	"email.failed":           StatusFailed,
	"email.suppressed":       StatusSuppressed,
}

// statusRank orders provider statuses. State only moves to a strictly higher
// rank, so a late or replayed event can never move an email backwards. An empty
// or unrecognised current status ranks 0 (the map's zero value).
var statusRank = map[string]int{
	StatusSent:       10,
	StatusDelayed:    20,
	StatusDelivered:  30,
	StatusComplained: 40,
	StatusBounced:    50,
	StatusFailed:     50,
	StatusSuppressed: 50,
}

// StatusForEvent returns the provider status a Resend event type sets.
func StatusForEvent(eventType string) (string, bool) {
	status, ok := eventStatuses[eventType]
	return status, ok
}

// Advance returns the status after applying incoming to current, and whether
// it changed. Only a strictly higher rank wins; an equal rank keeps the first
// status seen, so a bounce followed by a failure does not alert twice.
func Advance(current, incoming string) (string, bool) {
	incomingRank, ok := statusRank[incoming]
	if !ok {
		return current, false
	}
	if incomingRank > statusRank[current] {
		return incoming, true
	}
	return current, false
}

// IsProblem reports whether status is one the sender should be alerted about.
func IsProblem(status string) bool {
	switch status {
	case StatusDelayed, StatusComplained, StatusBounced, StatusFailed, StatusSuppressed:
		return true
	}
	return false
}

// DeriveState returns the single client-facing state for one email: the
// provider status when there is one, otherwise a reading of the send-queue
// status.
func DeriveState(queueStatus, providerStatus string) string {
	if _, known := statusRank[providerStatus]; known {
		if providerStatus == StatusDelayed {
			return StateDelayed
		}
		return providerStatus
	}
	switch queueStatus {
	case deliveries.StatusPending, deliveries.StatusProcessing:
		return StateQueued
	case deliveries.StatusSent:
		return StatusSent
	case deliveries.StatusRetrying:
		return StateRetrying
	case deliveries.StatusFailed:
		return StatusFailed
	case deliveries.StatusSkipped:
		return StateSkipped
	}
	return StateUnknown
}

var alertTitles = map[string]string{
	StatusBounced:    "Email to %s bounced",
	StatusComplained: "Email to %s was marked as spam",
	StatusFailed:     "Email to %s could not be sent",
	StatusSuppressed: "Email to %s was not sent",
	StatusDelayed:    "Email to %s is delayed",
}

var alertBodies = map[string]string{
	StatusBounced:    "The recipient's mail server rejected this email. Check the address and try again.",
	StatusComplained: "The recipient reported this email as spam. Avoid re-sending it to this address.",
	StatusFailed:     "The email could not be sent. Try again, or contact support if the problem continues.",
	StatusSuppressed: "This address is on a suppression list because earlier emails to it bounced or were reported as spam.",
	StatusDelayed:    "The recipient's mail server is slow to accept this email. We will keep trying; no action is needed yet.",
}

// AlertTitle is the client-safe alert headline for a problem status, or "" for
// any other status. The recipient is shortened so the title always fits its
// column. Provider detail is deliberately never part of it.
func AlertTitle(status, recipient string) string {
	format, ok := alertTitles[status]
	if !ok {
		return ""
	}
	return fmt.Sprintf(format, shortRecipient(recipient))
}

// AlertBody is the client-safe alert explanation for a problem status, or ""
// for any other status.
func AlertBody(status string) string {
	return alertBodies[status]
}

func shortRecipient(recipient string) string {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return fallbackRecipient
	}
	runes := []rune(recipient)
	if len(runes) > maxAlertRecipientRunes {
		return string(runes[:maxAlertRecipientRunes]) + "…"
	}
	return recipient
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `dgo sh -c 'go vet ./emailevents/ && go test ./emailevents/ -count=1 -v'`
Expected: PASS for `TestStatusForEvent`, `TestAdvance`, `TestIsProblem`, `TestDeriveState`, `TestAlertText`.

- [ ] **Step 5: Checkpoint** — stage `emailevents/status.go emailevents/status_test.go`; suggested message `feat(notify): add provider email status state machine`.

---

### Task 3: Svix signature verification

**Files:**
- Create: `emailevents/svix.go`
- Create: `emailevents/svix_test.go`

**Interfaces:**
- Produces:
  - `var ErrInvalidSignature, ErrBadSecret error`
  - `func VerifySignature(secret, msgID, timestamp, signatures string, body []byte, now time.Time) error` — `nil` when valid; `ErrBadSecret` when the configured secret is not a valid `whsec_<base64>`; `ErrInvalidSignature` for every request-side problem (missing id, bad/stale/future timestamp, no matching `v1` signature).
  - `func SignatureHeader(secret, msgID, timestamp string, body []byte) (string, error)` — returns `"v1,<base64>"`; used by tests, the dbtest, and `scripts/signwebhook`.

- [ ] **Step 1: Write the failing test** — `emailevents/svix_test.go`:

```go
package emailevents

import (
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"
)

var testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("test-signing-key-0123456789abcdef"))

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"email.bounced","data":{"email_id":"em_1"}}`)
	good, err := SignatureHeader(testSecret, "msg_1", ts, body)
	if err != nil {
		t.Fatalf("SignatureHeader: %v", err)
	}
	otherSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("a-completely-different-key-000000"))
	wrongSig, err := SignatureHeader(otherSecret, "msg_1", ts, body)
	if err != nil {
		t.Fatalf("SignatureHeader(other): %v", err)
	}

	tests := []struct {
		name      string
		secret    string
		msgID     string
		timestamp string
		sigs      string
		body      []byte
		now       time.Time
		wantErr   error
	}{
		{"valid", testSecret, "msg_1", ts, good, body, now, nil},
		{"valid with a second, wrong signature first", testSecret, "msg_1", ts, wrongSig + " " + good, body, now, nil},
		{"valid with the wrong signature last", testSecret, "msg_1", ts, good + " " + wrongSig, body, now, nil},
		{"tampered body", testSecret, "msg_1", ts, good, []byte(`{"type":"email.delivered"}`), now, ErrInvalidSignature},
		{"different message id", testSecret, "msg_2", ts, good, body, now, ErrInvalidSignature},
		{"signed with another secret", testSecret, "msg_1", ts, wrongSig, body, now, ErrInvalidSignature},
		{"timestamp too old", testSecret, "msg_1", ts, good, body, now.Add(6 * time.Minute), ErrInvalidSignature},
		{"timestamp too far in the future", testSecret, "msg_1", ts, good, body, now.Add(-6 * time.Minute), ErrInvalidSignature},
		{"timestamp just inside the window", testSecret, "msg_1", ts, good, body, now.Add(4 * time.Minute), nil},
		{"unversioned signature ignored", testSecret, "msg_1", ts, "v2," + good[3:], body, now, ErrInvalidSignature},
		{"garbled base64 signature ignored", testSecret, "msg_1", ts, "v1,@@@not-base64@@@", body, now, ErrInvalidSignature},
		{"empty signature header", testSecret, "msg_1", ts, "", body, now, ErrInvalidSignature},
		{"empty message id", testSecret, "", ts, good, body, now, ErrInvalidSignature},
		{"non-numeric timestamp", testSecret, "msg_1", "yesterday", good, body, now, ErrInvalidSignature},
		{"empty timestamp", testSecret, "msg_1", "", good, body, now, ErrInvalidSignature},
		{"secret is not base64", "whsec_!!!", "msg_1", ts, good, body, now, ErrBadSecret},
		{"empty secret", "", "msg_1", ts, good, body, now, ErrBadSecret},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySignature(tc.secret, tc.msgID, tc.timestamp, tc.sigs, tc.body, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("VerifySignature() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestSignatureHeader_RejectsBadSecret(t *testing.T) {
	if _, err := SignatureHeader("whsec_!!!", "msg_1", "1", nil); !errors.Is(err, ErrBadSecret) {
		t.Fatalf("SignatureHeader(bad secret) error = %v, want ErrBadSecret", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `dgo go test ./emailevents/ -run 'TestVerifySignature|TestSignatureHeader' -count=1`
Expected: FAIL to compile — `undefined: VerifySignature`, `SignatureHeader`, `ErrBadSecret`, `ErrInvalidSignature`.

- [ ] **Step 3: Implement** — `emailevents/svix.go`:

```go
package emailevents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Resend signs webhooks with Svix. The signed content is
// "{svix-id}.{svix-timestamp}.{raw body}"; the key is the base64 payload of the
// "whsec_…" secret; the svix-signature header is a space-separated list of
// "v1,<base64 HMAC-SHA256>" entries (more than one while a secret is rotating).
const (
	svixSecretPrefix       = "whsec_"
	svixSignatureVersion   = "v1"
	svixTimestampTolerance = 5 * time.Minute
)

var (
	// ErrInvalidSignature means the request failed authentication: a missing or
	// malformed header, a timestamp outside the replay window, or no matching signature.
	ErrInvalidSignature = errors.New("emailevents: invalid webhook signature")
	// ErrBadSecret means the configured signing secret is not a valid
	// "whsec_<base64>" value — a server misconfiguration, not a bad request.
	ErrBadSecret = errors.New("emailevents: webhook secret is not a valid whsec_ secret")
)

func svixKey(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, svixSecretPrefix))
	if err != nil || len(key) == 0 {
		return nil, ErrBadSecret
	}
	return key, nil
}

func svixMAC(key []byte, msgID, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// SignatureHeader returns the svix-signature header value ("v1,<base64>") for a
// body. Real webhooks are signed by Resend; this exists for tests and for
// scripts/signwebhook, which lets a developer exercise the route locally.
func SignatureHeader(secret, msgID, timestamp string, body []byte) (string, error) {
	key, err := svixKey(secret)
	if err != nil {
		return "", err
	}
	return svixSignatureVersion + "," + base64.StdEncoding.EncodeToString(svixMAC(key, msgID, timestamp, body)), nil
}

// VerifySignature authenticates a Svix-signed webhook. body must be the raw
// request bytes — re-marshalled JSON would not match. It accepts the request if
// ANY entry of signatures matches (constant-time compare) and the timestamp is
// within five minutes of now in either direction, which bounds replay.
func VerifySignature(secret, msgID, timestamp, signatures string, body []byte, now time.Time) error {
	key, err := svixKey(secret)
	if err != nil {
		return err
	}
	if msgID == "" {
		return ErrInvalidSignature
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > svixTimestampTolerance {
		return ErrInvalidSignature
	}

	want := svixMAC(key, msgID, timestamp, body)
	for _, candidate := range strings.Fields(signatures) {
		version, encoded, ok := strings.Cut(candidate, ",")
		if !ok || version != svixSignatureVersion {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrInvalidSignature
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `dgo sh -c 'go vet ./emailevents/ && go test ./emailevents/ -count=1'`
Expected: PASS (status and svix tests).

- [ ] **Step 5: Checkpoint** — stage `emailevents/svix.go emailevents/svix_test.go`; suggested message `feat(notify): verify Svix-signed Resend webhooks`.

---

### Task 4: Transactional store (`Apply`, `StatusForNotifications`) with DB-backed tests

**Files:**
- Create: `emailevents/store.go`
- Create: `emailevents/store_dbtest_test.go`

**Interfaces:**
- Consumes: Task 1 schema; Task 2 `StatusForEvent`, `Advance`, `IsProblem`, `AlertTitle`, `AlertBody`, `AlertEventType`; Task 3 `SignatureHeader` is **not** needed here.
- Produces:
  - `type Event struct{ SvixID, ProviderEmailID, Type string; OccurredAt time.Time; Detail string }`
  - `type ApplyResult struct{ Matched, Duplicate, Changed, Alerted bool; Status string }`
  - `type StatusRow struct{ NotificationID, Recipient, QueueStatus, ProviderStatus, Detail string; UpdatedAt time.Time }`
  - `type Store interface{ Apply(ctx context.Context, ev Event) (ApplyResult, error); StatusForNotifications(ctx context.Context, tenantID string, notificationIDs []string) ([]StatusRow, error) }`
  - `func NewPGStore(pool *pgxpool.Pool) *PGStore`

`Apply` semantics (one transaction, any error ⇒ rollback + error so Resend retries):
1. Lock the delivery by `provider_email_id` (`FOR UPDATE`) — none ⇒ `ApplyResult{}` (`Matched=false`).
2. Insert into `email_events … ON CONFLICT (svix_id) DO NOTHING` — 0 rows ⇒ `{Matched:true, Duplicate:true}`.
3. `Advance(current, incoming)`; if changed, update the delivery's provider columns.
4. If changed **and** `IsProblem(next)` **and** the original notification has an `actor_user_id`, insert the alert notification (`resource` = the original's `status_resource` when set, else its `resource`; the original's `resource_id`; `link = status_link`; `visible_in_app = true`) plus its terminal `in_app` delivery row.

- [ ] **Step 1: Write the failing DB tests** — `emailevents/store_dbtest_test.go`. The tests need a real Postgres, so they are behind the `dbtest` build tag and skip when `TEST_DATABASE_URL` is unset (the same convention StoneSuite-Backend uses):

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Start the dev Postgres (port 5544) and run the DB tests:

```bash
cd /p/WORKSPACE-SKOOKUM/stonesuite-notify && docker compose -f docker-compose.dev.yml up -d postgres
dgo() { MSYS_NO_PATHCONV=1 docker run --rm -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -e TEST_DATABASE_URL='postgres://notify:notify@host.docker.internal:5544/stonesuite_notify?sslmode=disable' \
  -v "P:\WORKSPACE-SKOOKUM\stonesuite-notify:/app" -v ss-go-build:/root/.cache/go-build \
  -v ss-go-mod:/go/pkg/mod -w /app golang:1.25.12-alpine "$@"; }
dgo go test -tags dbtest ./emailevents/ -count=1
```

Expected: FAIL to compile — `undefined: emailevents.NewPGStore`, `emailevents.Event`.

- [ ] **Step 3: Implement** — `emailevents/store.go`:

```go
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
```

- [ ] **Step 4: Run the DB tests to verify they pass** (same command as Step 2, with the `dgo` that has `TEST_DATABASE_URL`)

Run: `dgo go test -tags dbtest ./emailevents/ -count=1 -v`
Expected: PASS for all of `TestApply_*` and `TestStatusForNotifications` (none skipped). `newPool` also proves the Task 1 schema applies twice without error.

Also confirm the default (no tag) build/tests still pass and nothing needs a DB:

Run: `dgo sh -c 'go vet ./... && go test ./emailevents/ -count=1'`
Expected: PASS.

- [ ] **Step 5: Checkpoint** — stage `emailevents/store.go emailevents/store_dbtest_test.go`; suggested message `feat(notify): apply provider email events transactionally and alert the sender`.

---

### Task 5: Capture Resend's email id on send

**Files:**
- Modify: `channels/email.go` (`SendNotificationEmail`, `sendViaResend`)
- Modify: `channels/email_test.go`
- Modify: `workers/dispatch.go` (`Deps.SendEmail`, `attemptEmail`, push `MarkSent` call)
- Modify: `workers/dispatch_test.go`, `workers/consumer_test.go`, `workers/retry_test.go`
- Modify: `deliveries/store.go` (`Store.MarkSent`, `PGStore.MarkSent`)
- Modify: `controllers/notifications_test.go` (`fakeDeliveriesStore.MarkSent`)

**Interfaces:**
- Produces:
  - `channels.SendNotificationEmail(cfg, to, title, body, link, emailBodyHTML, attachment) (providerID string, err error)` — `providerID` is `""` for SMTP or an unparseable success body.
  - `workers.Deps.SendEmail func(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *channels.EmailAttachment) (string, error)`
  - `deliveries.Store.MarkSent(ctx context.Context, id string, providerResponse []byte, providerEmailID string) error`

- [ ] **Step 1: Migrate the signatures — no behaviour change yet.** The id is not captured in this step; the goal is a tree that compiles and passes with the new shapes, so the real change in Step 4 is small and the new tests in Step 2 are the only red.

*Production signatures.* `channels/email.go` — `SendNotificationEmail` and `sendViaResend` now return `(string, error)`; every existing `return err` / `return fmt.Errorf(…)` becomes `return "", …`, and a successful Resend call returns `""` for now:

```go
func SendNotificationEmail(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *EmailAttachment) (string, error) {
	if to == "" {
		return "", fmt.Errorf("email channel: recipient address is empty")
	}
	if cfg.EmailConfigured() && cfg.EmailFrom == "" {
		return "", fmt.Errorf("email channel: EMAIL_FROM is not set (required as the sender address for Resend and SMTP alike)")
	}
	// … unchanged htmlBody / textBody derivation …
	switch {
	case cfg.ResendAPIKey != "":
		return sendViaResend(cfg, to, title, htmlBody, textBody, attachment)
	case cfg.SMTPHost != "":
		return "", sendViaSMTP(cfg, to, title, htmlBody, textBody, attachment)
	default:
		return "", fmt.Errorf("email channel: no provider configured (set RESEND_API_KEY or SMTP_HOST, plus EMAIL_FROM)")
	}
}
```

Append to its doc comment: `// It returns the provider's id for the sent email (Resend only; "" for SMTP), which the worker stores so the provider's delivery webhooks can be matched to this delivery.` In `sendViaResend` change the signature to `(string, error)`, prefix every error return with `"", `, and end with `return "", nil` (the id capture is Step 4).

`deliveries/store.go` — interface and implementation (this is final code):

```go
	// MarkSent finalizes a delivery as sent. providerEmailID is the email
	// provider's id for the message (Resend), recorded so its delivery
	// webhooks can be matched to this row; "" when there is none (push, SMTP).
	MarkSent(ctx context.Context, id string, providerResponse []byte, providerEmailID string) error
```

```go
func (s *PGStore) MarkSent(ctx context.Context, id string, providerResponse []byte, providerEmailID string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE notification_deliveries
		SET status = 'sent', provider_response = $2, provider_email_id = NULLIF($3, ''), updated_at = NOW()
		WHERE id = $1`, id, providerResponse, providerEmailID); err != nil {
		return fmt.Errorf("mark delivery %s sent: %w", id, err)
	}
	return nil
}
```

`workers/dispatch.go` — change the `Deps.SendEmail` field type to `func(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *channels.EmailAttachment) (string, error)`. In `attemptEmail` ignore the id **for now**:

```go
	if _, err := deps.SendEmail(deps.Config, n.RecipientEmail, n.Title, n.Body, n.Link, emailBodyHTML, attachment); err != nil {
		log.Printf("workers: email delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if err := deps.Deliveries.MarkSent(ctx, d.ID, nil, ""); err != nil {
```

and in `attemptPush` change `deps.Deliveries.MarkSent(ctx, d.ID, responseJSON)` to `deps.Deliveries.MarkSent(ctx, d.ID, responseJSON, "")`.

*Test call-sites (mechanical).* The numbered `sed` edits below assume the test files are **still unmodified**; do them first.

`workers/*_test.go` — all 13 `SendEmail:` closures (dispatch_test.go: 135, 153, 346, 373, 393, 426, 454, 478, 516, 546; retry_test.go: 37, 56; consumer_test.go: 36). From the repo root with GNU sed:

```bash
# 1. signature: every SendEmail closure ends "*channels.EmailAttachment) error"
sed -i 's/\*channels\.EmailAttachment) error/*channels.EmailAttachment) (string, error)/' \
  workers/dispatch_test.go workers/retry_test.go workers/consumer_test.go
# 2. one-liners:  { return nil },  ->  { return "", nil },
sed -i 's/EmailAttachment) (string, error) { return nil }/EmailAttachment) (string, error) { return "", nil }/' \
  workers/dispatch_test.go workers/retry_test.go workers/consumer_test.go
# 3. multi-line bodies, by exact line (same-line edits, so numbers do not shift)
sed -i -E '154s/return errors\.New/return "", errors.New/; 374s/return errors\.New/return "", errors.New/; 394s/return errors\.New/return "", errors.New/; 427s/return errors\.New/return "", errors.New/; 480s/return nil/return "", nil/; 519s/return nil/return "", nil/; 549s/return nil/return "", nil/' workers/dispatch_test.go
sed -i -E '57s/return context\.DeadlineExceeded/return "", context.DeadlineExceeded/' workers/retry_test.go
```

Then check nothing was missed: `grep -rn "EmailAttachment) error" workers/` must print nothing. (If a line number has drifted, fix that closure by hand: every `return` inside a `SendEmail` closure needs a leading `"", `.)

`channels/email_test.go` — the call-sites that use the old single return (current line numbers): 19, 29, 40, 88 become `_, err := SendNotificationEmail(`; 61 becomes `_, err := sendViaResend(`; 197 and 220 become `if _, err := sendViaResend(`. Verify with `grep -n "SendNotificationEmail(\|sendViaResend(" channels/email_test.go` — every call must now start with `_, `.

*Fakes.* The two in-memory `deliveries.Store` fakes take the new parameter (same-line edits):

- `workers/dispatch_test.go`: `func (f *fakeDeliveries) MarkSent(_ context.Context, id string, resp []byte, _ string) error {`
- `controllers/notifications_test.go`: `func (f *fakeDeliveriesStore) MarkSent(_ context.Context, _ string, _ []byte, _ string) error { return nil }`

*Checkpoint of the migration:* Run `dgo sh -c 'go vet ./... && go vet -tags dbtest ./... && go test ./... -count=1'`
Expected: PASS — behaviour is unchanged.

- [ ] **Step 2: Write the failing tests**

(a) In `channels/email_test.go`, add:

```go
func TestSendViaResend_ReturnsProviderEmailID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	id, err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Subject", "<p>x</p>", "x", nil)
	if err != nil {
		t.Fatalf("sendViaResend: %v", err)
	}
	if id != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("provider id = %q, want the id from Resend's response", id)
	}
}

func TestSendViaResend_UnparseableSuccessBodyIsNotAnError(t *testing.T) {
	// The email was accepted; losing the id only means no webhook tracking.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	id, err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Subject", "<p>x</p>", "x", nil)
	if err != nil {
		t.Fatalf("a 2xx with a bad body must not be an error, got %v", err)
	}
	if id != "" {
		t.Fatalf("provider id = %q, want empty", id)
	}
}
```

(b) In `workers/dispatch_test.go`, make `fakeDeliveries` record the provider id. Add `sentProviderID string` to the struct (after `sentResp`), and change `MarkSent` to store it:

```go
func (f *fakeDeliveries) MarkSent(_ context.Context, id string, resp []byte, providerEmailID string) error {
	f.sentID, f.sentResp, f.sentProviderID = id, resp, providerEmailID
	return nil
}
```

Then add the test:

```go
func TestAttemptDelivery_Email_Success_StoresProviderEmailID(t *testing.T) {
	notifStore := &fakeNotifications{byID: map[string]notifications.Notification{"n1": testNotification("n1")}}
	delivStore := &fakeDeliveries{}
	deps := Deps{
		Notifications: notifStore,
		Deliveries:    delivStore,
		PushSubs:      &fakePushSubs{},
		SendEmail: func(_ config.Config, _, _, _, _, _ string, _ *channels.EmailAttachment) (string, error) {
			return "re_email_123", nil
		},
	}

	d := deliveries.Delivery{ID: "d1", NotificationID: "n1", TenantID: "t1", RecipientUserID: "u1", Channel: deliveries.ChannelEmail}
	attemptDelivery(context.Background(), deps, d)

	if delivStore.sentID != "d1" || delivStore.sentProviderID != "re_email_123" {
		t.Fatalf("MarkSent(id=%q, providerEmailID=%q), want (d1, re_email_123)", delivStore.sentID, delivStore.sentProviderID)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `dgo go test ./channels/ ./workers/ -run 'TestSendViaResend_ReturnsProviderEmailID|TestAttemptDelivery_Email_Success_StoresProviderEmailID' -count=1`
Expected: FAIL — `provider id = "", want the id from Resend's response` and `MarkSent(id="d1", providerEmailID=""), want (d1, re_email_123)`. (`TestSendViaResend_UnparseableSuccessBodyIsNotAnError` already passes: it pins the behaviour Step 4 must keep.)

- [ ] **Step 4: Implement the id capture**

`channels/email.go` — add the response type above `sendViaResend`:

```go
// resendResponse is the success body of Resend's POST /emails.
type resendResponse struct {
	ID string `json:"id"`
}
```

and replace the tail of `sendViaResend` (after the `resp.StatusCode >= 300` block, which keeps `return "", fmt.Errorf(…)`):

```go
	// The email was accepted. Its id is what Resend's later delivery webhooks
	// refer to; failing to read it must not fail a send that already happened.
	var accepted resendResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&accepted); err != nil {
		return "", nil
	}
	return accepted.ID, nil
}
```

`workers/dispatch.go` — `attemptEmail` now keeps the id:

```go
	providerID, err := deps.SendEmail(deps.Config, n.RecipientEmail, n.Title, n.Body, n.Link, emailBodyHTML, attachment)
	if err != nil {
		log.Printf("workers: email delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if err := deps.Deliveries.MarkSent(ctx, d.ID, nil, providerID); err != nil {
		log.Printf("workers: mark email delivery %s sent: %v", d.ID, err)
	}
	recordOutcome(ctx, deps, d, audit.ActionDeliverySent, map[string]any{})
```

- [ ] **Step 5: Run everything to verify it passes**

Run: `dgo sh -c 'go vet ./... && go vet -tags dbtest ./... && go test ./... -count=1'`
Expected: PASS, including `TestSendViaResend_ReturnsProviderEmailID`, `TestSendViaResend_UnparseableSuccessBodyIsNotAnError`, `TestAttemptDelivery_Email_Success_StoresProviderEmailID`.

- [ ] **Step 6: Checkpoint** — stage `channels/ workers/ deliveries/store.go controllers/notifications_test.go`; suggested message `feat(notify): record Resend's email id when an email is sent`.

---

### Task 6: HTTP handlers, routes, and the signing helper

**Files:**
- Create: `controllers/emailevents.go`
- Create: `controllers/emailevents_test.go`
- Modify: `main.go`
- Create: `scripts/signwebhook/main.go`

**Interfaces:**
- Consumes: `emailevents.Store`, `emailevents.Event`, `emailevents.ApplyResult`, `emailevents.StatusRow`, `emailevents.VerifySignature`, `emailevents.SignatureHeader`, `emailevents.StatusForEvent`, `emailevents.DeriveState`, `emailevents.ErrBadSecret`; the package-level `writeJSON`/`fail` helpers and `models.APIResponse` already in `controllers`.
- Produces — **the wire contract the backend plan depends on:**
  - `POST /api/webhooks/resend` (public; Svix headers `svix-id`, `svix-timestamp`, `svix-signature`). Always JSON; `{"success":true}` on 200.
  - `POST /api/deliveries/email-status` (header `X-Internal-Secret`). Request `{"tenantId":"<uuid>","notificationIds":["<uuid>", …≤200]}`. Response `200`:
    ```json
    {"success":true,"data":{"statuses":{"<notificationId>":{"state":"bounced","recipient":"a@b.com","detail":"…server-side only…","updatedAt":"2026-10-01T12:00:00Z"}}}}
    ```
    `state` ∈ `queued | sent | retrying | delayed | delivered | complained | bounced | failed | suppressed | skipped | unknown`. Notification ids with no email row are **absent** from `statuses`. `400` for: missing `tenantId`; empty `notificationIds`; more than 200; any id that is not a UUID.

- [ ] **Step 1: Write the failing tests** — `controllers/emailevents_test.go`:

```go
package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"stonesuite-notify/emailevents"
)

var evtSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("test-signing-key-0123456789abcdef"))

// fakeEmailEvents is an in-memory emailevents.Store.
type fakeEmailEvents struct {
	applied    []emailevents.Event
	applyRes   emailevents.ApplyResult
	applyErr   error
	statusRows []emailevents.StatusRow
	statusErr  error
	gotTenant  string
	gotIDs     []string
}

func (f *fakeEmailEvents) Apply(_ context.Context, ev emailevents.Event) (emailevents.ApplyResult, error) {
	f.applied = append(f.applied, ev)
	return f.applyRes, f.applyErr
}

func (f *fakeEmailEvents) StatusForNotifications(_ context.Context, tenantID string, ids []string) ([]emailevents.StatusRow, error) {
	f.gotTenant, f.gotIDs = tenantID, ids
	return f.statusRows, f.statusErr
}

var evtNow = time.Unix(1_790_000_000, 0)

func newEvtHandler(store emailevents.Store, secret string) *EmailEventsHandler {
	h := NewEmailEventsHandler(store, secret)
	h.Now = func() time.Time { return evtNow }
	return h
}

// signedRequest builds a webhook POST signed with secret at the handler's "now".
func signedRequest(t *testing.T, secret string, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(evtNow.Unix(), 10)
	sig, err := emailevents.SignatureHeader(secret, "msg_1", ts, []byte(body))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(body))
	req.Header.Set("svix-id", "msg_1")
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", sig)
	return req
}

const bounceBody = `{"type":"email.bounced","created_at":"2026-10-01T12:00:00.000Z","data":{"email_id":"em_1","to":["a@b.com"],"bounce":{"type":"Permanent","subType":"General","message":"mailbox does not exist"}}}`

func TestResendWebhook_ValidBounceIsApplied(t *testing.T) {
	store := &fakeEmailEvents{applyRes: emailevents.ApplyResult{Matched: true, Changed: true, Alerted: true}}
	h := newEvtHandler(store, evtSecret)

	rec := httptest.NewRecorder()
	h.Resend(rec, signedRequest(t, evtSecret, bounceBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.applied) != 1 {
		t.Fatalf("Apply called %d times, want 1", len(store.applied))
	}
	ev := store.applied[0]
	if ev.SvixID != "msg_1" || ev.ProviderEmailID != "em_1" || ev.Type != "email.bounced" {
		t.Fatalf("event = %+v", ev)
	}
	if !ev.OccurredAt.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("OccurredAt = %v, want the payload's created_at", ev.OccurredAt)
	}
	if !strings.Contains(ev.Detail, "mailbox does not exist") || !strings.Contains(ev.Detail, "Permanent") {
		t.Fatalf("Detail = %q, want the bounce type and message", ev.Detail)
	}
}

func TestResendWebhook_Rejections(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		req    func(t *testing.T) *http.Request
		want   int
	}{
		{"no secret configured", "", func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, bounceBody) }, http.StatusServiceUnavailable},
		{"secret is not a whsec_ value", "whsec_!!!", func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, bounceBody) }, http.StatusServiceUnavailable},
		{"missing headers", evtSecret, func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(bounceBody))
		}, http.StatusBadRequest},
		{"body tampered after signing", evtSecret, func(t *testing.T) *http.Request {
			req := signedRequest(t, evtSecret, bounceBody)
			req.Body = httptestBody(strings.Replace(bounceBody, "email.bounced", "email.delivered", 1))
			return req
		}, http.StatusBadRequest},
		{"signed with another secret", evtSecret, func(t *testing.T) *http.Request {
			other := "whsec_" + base64.StdEncoding.EncodeToString([]byte("another-signing-key-0123456789ab"))
			return signedRequest(t, other, bounceBody)
		}, http.StatusBadRequest},
		{"stale timestamp", evtSecret, func(t *testing.T) *http.Request {
			ts := strconv.FormatInt(evtNow.Add(-10*time.Minute).Unix(), 10)
			sig, _ := emailevents.SignatureHeader(evtSecret, "msg_1", ts, []byte(bounceBody))
			req := httptest.NewRequest(http.MethodPost, "/api/webhooks/resend", strings.NewReader(bounceBody))
			req.Header.Set("svix-id", "msg_1")
			req.Header.Set("svix-timestamp", ts)
			req.Header.Set("svix-signature", sig)
			return req
		}, http.StatusBadRequest},
		{"signed but not JSON", evtSecret, func(t *testing.T) *http.Request { return signedRequest(t, evtSecret, "not json") }, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			h := newEvtHandler(store, tc.secret)
			rec := httptest.NewRecorder()
			h.Resend(rec, tc.req(t))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			if len(store.applied) != 0 {
				t.Fatalf("a rejected webhook must not reach the store, got %d Apply calls", len(store.applied))
			}
		})
	}
}

func TestResendWebhook_IgnoredAndUnmatchedAre200(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"opened event", `{"type":"email.opened","created_at":"2026-10-01T12:00:00Z","data":{"email_id":"em_1"}}`},
		{"no email id", `{"type":"email.bounced","created_at":"2026-10-01T12:00:00Z","data":{}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			rec := httptest.NewRecorder()
			newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, tc.body))
			if rec.Code != http.StatusOK || len(store.applied) != 0 {
				t.Fatalf("status = %d with %d Apply calls, want 200 and 0", rec.Code, len(store.applied))
			}
		})
	}

	t.Run("event for an email notify did not send", func(t *testing.T) {
		store := &fakeEmailEvents{applyRes: emailevents.ApplyResult{Matched: false}}
		rec := httptest.NewRecorder()
		newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, bounceBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 so Resend does not retry", rec.Code)
		}
	})
}

func TestResendWebhook_StoreErrorIs500(t *testing.T) {
	store := &fakeEmailEvents{applyErr: errors.New("db down")}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).Resend(rec, signedRequest(t, evtSecret, bounceBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so Resend retries", rec.Code)
	}
}

const (
	notifA = "11111111-1111-1111-1111-111111111111"
	notifB = "22222222-2222-2222-2222-222222222222"
	notifC = "33333333-3333-3333-3333-333333333333"
)

func statusRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return httptest.NewRequest(http.MethodPost, "/api/deliveries/email-status", bytes.NewReader(b))
}

func TestEmailStatus_MapsRowsToClientStates(t *testing.T) {
	store := &fakeEmailEvents{statusRows: []emailevents.StatusRow{
		{NotificationID: notifA, Recipient: "a@b.com", QueueStatus: "sent", ProviderStatus: "delivery_delayed", UpdatedAt: evtNow},
		{NotificationID: notifB, Recipient: "c@d.com", QueueStatus: "pending", UpdatedAt: evtNow},
	}}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, map[string]any{
		"tenantId": "t-1", "notificationIds": []string{notifA, notifB, notifC},
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Statuses map[string]struct {
				State     string `json:"state"`
				Recipient string `json:"recipient"`
			} `json:"statuses"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Data.Statuses[notifA].State; got != "delayed" {
		t.Errorf("A state = %q, want delayed", got)
	}
	if got := resp.Data.Statuses[notifB].State; got != "queued" {
		t.Errorf("B state = %q, want queued", got)
	}
	if _, present := resp.Data.Statuses[notifC]; present {
		t.Error("an id with no email row must be absent from the map")
	}
	if store.gotTenant != "t-1" || len(store.gotIDs) != 3 {
		t.Errorf("store got tenant %q and %d ids, want t-1 and 3", store.gotTenant, len(store.gotIDs))
	}
}

func TestEmailStatus_Validation(t *testing.T) {
	tooMany := make([]string, 201)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}
	tests := []struct {
		name string
		body map[string]any
	}{
		{"missing tenant", map[string]any{"notificationIds": []string{notifA}}},
		{"no ids", map[string]any{"tenantId": "t-1", "notificationIds": []string{}}},
		{"too many ids", map[string]any{"tenantId": "t-1", "notificationIds": tooMany}},
		{"id is not a uuid", map[string]any{"tenantId": "t-1", "notificationIds": []string{"not-a-uuid"}}},
		{"sql-looking id", map[string]any{"tenantId": "t-1", "notificationIds": []string{"'; DROP TABLE x;--"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeEmailEvents{}
			rec := httptest.NewRecorder()
			newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if store.gotIDs != nil {
				t.Fatal("an invalid request must not reach the store")
			}
		})
	}

	t.Run("not JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newEvtHandler(&fakeEmailEvents{}, evtSecret).EmailStatus(rec,
			httptest.NewRequest(http.MethodPost, "/api/deliveries/email-status", strings.NewReader("nope")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestEmailStatus_StoreErrorIs500(t *testing.T) {
	store := &fakeEmailEvents{statusErr: errors.New("db down")}
	rec := httptest.NewRecorder()
	newEvtHandler(store, evtSecret).EmailStatus(rec, statusRequest(t, map[string]any{
		"tenantId": "t-1", "notificationIds": []string{notifA},
	}))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// httptestBody returns a request body reader for s.
func httptestBody(s string) *readCloser { return &readCloser{strings.NewReader(s)} }

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }
```

- [ ] **Step 2: Run to verify failure**

Run: `dgo go test ./controllers/ -run 'TestResendWebhook|TestEmailStatus' -count=1`
Expected: FAIL to compile — `undefined: EmailEventsHandler`, `NewEmailEventsHandler`.

- [ ] **Step 3: Implement** — `controllers/emailevents.go`:

```go
package controllers

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"stonesuite-notify/emailevents"
	"stonesuite-notify/models"
)

const (
	headerSvixID        = "svix-id"
	headerSvixTimestamp = "svix-timestamp"
	headerSvixSignature = "svix-signature"

	maxWebhookBodyBytes = 1 << 20
	maxStatusRequestIDs = 200
	maxEventDetailRunes = 500
)

// uuidPattern guards the status endpoint's ids before they reach a ::uuid cast.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// EmailEventsHandler serves the provider-webhook receiver and the internal
// email-status read. Now is injectable so tests can pin the replay window.
type EmailEventsHandler struct {
	Store  emailevents.Store
	Secret string
	Now    func() time.Time
}

// NewEmailEventsHandler builds the handler. An empty secret is allowed: the
// webhook route then answers 503 (see Resend).
func NewEmailEventsHandler(store emailevents.Store, secret string) *EmailEventsHandler {
	return &EmailEventsHandler{Store: store, Secret: secret, Now: time.Now}
}

// resendWebhook is the subset of Resend's webhook payload notify reads. The
// event-specific objects are optional: a missing one just yields no detail.
type resendWebhook struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		EmailID string `json:"email_id"`
		Bounce  *struct {
			Type    string `json:"type"`
			SubType string `json:"subType"`
			Message string `json:"message"`
		} `json:"bounce"`
		Suppressed *struct {
			Message string `json:"message"`
		} `json:"suppressed"`
		Failed *struct {
			Reason string `json:"reason"`
		} `json:"failed"`
	} `json:"data"`
}

// detail extracts the short provider diagnostic, if the event carries one.
func (w resendWebhook) detail() string {
	var d string
	switch {
	case w.Data.Bounce != nil:
		b := w.Data.Bounce
		kind := strings.TrimSpace(b.Type)
		if sub := strings.TrimSpace(b.SubType); sub != "" {
			kind += "/" + sub
		}
		d = strings.TrimSpace(strings.TrimSpace(kind) + ": " + strings.TrimSpace(b.Message))
		d = strings.TrimPrefix(d, ": ")
	case w.Data.Suppressed != nil:
		d = strings.TrimSpace(w.Data.Suppressed.Message)
	case w.Data.Failed != nil:
		d = strings.TrimSpace(w.Data.Failed.Reason)
	}
	if utf8.RuneCountInString(d) > maxEventDetailRunes {
		d = string([]rune(d)[:maxEventDetailRunes])
	}
	return d
}

// Resend handles POST /api/webhooks/resend — Resend reporting what happened to
// an email after accepting it. It is public (Resend holds no StoneSuite
// credentials); the Svix signature is its only gate, so nothing is read from
// the payload before it verifies.
func (h *EmailEventsHandler) Resend(w http.ResponseWriter, r *http.Request) {
	if h.Secret == "" {
		fail(w, http.StatusServiceUnavailable, "Webhook receiving is not configured.")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}

	msgID := r.Header.Get(headerSvixID)
	err = emailevents.VerifySignature(h.Secret, msgID, r.Header.Get(headerSvixTimestamp),
		r.Header.Get(headerSvixSignature), body, h.Now())
	switch {
	case errors.Is(err, emailevents.ErrBadSecret):
		log.Printf("emailevents: event=resend_webhook_misconfigured reason=%q", "RESEND_WEBHOOK_SECRET is not a valid whsec_ secret")
		fail(w, http.StatusServiceUnavailable, "Webhook receiving is not configured.")
		return
	case err != nil:
		// Never log the body, the signature headers or the secret.
		log.Printf("security_event=resend_webhook_rejected remote=%s", r.RemoteAddr)
		fail(w, http.StatusBadRequest, "Invalid signature.")
		return
	}

	var evt resendWebhook
	if err := json.Unmarshal(body, &evt); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if _, relevant := emailevents.StatusForEvent(evt.Type); !relevant || evt.Data.EmailID == "" {
		writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
		return
	}

	occurredAt := evt.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = h.Now()
	}
	res, err := h.Store.Apply(r.Context(), emailevents.Event{
		SvixID:          msgID,
		ProviderEmailID: evt.Data.EmailID,
		Type:            evt.Type,
		OccurredAt:      occurredAt,
		Detail:          evt.detail(),
	})
	if err != nil {
		log.Printf("emailevents: apply %s: %v", evt.Type, err)
		fail(w, http.StatusInternalServerError, "Failed to record event.")
		return
	}
	log.Printf("emailevents: event=%s matched=%v duplicate=%v changed=%v alerted=%v status=%s",
		evt.Type, res.Matched, res.Duplicate, res.Changed, res.Alerted, res.Status)

	writeJSON(w, http.StatusOK, models.APIResponse{Success: true})
}

type emailStatusRequest struct {
	TenantID        string   `json:"tenantId"`
	NotificationIDs []string `json:"notificationIds"`
}

// emailStatusEntry is one email's status as the backend reads it. Detail is
// the provider's diagnostic: this route is internal-secret gated and the
// backend keeps the detail server-side.
type emailStatusEntry struct {
	State     string    `json:"state"`
	Recipient string    `json:"recipient,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// EmailStatus handles POST /api/deliveries/email-status — the internal-secret
// gated batched read of "what actually happened to these emails". The tenant is
// named explicitly (there is no JWT on this path) and is part of the query, so
// an id from another tenant yields nothing.
func (h *EmailEventsHandler) EmailStatus(w http.ResponseWriter, r *http.Request) {
	var req emailStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	switch {
	case req.TenantID == "":
		fail(w, http.StatusBadRequest, "tenantId is required.")
		return
	case len(req.NotificationIDs) == 0:
		fail(w, http.StatusBadRequest, "notificationIds is required.")
		return
	case len(req.NotificationIDs) > maxStatusRequestIDs:
		fail(w, http.StatusBadRequest, "notificationIds has too many entries.")
		return
	}
	for _, id := range req.NotificationIDs {
		if !uuidPattern.MatchString(id) {
			fail(w, http.StatusBadRequest, "notificationIds must be UUIDs.")
			return
		}
	}

	rows, err := h.Store.StatusForNotifications(r.Context(), req.TenantID, req.NotificationIDs)
	if err != nil {
		log.Printf("emailevents: read status for tenant %s: %v", req.TenantID, err)
		fail(w, http.StatusInternalServerError, "Failed to load email status.")
		return
	}

	statuses := make(map[string]emailStatusEntry, len(rows))
	for _, row := range rows {
		statuses[row.NotificationID] = emailStatusEntry{
			State:     emailevents.DeriveState(row.QueueStatus, row.ProviderStatus),
			Recipient: row.Recipient,
			Detail:    row.Detail,
			UpdatedAt: row.UpdatedAt,
		}
	}
	writeJSON(w, http.StatusOK, models.APIResponse{
		Success: true,
		Data:    map[string]any{"statuses": statuses},
	})
}
```

- [ ] **Step 4: Run the handler tests to verify they pass**

Run: `dgo sh -c 'go vet ./controllers/ && go test ./controllers/ -run "TestResendWebhook|TestEmailStatus" -count=1 -v'`
Expected: PASS.

- [ ] **Step 5: Wire the routes in `main.go`**

Add the import `"stonesuite-notify/emailevents"`. Next to the other store constructors:

```go
	emailEventsStore := emailevents.NewPGStore(pool)
```

Next to the other handlers:

```go
	emailEventsHandler := controllers.NewEmailEventsHandler(emailEventsStore, cfg.ResendWebhookSecret)
```

After the existing "notice:" lines for email and push:

```go
	if !cfg.ResendWebhookConfigured() {
		log.Println("notice: no RESEND_WEBHOOK_SECRET — Resend delivery webhooks will be rejected (503); provider delivery status will not be tracked")
	}
```

Register the routes — the webhook with the other public routes (no auth wrapper, deliberately), the status read with the internal ones:

```go
	// Provider webhook: public by necessity (Resend holds no StoneSuite
	// credentials). The Svix signature check inside the handler is its ONLY gate.
	mux.HandleFunc("POST /api/webhooks/resend", emailEventsHandler.Resend)
```

```go
	mux.Handle("POST /api/deliveries/email-status", requireInternal(http.HandlerFunc(emailEventsHandler.EmailStatus)))
```

(Place the second line with the other `requireInternal` routes, after `GET /api/deliveries`.)

- [ ] **Step 6: Add the signing helper** — `scripts/signwebhook/main.go`:

```go
// Command signwebhook prints the Svix headers for a webhook body, so a
// developer can POST a hand-written event to a local
// /api/webhooks/resend without involving Resend. Local testing only.
//
// Usage:
//
//	go run ./scripts/signwebhook -secret whsec_... -body '{"type":"email.bounced",...}'
//	go run ./scripts/signwebhook -secret "$RESEND_WEBHOOK_SECRET" -id msg_demo_1 -body "$(cat event.json)"
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"stonesuite-notify/emailevents"
)

func main() {
	secret := flag.String("secret", os.Getenv("RESEND_WEBHOOK_SECRET"), "signing secret (whsec_…); defaults to $RESEND_WEBHOOK_SECRET")
	id := flag.String("id", "msg_local_"+strconv.FormatInt(time.Now().UnixNano(), 36), "svix-id (must be unique per event)")
	body := flag.String("body", "", "exact request body to sign")
	flag.Parse()

	if *secret == "" || *body == "" {
		fmt.Fprintln(os.Stderr, "signwebhook: -secret (or $RESEND_WEBHOOK_SECRET) and -body are required")
		os.Exit(1)
	}

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	signature, err := emailevents.SignatureHeader(*secret, *id, timestamp, []byte(*body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "signwebhook: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("svix-id: %s\nsvix-timestamp: %s\nsvix-signature: %s\n", *id, timestamp, signature)
}
```

- [ ] **Step 7: Verify the whole module still builds and passes**

Run: `dgo sh -c 'go vet ./... && go build ./... && go test ./... -count=1'`
Expected: PASS.

- [ ] **Step 8: Checkpoint** — stage `controllers/emailevents.go controllers/emailevents_test.go main.go scripts/signwebhook/main.go`; suggested message `feat(notify): receive Resend delivery webhooks and serve email status`.

---

### Task 7: `statusLink` / `statusResource` on create, and provider status in the delivery log

**Files:**
- Modify: `notifications/types.go` (`CreateInput`)
- Modify: `notifications/store.go` (`Create` SQL)
- Modify: `controllers/notifications.go` (`createNotificationRequest`, `Create`)
- Modify: `controllers/notifications_test.go` (`fakeStore`, three tests)
- Modify: `deliveries/types.go`, `deliveries/store.go` (`deliveryColumns`, `scanDelivery`)
- Create: `deliveries/types_test.go`
- Modify: `emailevents/store_dbtest_test.go` (one round-trip test through the real write/read paths)

**Interfaces:**
- Produces (wire): `POST /api/notifications/internal` accepts optional `"statusLink": "<≤300 chars>"` and `"statusResource": "<≤64 chars>"` (each 400 if longer); stored in `notifications.status_link` / `notifications.status_resource`. `statusResource` is the RBAC resource that governs the status page (e.g. `invoice`, `user`, `portal_access`); the delivery-problem alert is stored under it so the bell's `accessible_resources` filter shows the alert to the sender. `GET /api/notifications/{id}/deliveries` and `GET /api/deliveries` rows gain optional `providerStatus` (string) and `providerStatusAt` (RFC 3339).

- [ ] **Step 1: Write the failing tests**

In `controllers/notifications_test.go`, record create inputs in the fake. Add the field and the append:

```go
type fakeStore struct {
	rows             []notifications.Notification
	inputs           []notifications.CreateInput // every CreateInput seen, in order
	nextID           int
	forceErr         error
	notFoundErr      bool
	savedAttachments map[string]notifications.AttachmentInput // keyed by notificationID
}
```

At the top of `fakeStore.Create`, after the `forceErr` check, add `f.inputs = append(f.inputs, in)`. Then append three tests:

```go
func TestCreate_PersistsStatusLinkAndResource(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:       "t1",
		Recipients:     []recipientTarget{{UserID: "u1"}},
		EventType:      "invoice.approved",
		Resource:       "invoice",
		ResourceID:     "inv-1",
		Title:          "Invoice INV-1042 approved",
		StatusLink:     "/sales/invoices/inv-1",
		StatusResource: "invoice",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(store.inputs) != 1 || store.inputs[0].StatusLink != "/sales/invoices/inv-1" || store.inputs[0].StatusResource != "invoice" {
		t.Fatalf("CreateInput = %+v, want StatusLink and StatusResource to be passed through", store.inputs)
	}
}

func TestCreate_RejectsOverlongStatusResource(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:       "t1",
		Recipients:     []recipientTarget{{UserID: "u1"}},
		EventType:      "invoice.approved",
		Resource:       "invoice",
		ResourceID:     "inv-1",
		Title:          "Invoice INV-1042 approved",
		StatusResource: strings.Repeat("x", 65),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest || len(store.rows) != 0 {
		t.Fatalf("status = %d with %d rows, want 400 and 0 rows for a statusResource over 64 characters", rec.Code, len(store.rows))
	}
}

func TestCreate_RejectsOverlongStatusLink(t *testing.T) {
	store := &fakeStore{}
	h := newTestHandler(store, newFakePreferencesStore(), &fakeDeliveriesStore{}, config.Config{})

	body, _ := json.Marshal(createNotificationRequest{
		TenantID:   "t1",
		Recipients: []recipientTarget{{UserID: "u1"}},
		EventType:  "invoice.approved",
		Resource:   "invoice",
		ResourceID: "inv-1",
		Title:      "Invoice INV-1042 approved",
		StatusLink: strings.Repeat("x", 301),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/internal", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a statusLink over 300 characters", rec.Code)
	}
	if len(store.rows) != 0 {
		t.Fatalf("store has %d rows, want 0 — nothing is created for a rejected request", len(store.rows))
	}
}
```

(`strings` may need adding to that file's imports — check with the compiler.)

Create `deliveries/types_test.go`:

```go
package deliveries

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDelivery_JSON_ProviderStatus(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	t.Run("included when set", func(t *testing.T) {
		out, err := json.Marshal(Delivery{ID: "d1", ProviderStatus: "bounced", ProviderStatusAt: &at})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"providerStatus":"bounced"`) || !strings.Contains(s, `"providerStatusAt":"2026-10-01T12:00:00Z"`) {
			t.Fatalf("json = %s, want providerStatus and providerStatusAt", s)
		}
	})

	t.Run("omitted when unset", func(t *testing.T) {
		out, err := json.Marshal(Delivery{ID: "d1"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(out), "providerStatus") {
			t.Fatalf("json = %s, want no providerStatus fields", out)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

Run: `dgo go vet ./...`
Expected: FAIL — `unknown field StatusLink`, `unknown field ProviderStatus`.

- [ ] **Step 3: Implement**

`notifications/types.go` — add to `CreateInput` (after `Link`):

```go
	// StatusLink is where a delivery-problem alert for this notification's
	// email should send the user: the staff-side page that shows the email's
	// status. Optional; distinct from Link, which for a customer email is the
	// customer's URL.
	StatusLink string `json:"statusLink,omitempty"`
```

`notifications/store.go` — `Create`: add the column and one more placeholder/arg:

```go
	row := s.pool.QueryRow(ctx, `
		INSERT INTO notifications
			(tenant_id, recipient_user_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, body, link, visible_in_app, status_link)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+notificationColumns,
		in.TenantID, recipientUserIDArg, in.RecipientEmail, actorUserIDArg, in.EventType, in.Resource, in.ResourceID, in.Title, in.Body, in.Link, in.VisibleInApp, in.StatusLink)
```

(`notificationColumns` is unchanged — `status_link` is written here and read only by the alert query in `emailevents`.)

`controllers/notifications.go` — add to `createNotificationRequest` after `Link`:

```go
	// StatusLink is the staff-side page a delivery-problem alert for this
	// notification's email links to. Optional; at most maxStatusLinkChars.
	StatusLink string `json:"statusLink,omitempty"`
```

Add the constant beside `channelEmail`:

```go
// maxStatusLinkChars is the width of notifications.status_link.
const maxStatusLinkChars = 300
```

In `Create`, right after the `len(req.Recipients) == 0` check:

```go
	if utf8.RuneCountInString(req.StatusLink) > maxStatusLinkChars {
		fail(w, http.StatusBadRequest, "statusLink must be at most 300 characters.")
		return
	}
```

and add `StatusLink: req.StatusLink,` to the `notifications.CreateInput{…}` literal inside the recipients loop. Add `"unicode/utf8"` to the imports.

`deliveries/types.go` — add to `Delivery` after `ProviderResponse`:

```go
	// ProviderStatus is what the email provider reported after accepting the
	// message (delivered, bounced, …); empty until a webhook arrives, and
	// always empty for non-email channels. Distinct from Status, which is the
	// send-queue state.
	ProviderStatus   string     `json:"providerStatus,omitempty"`
	ProviderStatusAt *time.Time `json:"providerStatusAt,omitempty"`
```

`deliveries/store.go` — extend the column list and the scan (order must match):

```go
const deliveryColumns = `id, notification_id, tenant_id, recipient_user_id, channel, status, attempts, max_attempts, next_attempt_at, last_error, provider_response, provider_status, provider_status_at, created_at, updated_at`
```

In `scanDelivery` add `var providerStatus *string` beside `lastError`, change the `Scan` list to
`… &lastError, &d.ProviderResponse, &providerStatus, &d.ProviderStatusAt, &d.CreatedAt, &d.UpdatedAt,` and after the `lastError` handling:

```go
	if providerStatus != nil {
		d.ProviderStatus = *providerStatus
	}
```

- [ ] **Step 4: Run to verify everything passes**

Run: `dgo sh -c 'go vet ./... && go vet -tags dbtest ./... && go test ./... -count=1'`
Expected: PASS. Then re-run the DB suite to prove the widened `Create`/`scanDelivery` SQL is valid against real Postgres: `dgo go test -tags dbtest ./emailevents/ -count=1` (the helper with `TEST_DATABASE_URL`, as in Task 4) — PASS.

- [ ] **Step 5: Checkpoint** — stage `notifications/ controllers/notifications.go controllers/notifications_test.go deliveries/`; suggested message `feat(notify): accept statusLink on create and expose provider status in the delivery log`.

---

### Task 8: Documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] **Step 1:** In `README.md`, in the environment-variable table next to the `RESEND_API_KEY` row (line ~23), add:

```markdown
| `RESEND_WEBHOOK_SECRET` | no | Signing secret (`whsec_…`) for Resend's delivery webhooks at `POST /api/webhooks/resend`. Unset ⇒ the route answers 503 and provider delivery status is not tracked. |
```

- [ ] **Step 2:** In `CLAUDE.md`, add to the `Layering is store → handler…` list a bullet, and a short section after the "Key flow" paragraph:

```markdown
- `emailevents/` — what the email provider (Resend) reports *after* accepting a message (delivered / delayed / bounced / complained / failed / suppressed). Pure state machine (`status.go`), Svix signature verification (`svix.go`), and one transactional `Store.Apply` (dedupe by svix id → advance state → alert the sender). It never writes `notification_deliveries.status`, which is the send-queue state the workers claim on.
```

```markdown
## Provider delivery webhooks

Resend → `POST /api/webhooks/resend` (public; the Svix signature is its only gate — never put real work before `VerifySignature`). The email's Resend id is captured at send time (`channels.sendViaResend` → `deliveries.MarkSent(…, providerEmailID)`) and is the key that matches a webhook to its `notification_deliveries` row; SMTP sends have no id and therefore no webhook tracking. State only moves forward by rank (`emailevents.Advance`); the first problem state (delayed / complained / bounced / failed / suppressed) inserts an in-app `email.delivery_problem` notification for the original notification's `actor_user_id`, copying its `resource` so the bell's `accessible_resources` filter applies, linking to its optional `status_link`. Provider detail text is stored server-side and returned only by the internal-secret `POST /api/deliveries/email-status`; it must never reach an alert or a browser. Needs `RESEND_WEBHOOK_SECRET`. Locally: `go run ./scripts/signwebhook -secret … -body '…'` prints headers to sign a hand-written event.
```

- [ ] **Step 3: Checkpoint** — stage `CLAUDE.md README.md`; suggested message `docs(notify): document provider delivery webhooks`.

---

### Task 9: Whole-service verification and a local end-to-end check

**Files:** none modified (verification only).

- [ ] **Step 1: Static checks and the full unit + DB suites**

```bash
cd /p/WORKSPACE-SKOOKUM/stonesuite-notify && docker compose -f docker-compose.dev.yml up -d postgres
dgo() { MSYS_NO_PATHCONV=1 docker run --rm -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -e TEST_DATABASE_URL='postgres://notify:notify@host.docker.internal:5544/stonesuite_notify?sslmode=disable' \
  -v "P:\WORKSPACE-SKOOKUM\stonesuite-notify:/app" -v ss-go-build:/root/.cache/go-build \
  -v ss-go-mod:/go/pkg/mod -w /app golang:1.25.12-alpine "$@"; }
dgo sh -c 'go vet ./... && go vet -tags dbtest ./... && go test ./... -count=1 && go test -tags dbtest ./emailevents/ -count=1 -v'
```

Expected: everything PASS; the DB tests report `PASS`, not `SKIP`.

- [ ] **Step 2: Start the real service against the dev Postgres, with no email provider**

Pass the provider settings **explicitly empty** (godotenv does not override variables that are already set), so a stray `.env` in the repo can never make this run send real mail:

```bash
MSYS_NO_PATHCONV=1 docker run -d --rm --name notify-e2e -p 8090:8090 \
  -e DATABASE_URL='postgres://notify:notify@host.docker.internal:5544/stonesuite_notify?sslmode=disable' \
  -e JWT_SECRET=e2e_jwt_secret -e INTERNAL_SERVICE_SECRET=e2e_internal -e PORT=8090 \
  -e RESEND_API_KEY= -e SMTP_HOST= -e EMAIL_FROM= \
  -e RESEND_WEBHOOK_SECRET=whsec_dGVzdC1zaWduaW5nLWtleS0wMTIzNDU2Nzg5YWJjZGVm \
  -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
  -v "P:\WORKSPACE-SKOOKUM\stonesuite-notify:/app" -v ss-go-build:/root/.cache/go-build \
  -v ss-go-mod:/go/pkg/mod -w /app golang:1.25.12-alpine go run .
sleep 20 && curl -s localhost:8090/readyz && docker logs notify-e2e 2>&1 | grep -i "notice\|CRITICAL"
```

Expected: `{"success":true}`; no `CRITICAL`; no "RESEND_WEBHOOK_SECRET" notice (it is set).

- [ ] **Step 3: Seed one email delivery directly in SQL** (no real send), with a known actor:

```bash
PSQL="docker compose -f docker-compose.dev.yml exec -T postgres psql -U notify -d stonesuite_notify -v ON_ERROR_STOP=1 -At"
TENANT=11111111-1111-1111-1111-111111111111; ACTOR=33333333-3333-3333-3333-333333333333; RES=44444444-4444-4444-4444-444444444444
NID=$($PSQL -c "INSERT INTO notifications (tenant_id, recipient_email, actor_user_id, event_type, resource, resource_id, title, status_link) VALUES ('$TENANT','cust@example.com','$ACTOR','document.sent','invoice','$RES','Invoice sent','/sales/invoices/1') RETURNING id")
$PSQL -c "INSERT INTO notification_deliveries (notification_id, tenant_id, channel, status, provider_email_id) VALUES ('$NID','$TENANT','email','sent','em_e2e_$NID')"
echo "NID=$NID"
```

- [ ] **Step 4: Signed bounce → status and alert**

```bash
dgo2() { MSYS_NO_PATHCONV=1 docker run --rm -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false -v "P:\WORKSPACE-SKOOKUM\stonesuite-notify:/app" -v ss-go-build:/root/.cache/go-build -v ss-go-mod:/go/pkg/mod -w /app golang:1.25.12-alpine "$@"; }
BODY="{\"type\":\"email.bounced\",\"created_at\":\"2026-10-01T12:00:00.000Z\",\"data\":{\"email_id\":\"em_e2e_$NID\",\"bounce\":{\"type\":\"Permanent\",\"subType\":\"General\",\"message\":\"mailbox does not exist\"}}}"
HDRS=$(dgo2 go run ./scripts/signwebhook -secret whsec_dGVzdC1zaWduaW5nLWtleS0wMTIzNDU2Nzg5YWJjZGVm -body "$BODY")
ID=$(echo "$HDRS" | sed -n 's/^svix-id: //p'); TS=$(echo "$HDRS" | sed -n 's/^svix-timestamp: //p'); SIG=$(echo "$HDRS" | sed -n 's/^svix-signature: //p')
curl -s -w ' -> %{http_code}\n' -X POST localhost:8090/api/webhooks/resend -H "svix-id: $ID" -H "svix-timestamp: $TS" -H "svix-signature: $SIG" -d "$BODY"
curl -s -w ' -> %{http_code}\n' -X POST localhost:8090/api/webhooks/resend -H "svix-id: $ID" -H "svix-timestamp: $TS" -H "svix-signature: $SIG" -d "$BODY"   # replay
curl -s -w ' -> %{http_code}\n' -X POST localhost:8090/api/webhooks/resend -H "svix-id: $ID" -H "svix-timestamp: $TS" -H "svix-signature: v1,AAAA" -d "$BODY"   # bad signature
curl -s -X POST localhost:8090/api/deliveries/email-status -H 'X-Internal-Secret: e2e_internal' -H 'Content-Type: application/json' -d "{\"tenantId\":\"$TENANT\",\"notificationIds\":[\"$NID\"]}"
$PSQL -c "SELECT title, link, resource FROM notifications WHERE tenant_id='$TENANT' AND event_type='email.delivery_problem' AND recipient_user_id='$ACTOR'"
```

Expected:
1. first POST → `{"success":true} -> 200`; replay → `200` too (a duplicate is a no-op);
2. bad signature → `{"success":false,"message":"Invalid signature."} -> 400`;
3. email-status → `"state":"bounced"` for `$NID`, with `"recipient":"cust@example.com"` and the detail `Permanent/General: mailbox does not exist`;
4. exactly **one** alert row: title `Email to cust@example.com bounced`, link `/sales/invoices/1`, resource `invoice`;
5. `docker logs notify-e2e | grep security_event` shows one `resend_webhook_rejected` line and no body/header values.

- [ ] **Step 5: Tear down and clean the seed data**

```bash
docker stop notify-e2e
$PSQL -c "DELETE FROM notifications WHERE tenant_id='$TENANT' AND (id='$NID' OR event_type='email.delivery_problem')"
```

- [ ] **Step 6: Report** — summarise to the user: the test commands run and their pass counts, the E2E output, the files changed (`git status`), the suggested commit messages from each checkpoint, and that **nothing was committed**. Remind them the operator steps (Resend dashboard endpoint + `fly secrets set RESEND_WEBHOOK_SECRET …`, deploy order) are in spec §11 and are done by them, after this is deployed.

---

## Self-Review (run against the spec before handing off)

**Spec coverage (§4):**
- §4.1 schema → Task 1 (+ proven twice-idempotent in Task 4's `newPool`).
- §4.2 capture the Resend id → Task 5 (SMTP and unparseable body tolerated, tests included).
- §4.3 state model, ranks, derived client state → Task 2 (`Advance`, `DeriveState`, tables above mirror the spec).
- §4.4 webhook endpoint, response codes, raw body, 5-minute window, tenant-from-row → Tasks 3 (verify), 4 (`Apply`), 6 (handler, 400/503/200/500 matrix tested).
- §4.5 sender alert (resource copied, `visible_in_app = true`, fixed text, `status_link`, no actor ⇒ none, once per state) → Task 4 (`TestApply_*`, including the real `ListForUser` visibility check that resolves spec §9 item 2 at the notify level) + Task 7 (`statusLink` on create).
- §4.6 status read endpoint (tenant-scoped, ≤200, absent ids) + `providerStatus` on the existing delivery log → Tasks 6 and 7.
- §4.7 config → Task 1 (+ boot notice in Task 6 Step 5).
- §7/§8 security and failure modes → Global Constraints; unknown email id ⇒ 200 (Task 6 test), duplicate ⇒ no-op (Task 4), out-of-order ⇒ rank (Task 2), SMTP ⇒ no id (Task 5).
- Not in this plan by design: backend persistence/`EmailStatuses`/API fields (§5) and WebUI (§6) — companion plans.

**Placeholder scan:** none — every code step carries the code; the only judgement call left to the implementer is the mechanical test-closure edits in Task 5 Step 4, which are given as exact `sed` commands with line numbers and a compiler-checked fallback.

**Type consistency:** `Event{SvixID, ProviderEmailID, Type, OccurredAt, Detail}`, `ApplyResult{Matched, Duplicate, Changed, Alerted, Status}`, `StatusRow{NotificationID, Recipient, QueueStatus, ProviderStatus, Detail, UpdatedAt}`, `Store{Apply, StatusForNotifications}`, `VerifySignature(secret, msgID, timestamp, signatures, body, now)`, `SignatureHeader(secret, msgID, timestamp, body)`, `MarkSent(ctx, id, providerResponse, providerEmailID)` and `SendEmail … (string, error)` are used identically in Tasks 2–7.
