#!/usr/bin/env bash
set -euo pipefail

# Required: USERNAME, PASSWORD
# Optional: GATEWAY_URL, PB_URL, GATEWAY_BASE_PATH, SYNTHETIC_USER_ID,
#           SMOKE_OPTIONAL_MEDIA, SMOKE_M3U8_PATH, CURL_OPTS

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8090}"
PB_URL="${PB_URL:-$GATEWAY_URL}"
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH:-/emby}"
USERNAME="${USERNAME:-}"
PASSWORD="${PASSWORD:-}"
SYNTHETIC_USER_ID="${SYNTHETIC_USER_ID:-}"
SMOKE_OPTIONAL_MEDIA="${SMOKE_OPTIONAL_MEDIA:-0}"
SMOKE_M3U8_PATH="${SMOKE_M3U8_PATH:-}"

if [[ -z "$USERNAME" || -z "$PASSWORD" ]]; then
  printf 'USERNAME and PASSWORD are required\n' >&2
  exit 2
fi

command -v curl >/dev/null 2>&1 || {
  printf 'curl is required\n' >&2
  exit 2
}

GATEWAY_URL="${GATEWAY_URL%/}"
PB_URL="${PB_URL%/}"
GATEWAY_BASE_PATH="/${GATEWAY_BASE_PATH#/}"
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH%/}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

curl_extra=()
if [[ -n "${CURL_OPTS:-}" ]]; then
  # shellcheck disable=SC2206
  curl_extra=(${CURL_OPTS})
fi

request() {
  local method="$1"
  local url="$2"
  local body_file="$3"
  shift 3
  curl -sS "${curl_extra[@]}" -o "$body_file" -w '%{http_code}' -X "$method" "$url" "$@"
}

expect_2xx() {
  local label="$1"
  local code="$2"
  local body_file="$3"
  if [[ ! "$code" =~ ^2[0-9][0-9]$ ]]; then
    printf '%s failed: expected 2xx, got %s\n' "$label" "$code" >&2
    sed 's/^/  /' "$body_file" >&2 || true
    exit 1
  fi
  printf 'ok: %s (%s)\n' "$label" "$code"
}

expect_status() {
  local label="$1"
  local expected="$2"
  local code="$3"
  local body_file="$4"
  if [[ "$code" != "$expected" ]]; then
    printf '%s failed: expected %s, got %s\n' "$label" "$expected" "$code" >&2
    sed 's/^/  /' "$body_file" >&2 || true
    exit 1
  fi
  printf 'ok: %s (%s)\n' "$label" "$code"
}

expect_not_2xx() {
  local label="$1"
  local code="$2"
  local body_file="$3"
  if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
    printf '%s failed: expected non-2xx, got %s\n' "$label" "$code" >&2
    sed 's/^/  /' "$body_file" >&2 || true
    exit 1
  fi
  printf 'ok: %s rejected anonymous access (%s)\n' "$label" "$code"
}

json_string() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

extract_token() {
  local body_file="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r '.AccessToken // empty' "$body_file"
    return
  fi
  sed -n 's/.*"AccessToken"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$body_file" | head -n 1
}

body="$tmpdir/public-info.json"
code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/System/Info/Public" "$body")"
expect_2xx 'anonymous gateway System/Info/Public' "$code" "$body"

for collection in gateway_users emby_servers backend_accounts user_mappings gateway_sessions audit_logs; do
  body="$tmpdir/pb-$collection.json"
  code="$(request GET "$PB_URL/api/collections/$collection/records" "$body")"
  expect_not_2xx "anonymous PB $collection records" "$code" "$body"
done

login_body="$tmpdir/login-request.json"
printf '{"Username":"%s","Pw":"%s"}' "$(json_string "$USERNAME")" "$(json_string "$PASSWORD")" >"$login_body"

body="$tmpdir/login-response.json"
code="$(request POST "$GATEWAY_URL$GATEWAY_BASE_PATH/Users/AuthenticateByName" "$body" \
  -H 'Content-Type: application/json' \
  -H 'X-Emby-Authorization: Emby Client="smoke", Device="curl", DeviceId="smoke-script", Version="1"' \
  --data-binary "@$login_body")"
expect_2xx 'gateway login' "$code" "$body"

token="$(extract_token "$body")"
if [[ -z "$token" || "$token" == "null" ]]; then
  printf 'gateway login did not return AccessToken\n' >&2
  sed 's/^/  /' "$body" >&2 || true
  exit 1
fi
printf 'ok: gateway token issued\n'

body="$tmpdir/system-info.json"
code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/System/Info" "$body" -H "X-Emby-Token: $token")"
expect_2xx 'authenticated gateway System/Info' "$code" "$body"

if [[ "$SMOKE_OPTIONAL_MEDIA" == "1" ]]; then
  if [[ -z "$SYNTHETIC_USER_ID" ]]; then
    printf 'SMOKE_OPTIONAL_MEDIA=1 requires SYNTHETIC_USER_ID\n' >&2
    exit 2
  fi
  body="$tmpdir/items.json"
  code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/Users/$SYNTHETIC_USER_ID/Items?UserId=$SYNTHETIC_USER_ID&api_key=$token" "$body")"
  expect_2xx 'optional gateway Items' "$code" "$body"

  if [[ -n "$SMOKE_M3U8_PATH" ]]; then
    body="$tmpdir/media.m3u8"
    code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/${SMOKE_M3U8_PATH#/}?api_key=$token" "$body")"
    expect_2xx 'optional gateway m3u8' "$code" "$body"
  fi
fi

body="$tmpdir/logout.txt"
code="$(request POST "$GATEWAY_URL$GATEWAY_BASE_PATH/Sessions/Logout" "$body" -H "X-Emby-Token: $token")"
expect_2xx 'gateway logout' "$code" "$body"

body="$tmpdir/post-logout-system-info.txt"
code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/System/Info" "$body" -H "X-Emby-Token: $token")"
expect_status 'revoked token rejected' 401 "$code" "$body"

printf 'smoke passed\n'
