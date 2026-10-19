# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go build ./...              # build everything
go run .                    # run the service locally (needs env vars, see below)
go test ./...                # run all tests
go test ./controllers/...    # test a single package
go test ./controllers/ -run TestName   # run a single test by name
go vet ./...                 # static analysis
```

Required environment variables (service fails fast on `config.Load()` if missing): `DATABASE_URL`, `JWT_SECRET`, `INTERNAL_SERVICE_SECRET`. Email (`RESEND_API_KEY` or `SMTP_HOST`/`SMTP_PORT`/`SMTP_USERNAME`/`SMTP_PASSWORD`) and push (`VAPID_PUBLIC_KEY`/`VAPID_PRIVATE_KEY`/`VAPID_SUBJECT`) are optional — the corresponding channel is skipped (logged) if unconfigured. `PORT` defaults to `8090`, `CORS_ORIGIN` is a comma-separated allowlist.

## Architecture

This is `stonesuite-notify`, a standalone Go microservice within the larger StoneSuite system. It does **not** own user identity or login — it trusts sessions minted by StoneSuite-Backend and validates the same JWT (`JWT_SECRET` must match that service). Other StoneSuite services push notifications into it over an internal-secret-gated endpoint rather than this service reaching out to them.

Request flow: `main.go` wires everything by hand (no framework, no DI container) — loads config, opens the Postgres pool, applies the embedded schema, constructs stores → handlers, and registers routes on `net/http`'s `ServeMux` (Go 1.22+ method+path patterns like `"POST /api/notifications/{id}/read"`). Two middleware wrap handlers: `middleware.RequireAuth` (end-user JWT, from `Authorization: Bearer` header or the `auth_token` cookie) and `middleware.RequireInternalSecret` (shared-secret header `X-Internal-Secret`, for service-to-service calls). CORS and request logging wrap the whole mux.

Layering is store → handler, per top-level package:
- `notifications/` — core domain (the in-app feed). `Store` is an interface; `PGStore` is the Postgres implementation. Handlers depend on the interface so they're testable with fakes.
- `pushsubs/` — same store/interface pattern, for Web Push subscription rows (one per browser/device, upserted by `endpoint`).
- `controllers/` — HTTP handlers translating requests to store calls. `Handler` (notifications) and `PushHandler` (web push) share the same package so they can share `writeJSON`/`fail` helpers.
- `channels/` — fire-and-forget side-channel delivery (email via Resend-then-SMTP fallback, web push via VAPID). Failures here are logged, never surfaced to the create-notification response, since the in-app row is already the source of truth.
- `emailevents/` — what Resend reports about an email after accepting it (delivered / bounced / …), via the public Svix-signed `POST /api/webhooks/resend`.
- `inbound/` — a customer's reply received by Resend (`email.received` on that same webhook): fetches the mail from Resend's Receiving API and forwards it to StoneSuite-Backend's `POST /api/internal/email/inbound`, HMAC-signed with `INTERNAL_SERVICE_SECRET`. Needs `REPLY_EMAIL_DOMAIN` (must equal the backend's), `BACKEND_URL`, `RESEND_API_KEY`. Notify never decides which record a reply belongs to; the webhook answers 5xx only for failures a retry could fix, so one stuck forward cannot get the shared endpoint disabled by Resend.
- `middleware/` — JWT auth and internal-secret auth.
- `database/` — connection pool + schema application. `database/migrations/schema.sql` is embedded via `//go:embed` and applied on every boot; every statement is `IF NOT EXISTS`/idempotent, so there is no separate migration runner or version table — editing that file *is* the migration.
- `models/` — the single shared `APIResponse{success, message, data}` envelope every handler returns.

Key flow worth knowing before touching `controllers.Handler.Create`: it's the internal endpoint other StoneSuite services call when a business event fires. It accepts multiple recipients (the caller resolves role membership / addressing, since this service has no user directory), validates all inputs up front so a bad entry rejects the whole batch, creates the in-app row synchronously, then dispatches email/push for each in a detached goroutine (`dispatchChannels`) so a slow provider never delays the response. A push subscription the provider reports as gone (404/410) is pruned automatically.

Auth note: StoneSuite-Backend's JWTs carry the user's identity only in the `id` claim (no separate `user_id`), so `middleware.UserContext.UserID` is set from `id` too — this is intentional, not a bug.
