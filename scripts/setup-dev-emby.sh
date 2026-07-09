#!/usr/bin/env bash
set -euo pipefail

# Initializes the official Emby dev container without using the web wizard.
# Defaults match docker-compose.dev.yml.

EMBY_URL="${EMBY_URL:-http://127.0.0.1:${EMBY_PORT:-8096}/emby}"
EMBY_ADMIN_USER="${EMBY_ADMIN_USER:-embyadmin}"
EMBY_ADMIN_PASSWORD="${EMBY_ADMIN_PASSWORD:-embytest}"
EMBY_MEDIA_PATH="${EMBY_MEDIA_PATH:-/mnt/share1}"
EMBY_LIBRARY_NAME="${EMBY_LIBRARY_NAME:-Movies}"
EMBY_LIBRARY_TYPE="${EMBY_LIBRARY_TYPE:-movies}"

EMBY_URL="${EMBY_URL%/}"

command -v curl >/dev/null 2>&1 || {
  printf 'curl is required\n' >&2
  exit 2
}

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

request() {
  local method="$1"
  local url="$2"
  local body_file="$3"
  shift 3
  curl -sS -o "$body_file" -w '%{http_code}' -X "$method" "$url" "$@"
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

wait_for_emby() {
	local body="$tmpdir/public-info.json"
	for _ in $(seq 1 90); do
		if code="$(request GET "$EMBY_URL/System/Info/Public" "$body" 2>/dev/null)" && [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
			return 0
		fi
    sleep 2
  done
  printf 'Emby did not become ready at %s\n' "$EMBY_URL" >&2
  return 1
}

login_admin() {
  local body="$tmpdir/login.json"
  local payload
  payload="{\"Username\":\"$(json_string "$EMBY_ADMIN_USER")\",\"Pw\":\"$(json_string "$EMBY_ADMIN_PASSWORD")\"}"
  code="$(request POST "$EMBY_URL/Users/AuthenticateByName" "$body" \
    -H 'Content-Type: application/json' \
    -H 'X-Emby-Authorization: Emby Client="dev-setup", Device="curl", DeviceId="dev-setup", Version="1"' \
    --data-binary "$payload")"
  if [[ "$code" =~ ^2[0-9][0-9]$ ]]; then
    extract_token "$body"
    return 0
  fi
  return 1
}

initialize_wizard() {
  local body="$tmpdir/startup.json"
  request POST "$EMBY_URL/Startup/Configuration" "$body" \
    -H 'Content-Type: application/json' \
    --data-binary '{"UICulture":"en-us","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' >/dev/null || true

  local payload
  payload="{\"Name\":\"$(json_string "$EMBY_ADMIN_USER")\",\"Password\":\"$(json_string "$EMBY_ADMIN_PASSWORD")\"}"
  request POST "$EMBY_URL/Startup/User" "$body" \
    -H 'Content-Type: application/json' \
    --data-binary "$payload" >/dev/null || true

  request POST "$EMBY_URL/Startup/Complete" "$body" >/dev/null || true
}

create_library() {
  local token="$1"
  local body="$tmpdir/library.txt"
  local code
  code="$(request POST "$EMBY_URL/Library/VirtualFolders?name=$EMBY_LIBRARY_NAME&collectionType=$EMBY_LIBRARY_TYPE&paths=$EMBY_MEDIA_PATH&refreshLibrary=false&api_key=$token" "$body")"
  case "$code" in
    200|204|400) return 0 ;;
  esac
  printf 'create library failed with status %s\n' "$code" >&2
  sed 's/^/  /' "$body" >&2 || true
  return 1
}

wait_for_emby
token="$(login_admin || true)"
if [[ -z "$token" ]]; then
  initialize_wizard
  token="$(login_admin || true)"
fi
if [[ -z "$token" ]]; then
  printf 'failed to login Emby admin %q at %s\n' "$EMBY_ADMIN_USER" "$EMBY_URL" >&2
  exit 1
fi
create_library "$token"
printf 'ok: Emby dev server initialized at %s with admin %s\n' "$EMBY_URL" "$EMBY_ADMIN_USER"
