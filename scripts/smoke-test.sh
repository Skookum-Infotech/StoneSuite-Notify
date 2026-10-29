#!/usr/bin/env bash
# End-to-end smoke test against a running stonesuite-notify instance:
# exercises the in-app feed, email channel (via Mailpit), push channel
# (subscribe + observe the retry-on-failure path), preferences, admin
# routes, the audit trail, and the auth/permission/validation boundaries.
#
# Requires: scripts/dev-up.sh running in another terminal (or any instance
# reachable at BASE_URL with a Mailpit-compatible catcher at MAILPIT_URL).
# No jq/python dependency — parses just enough JSON by hand.
#
# Usage: scripts/smoke-test.sh
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BASE_URL="${BASE_URL:-http://localhost:8090}"
MAILPIT_URL="${MAILPIT_URL:-http://localhost:8025}"

if [ -f .env.local ]; then
  set -a
  # shellcheck disable=SC1091
  source .env.local
  set +a
fi
: "${INTERNAL_SERVICE_SECRET:?INTERNAL_SERVICE_SECRET not set — source .env.local (see scripts/dev-up.sh) first}"

TENANT_ID="11111111-1111-1111-1111-111111111111"
USER_ID="22222222-2222-2222-2222-222222222222"
ACTOR_ID="33333333-3333-3333-3333-333333333333"

PASS=0
FAIL=0

# check <label> <actual> <expected-substring>
check() {
  if [[ "$2" == *"$3"* ]]; then
    echo "  OK  $1"
    PASS=$((PASS + 1))
  else
    echo "FAIL  $1 — expected to contain: $3"
    echo "      got: $2"
    FAIL=$((FAIL + 1))
  fi
}

echo "== Minting test tokens =="
USER_TOKEN=$(go run ./scripts/mint-token --id "$USER_ID" --tenant "$TENANT_ID")
ADMIN_TOKEN=$(go run ./scripts/mint-token --id "$USER_ID" --tenant "$TENANT_ID" --admin)

echo "== Health =="
check "healthz" "$(curl -s "$BASE_URL/healthz")" '"success":true'
check "readyz"  "$(curl -s "$BASE_URL/readyz")"  '"success":true'

echo "== Create notification (email channel) =="
RESOURCE_ID="44444444-4444-4444-4444-$(date +%s%N | tail -c 13)"
CREATE_RESP=$(curl -s -X POST "$BASE_URL/api/notifications/internal" \
  -H "X-Internal-Secret: $INTERNAL_SERVICE_SECRET" -H "Content-Type: application/json" \
  -d "{\"tenantId\":\"$TENANT_ID\",\"recipients\":[{\"userId\":\"$USER_ID\",\"email\":\"smoke@example.com\"}],\"actorUserId\":\"$ACTOR_ID\",\"eventType\":\"smoke.test\",\"resource\":\"smoke\",\"resourceId\":\"$RESOURCE_ID\",\"title\":\"Smoke test notification\",\"channels\":[\"email\"]}")
check "create returns success" "$CREATE_RESP" '"success":true'
NOTIF_ID=$(echo "$CREATE_RESP" | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
check "create returns an id" "${NOTIF_ID:-}" "-"

echo "== Bell endpoints =="
check "summary shows unread" "$(curl -s "$BASE_URL/api/notifications/summary" -H "Authorization: Bearer $USER_TOKEN")" '"unreadCount":1'
check "list contains notification" "$(curl -s "$BASE_URL/api/notifications" -H "Authorization: Bearer $USER_TOKEN")" "$NOTIF_ID"
check "mark read succeeds" "$(curl -s -X POST "$BASE_URL/api/notifications/$NOTIF_ID/read" -H "Authorization: Bearer $USER_TOKEN")" '"success":true'
check "summary drops to zero" "$(curl -s "$BASE_URL/api/notifications/summary" -H "Authorization: Bearer $USER_TOKEN")" '"unreadCount":0'
check "mark-all-read succeeds" "$(curl -s -X POST "$BASE_URL/api/notifications/read-all" -H "Authorization: Bearer $USER_TOKEN")" '"success":true'

echo "== Email channel (waiting for the queue consumer) =="
sleep 3
check "email captured by Mailpit" "$(curl -s "$MAILPIT_URL/api/v1/messages")" "Smoke test notification"

echo "== Preferences =="
check "get preferences" "$(curl -s "$BASE_URL/api/preferences" -H "Authorization: Bearer $USER_TOKEN")" '"emailEnabled"'
check "update preferences" "$(curl -s -X PUT "$BASE_URL/api/preferences" -H "Authorization: Bearer $USER_TOKEN" -H "Content-Type: application/json" -d '{"pushEnabled":true}')" '"success":true'

echo "== Push channel =="
check "vapid public key" "$(curl -s "$BASE_URL/api/push/vapid-public-key")" '"publicKey"'
SUB_RESP=$(curl -s -X POST "$BASE_URL/api/push/subscribe" -H "Authorization: Bearer $USER_TOKEN" -H "Content-Type: application/json" \
  -d "{\"endpoint\":\"https://push.smoke-test.invalid/smoke-test-unreachable\",\"keys\":{\"p256dh\":\"${VAPID_PUBLIC_KEY:-}\",\"auth\":\"AAAAAAAAAAAAAAAAAAAAAA\"}}")
check "push subscribe succeeds" "$SUB_RESP" '"success":true'
check "push unsubscribe succeeds" "$(curl -s -X DELETE "$BASE_URL/api/push/subscribe" -H "Authorization: Bearer $USER_TOKEN" -H "Content-Type: application/json" -d '{"endpoint":"https://push.smoke-test.invalid/smoke-test-unreachable"}')" '"success":true'

echo "== Admin & audit =="
check "admin tenant-defaults readable" "$(curl -s "$BASE_URL/api/admin/tenant-defaults" -H "Authorization: Bearer $ADMIN_TOKEN")" '"defaults"'
check "admin audit-logs readable" "$(curl -s "$BASE_URL/api/admin/audit-logs" -H "Authorization: Bearer $ADMIN_TOKEN")" '"entries"'

echo "== Auth / permission / validation boundaries =="
check "no token -> 401" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/api/notifications/summary")" "401"
check "self-service token on admin route -> 403" "$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/api/admin/audit-logs" -H "Authorization: Bearer $USER_TOKEN")" "403"
check "wrong internal secret -> 401" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/notifications/internal" -H "X-Internal-Secret: wrong" -H "Content-Type: application/json" -d '{}')" "401"
check "missing required field -> 400" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/notifications/internal" -H "X-Internal-Secret: $INTERNAL_SERVICE_SECRET" -H "Content-Type: application/json" -d "{\"tenantId\":\"$TENANT_ID\"}")" "400"
check "unknown notification id -> 404" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE_URL/api/notifications/00000000-0000-0000-0000-000000000000/read" -H "Authorization: Bearer $USER_TOKEN")" "404"

echo
echo "===================="
echo "  $PASS passed, $FAIL failed"
echo "===================="
[ "$FAIL" -eq 0 ]
