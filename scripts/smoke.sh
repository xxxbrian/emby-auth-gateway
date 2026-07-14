#!/usr/bin/env bash
set -euo pipefail

# Required (unless SMOKE_WEB_ONLY=1 or SMOKE_HEADER_SELFTEST=1):
#   SMOKE_USERNAME/SMOKE_PASSWORD (USERNAME/PASSWORD still accepted)
# Optional:
#   GATEWAY_URL, PB_URL, GATEWAY_BASE_PATH, SYNTHETIC_USER_ID,
#   SMOKE_OPTIONAL_MEDIA, SMOKE_M3U8_PATH, CURL_OPTS,
#   SMOKE_WEB (disabled|ready),
#   SMOKE_WEB_ONLY=1 (test-only: Web checks alone; requires SMOKE_WEB),
#   SMOKE_WEB_NON_CANARY_PATH (test-only: relative path under /emby/web/ that
#     must serve 2xx without CORS grant when SMOKE_WEB=ready),
#   SMOKE_HEADER_SELFTEST=1 (test-only: header-parser fixtures, no network)

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8090}"
PB_URL="${PB_URL:-$GATEWAY_URL}"
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH:-/emby}"
USERNAME="${SMOKE_USERNAME:-${USERNAME:-}}"
PASSWORD="${SMOKE_PASSWORD:-${PASSWORD:-}}"
SYNTHETIC_USER_ID="${SYNTHETIC_USER_ID:-}"
SMOKE_OPTIONAL_MEDIA="${SMOKE_OPTIONAL_MEDIA:-0}"
SMOKE_M3U8_PATH="${SMOKE_M3U8_PATH:-}"
SMOKE_WEB="${SMOKE_WEB:-}"
SMOKE_WEB_ONLY="${SMOKE_WEB_ONLY:-0}"
SMOKE_WEB_NON_CANARY_PATH="${SMOKE_WEB_NON_CANARY_PATH:-}"
SMOKE_HEADER_SELFTEST="${SMOKE_HEADER_SELFTEST:-0}"

case "$SMOKE_WEB" in
  '' | 0 | false | no) SMOKE_WEB="" ;;
  disabled | ready) ;;
  *)
    printf 'SMOKE_WEB must be empty, disabled, or ready (got %q)\n' "$SMOKE_WEB" >&2
    exit 2
    ;;
esac

case "$SMOKE_WEB_ONLY" in
  1 | true | yes) SMOKE_WEB_ONLY=1 ;;
  '' | 0 | false | no) SMOKE_WEB_ONLY=0 ;;
  *)
    printf 'SMOKE_WEB_ONLY must be 0 or 1 (got %q)\n' "$SMOKE_WEB_ONLY" >&2
    exit 2
    ;;
esac

case "$SMOKE_HEADER_SELFTEST" in
  1 | true | yes) SMOKE_HEADER_SELFTEST=1 ;;
  '' | 0 | false | no) SMOKE_HEADER_SELFTEST=0 ;;
  *)
    printf 'SMOKE_HEADER_SELFTEST must be 0 or 1 (got %q)\n' "$SMOKE_HEADER_SELFTEST" >&2
    exit 2
    ;;
esac

if [[ "$SMOKE_HEADER_SELFTEST" != "1" ]]; then
  if [[ "$SMOKE_WEB_ONLY" == "1" ]]; then
    if [[ "$SMOKE_WEB" != "disabled" && "$SMOKE_WEB" != "ready" ]]; then
      printf 'SMOKE_WEB_ONLY=1 requires SMOKE_WEB=disabled or SMOKE_WEB=ready\n' >&2
      exit 2
    fi
  elif [[ -z "$USERNAME" || -z "$PASSWORD" ]]; then
    printf 'USERNAME and PASSWORD are required (or set SMOKE_WEB_ONLY=1 with SMOKE_WEB)\n' >&2
    exit 2
  fi

  command -v curl >/dev/null 2>&1 || {
    printf 'curl is required\n' >&2
    exit 2
  }
fi

GATEWAY_URL="${GATEWAY_URL%/}"
PB_URL="${PB_URL%/}"
GATEWAY_BASE_PATH="/${GATEWAY_BASE_PATH#/}"
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH%/}"

# Fixed Web v1 mount (independent of GATEWAY_BASE_PATH for API routes).
WEB_PREFIX="/emby/web"
WEB_CORS_ORIGIN="https://app.emby.media"
WEB_CANARIES=(
  "manifest.json"
  "index.html"
  "strings/en-US.json"
)
# Preflight Vary tokens required by embyweb canary OPTIONS contract.
WEB_PREFLIGHT_VARY=(
  "Origin"
  "Access-Control-Request-Method"
  "Access-Control-Request-Headers"
  "Access-Control-Request-Private-Network"
)

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

curl_extra=()
if [[ -n "${CURL_OPTS:-}" ]]; then
  # shellcheck disable=SC2206
  curl_extra=(${CURL_OPTS})
fi

# Always pass -q first so user/global curlrc cannot alter smoke behavior.
curl_base=(curl -q -sS)

# Runs curl; on non-zero exit prints diagnostics and exits 1.
# Writes body to body_file; prints HTTP status code on stdout.
request() {
  local method="$1"
  local url="$2"
  local body_file="$3"
  shift 3
  local code
  local curl_status=0
  code="$("${curl_base[@]}" "${curl_extra[@]}" -o "$body_file" -w '%{http_code}' -X "$method" "$url" "$@" 2>"$tmpdir/curl.err")" || curl_status=$?
  if [[ "$curl_status" -ne 0 ]]; then
    printf 'curl failed (%s) %s %s\n' "$curl_status" "$method" "$url" >&2
    sed 's/^/  /' "$tmpdir/curl.err" >&2 || true
    exit 1
  fi
  if [[ ! "$code" =~ ^[0-9][0-9][0-9]$ ]]; then
    printf 'curl returned invalid status %q for %s %s\n' "$code" "$method" "$url" >&2
    exit 1
  fi
  printf '%s' "$code"
}

# Like request, but also writes response headers to headers_file (-D).
request_with_headers() {
  local method="$1"
  local url="$2"
  local body_file="$3"
  local headers_file="$4"
  shift 4
  local code
  local curl_status=0
  code="$("${curl_base[@]}" "${curl_extra[@]}" -D "$headers_file" -o "$body_file" -w '%{http_code}' -X "$method" "$url" "$@" 2>"$tmpdir/curl.err")" || curl_status=$?
  if [[ "$curl_status" -ne 0 ]]; then
    printf 'curl failed (%s) %s %s\n' "$curl_status" "$method" "$url" >&2
    sed 's/^/  /' "$tmpdir/curl.err" >&2 || true
    exit 1
  fi
  if [[ ! "$code" =~ ^[0-9][0-9][0-9]$ ]]; then
    printf 'curl returned invalid status %q for %s %s\n' "$code" "$method" "$url" >&2
    exit 1
  fi
  printf '%s' "$code"
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

# ---------------------------------------------------------------------------
# Header parsing (curl -D dumps)
#
# - Selects the *final* HTTP response block (last HTTP/x status line), so
#   interim 3xx/1xx blocks cannot hide final headers.
# - Collects *every* case-insensitive occurrence in that block.
# - Singleton CORS/Cache-Control headers: expected present => exactly one
#   occurrence; expected absent => zero occurrences (empty-first + later grant
#   cannot hide).
# - Vary: aggregates comma-separated tokens across all Vary lines in the block.
# - CRLF-safe.
# ---------------------------------------------------------------------------

# Print every value of header name from the final response block (one per line).
header_values() {
  local headers_file="$1"
  local name="$2"
  awk -v name="$name" '
    BEGIN { want = tolower(name); n = 0; in_headers = 0 }
    {
      line = $0
      sub(/\r$/, "", line)
      # Final-block selection: each HTTP status line starts a new block.
      # Portable check (BSD/macOS awk-friendly; no character classes in ~).
      if (index(line, "HTTP/") == 1) {
        delete vals
        n = 0
        in_headers = 1
        next
      }
      if (!in_headers) next
      if (line == "") {
        # End of this header block; keep vals (overwritten if another block follows).
        in_headers = 0
        next
      }
      # Skip obsolete line-folded continuations.
      c0 = substr(line, 1, 1)
      if (c0 == " " || c0 == "\t") next
      colon = index(line, ":")
      if (colon < 1) next
      key = substr(line, 1, colon - 1)
      if (tolower(key) == want) {
        val = substr(line, colon + 1)
        sub(/^[[:space:]]+/, "", val)
        sub(/[[:space:]]+$/, "", val)
        n++
        vals[n] = val
      }
    }
    END {
      for (i = 1; i <= n; i++) print vals[i]
    }
  ' "$headers_file"
}

# Number of occurrences of header name in the final block.
header_count() {
  local headers_file="$1"
  local name="$2"
  # Count lines including empty values (NR), not non-empty only.
  header_values "$headers_file" "$name" | awk 'END { print NR+0 }'
}

# Require zero occurrences of header name.
header_require_absent() {
  local label="$1"
  local headers_file="$2"
  local name="$3"
  local c
  c="$(header_count "$headers_file" "$name")"
  if [[ "$c" -ne 0 ]]; then
    printf '%s: header %s must be absent (count=%s)\n' "$label" "$name" "$c" >&2
    header_values "$headers_file" "$name" | sed 's/^/  value: /' >&2 || true
    sed 's/^/  /' "$headers_file" >&2 || true
    exit 1
  fi
}

# Require exactly one occurrence equal to expected; print value on stdout.
header_require_exact_once() {
  local label="$1"
  local headers_file="$2"
  local name="$3"
  local expected="$4"
  local c val
  c="$(header_count "$headers_file" "$name")"
  if [[ "$c" -ne 1 ]]; then
    printf '%s: header %s must occur exactly once (count=%s want 1 value %q)\n' \
      "$label" "$name" "$c" "$expected" >&2
    header_values "$headers_file" "$name" | sed 's/^/  value: /' >&2 || true
    sed 's/^/  /' "$headers_file" >&2 || true
    exit 1
  fi
  val="$(header_values "$headers_file" "$name")"
  if [[ "$val" != "$expected" ]]; then
    printf '%s: header %s=%q want %q\n' "$label" "$name" "$val" "$expected" >&2
    sed 's/^/  /' "$headers_file" >&2 || true
    exit 1
  fi
  printf '%s' "$val"
}

# True if aggregated Vary tokens (all Vary lines, comma-split) include token.
header_has_vary_token() {
  local headers_file="$1"
  local token="$2"
  local part want
  want="$(printf '%s' "$token" | tr '[:upper:]' '[:lower:]')"
  # Aggregate every Vary line from the final block, split on commas.
  while IFS= read -r part || [[ -n "${part:-}" ]]; do
    part="${part//$'\r'/}"
    part="${part#"${part%%[![:space:]]*}"}"
    part="${part%"${part##*[![:space:]]}"}"
    [[ -z "$part" ]] && continue
    if [[ "$(printf '%s' "$part" | tr '[:upper:]' '[:lower:]')" == "$want" ]]; then
      return 0
    fi
  done < <(header_values "$headers_file" "Vary" | tr ',' '\n'; printf '\n')
  return 1
}

assert_preflight_vary() {
  local label="$1"
  local headers_file="$2"
  local tok
  for tok in "${WEB_PREFLIGHT_VARY[@]}"; do
    if ! header_has_vary_token "$headers_file" "$tok"; then
      printf '%s: Vary missing %q (Vary lines:)\n' "$label" "$tok" >&2
      header_values "$headers_file" "Vary" | sed 's/^/  /' >&2 || true
      sed 's/^/  /' "$headers_file" >&2 || true
      exit 1
    fi
  done
}

# Shared preflight policy always present (grant and no-grant): no-store + full Vary.
assert_preflight_base() {
  local label="$1"
  local headers_file="$2"
  header_require_exact_once "$label" "$headers_file" "Cache-Control" "no-store" >/dev/null
  assert_preflight_vary "$label" "$headers_file"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Credentials"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Headers"
}

# Positive canary preflight grant (ordinary or with optional PNA).
# pna_expected: "true" requires exactly Access-Control-Allow-Private-Network: true;
#               "false" requires the header absent.
assert_preflight_grant() {
  local label="$1"
  local headers_file="$2"
  local pna_expected="$3"
  assert_preflight_base "$label" "$headers_file"
  header_require_exact_once "$label" "$headers_file" "Access-Control-Allow-Origin" "$WEB_CORS_ORIGIN" >/dev/null
  header_require_exact_once "$label" "$headers_file" "Access-Control-Allow-Methods" "GET, HEAD" >/dev/null
  if [[ "$pna_expected" == "true" ]]; then
    header_require_exact_once "$label" "$headers_file" "Access-Control-Allow-Private-Network" "true" >/dev/null
  else
    header_require_absent "$label" "$headers_file" "Access-Control-Allow-Private-Network"
  fi
}

# Negative canary preflight: base policy, no grant headers at all.
assert_preflight_no_grant() {
  local label="$1"
  local headers_file="$2"
  assert_preflight_base "$label" "$headers_file"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Origin"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Methods"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Private-Network"
}

# Simple GET/HEAD canary CORS grant.
assert_canary_simple_cors_grant() {
  local label="$1"
  local headers_file="$2"
  header_require_exact_once "$label" "$headers_file" "Access-Control-Allow-Origin" "$WEB_CORS_ORIGIN" >/dev/null
  if ! header_has_vary_token "$headers_file" "Origin"; then
    printf '%s: Vary missing Origin (Vary lines:)\n' "$label" >&2
    header_values "$headers_file" "Vary" | sed 's/^/  /' >&2 || true
    exit 1
  fi
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Credentials"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Headers"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Methods"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Private-Network"
}

# No CORS grant headers (simple or preflight no-grant extras checked by caller).
assert_no_cors_grant() {
  local label="$1"
  local headers_file="$2"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Origin"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Credentials"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Headers"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Methods"
  header_require_absent "$label" "$headers_file" "Access-Control-Allow-Private-Network"
}

# Fixture-driven parser self-test (no network). Invoked via SMOKE_HEADER_SELFTEST=1.
smoke_header_selftest() {
  local f

  # Final block wins over interim 100 Continue + redirect chain (CRLF-realistic
  # curl -D dump). Poison headers in 100/301 must not leak into final parse.
  f="$tmpdir/hdr-redirect.txt"
  {
    printf 'HTTP/1.1 100 Continue\r\n'
    printf 'Access-Control-Allow-Origin: https://continue.evil.example\r\n'
    printf 'Vary: Cookie\r\n'
    printf 'X-Interim-Continue: 1\r\n'
    printf '\r\n'
    printf 'HTTP/1.1 301 Moved Permanently\r\n'
    printf 'Access-Control-Allow-Origin: https://evil.example\r\n'
    printf 'Vary: Accept-Encoding\r\n'
    printf 'Location: https://example.invalid/final\r\n'
    printf '\r\n'
    printf 'HTTP/1.1 200 OK\r\n'
    printf 'Access-Control-Allow-Origin: https://app.emby.media\r\n'
    printf 'Vary: Origin\r\n'
    printf 'Cache-Control: no-store\r\n'
    printf '\r\n'
  } >"$f"
  header_require_exact_once 'selftest redirect final' "$f" 'Access-Control-Allow-Origin' 'https://app.emby.media' >/dev/null
  header_require_exact_once 'selftest redirect cache' "$f" 'Cache-Control' 'no-store' >/dev/null
  if ! header_has_vary_token "$f" 'Origin'; then
    printf 'selftest: final Vary Origin missing\n' >&2
    exit 1
  fi
  if header_has_vary_token "$f" 'Accept-Encoding'; then
    printf 'selftest: interim 301 Vary must not leak into final block\n' >&2
    exit 1
  fi
  if header_has_vary_token "$f" 'Cookie'; then
    printf 'selftest: interim 100 Continue Vary must not leak into final block\n' >&2
    exit 1
  fi
  header_require_absent 'selftest no 100 continue leak' "$f" 'X-Interim-Continue'
  header_require_absent 'selftest no 301 location leak' "$f" 'Location'
  # Count must be exactly one ACAO (final only), not 100+301+200.
  c="$(header_count "$f" 'Access-Control-Allow-Origin')"
  if [[ "$c" -ne 1 ]]; then
    printf 'selftest: ACAO count=%s want 1 (final block only)\n' "$c" >&2
    exit 1
  fi
  printf 'ok: header selftest final block over 100 Continue + redirect\n'

  # Duplicate singleton in final block must fail exact-once (subshell: helpers exit 1).
  f="$tmpdir/hdr-dup.txt"
  printf '%s\n' \
    'HTTP/1.1 200 OK' \
    'Access-Control-Allow-Origin: https://app.emby.media' \
    'Access-Control-Allow-Origin: https://evil.example' \
    '' >"$f"
  if (header_require_exact_once 'selftest dup' "$f" 'Access-Control-Allow-Origin' 'https://app.emby.media' >/dev/null 2>"$tmpdir/dup.err"); then
    printf 'selftest: duplicate ACAO should fail exact-once\n' >&2
    exit 1
  fi
  printf 'ok: header selftest rejects duplicate singleton\n'

  # Empty first + later grant: first-match would see empty; count must be 2 and absent fails.
  f="$tmpdir/hdr-empty-first.txt"
  printf '%s\n' \
    'HTTP/1.1 200 OK' \
    'Access-Control-Allow-Origin: ' \
    'Access-Control-Allow-Origin: https://evil.example' \
    '' >"$f"
  c="$(header_count "$f" 'Access-Control-Allow-Origin')"
  if [[ "$c" -ne 2 ]]; then
    printf 'selftest: empty-first count=%s want 2\n' "$c" >&2
    exit 1
  fi
  if (header_require_absent 'selftest empty-first absent' "$f" 'Access-Control-Allow-Origin' 2>/dev/null); then
    printf 'selftest: empty-first must not report absent\n' >&2
    exit 1
  fi
  printf 'ok: header selftest empty-first still counts later grant\n'

  # Multi Vary lines aggregate tokens; CRLF endings.
  f="$tmpdir/hdr-vary-crlf.txt"
  printf 'HTTP/1.1 204 No Content\r\n' >"$f"
  printf 'Vary: Origin\r\n' >>"$f"
  printf 'Vary: Access-Control-Request-Method, Access-Control-Request-Headers\r\n' >>"$f"
  printf 'Cache-Control: no-store\r\n' >>"$f"
  printf '\r\n' >>"$f"
  for tok in Origin Access-Control-Request-Method Access-Control-Request-Headers; do
    if ! header_has_vary_token "$f" "$tok"; then
      printf 'selftest: CRLF multi-Vary missing %s\n' "$tok" >&2
      exit 1
    fi
  done
  if header_has_vary_token "$f" 'Access-Control-Request-Private-Network'; then
    printf 'selftest: unexpected PNA vary token\n' >&2
    exit 1
  fi
  header_require_exact_once 'selftest crlf cache' "$f" 'Cache-Control' 'no-store' >/dev/null
  printf 'ok: header selftest multi-Vary CRLF aggregation\n'

  # Case-insensitive header names.
  f="$tmpdir/hdr-case.txt"
  printf '%s\n' \
    'HTTP/1.1 200 OK' \
    'access-control-allow-origin: https://app.emby.media' \
    'CACHE-CONTROL: no-store' \
    '' >"$f"
  header_require_exact_once 'selftest case acao' "$f" 'Access-Control-Allow-Origin' 'https://app.emby.media' >/dev/null
  header_require_exact_once 'selftest case cache' "$f" 'Cache-Control' 'no-store' >/dev/null
  printf 'ok: header selftest case-insensitive names\n'

  printf 'smoke header selftest passed\n'
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

smoke_web_disabled() {
  local body code path
  # Canonical disabled behavior: always 404 (no redirect, no API fall-through).
  for path in "$WEB_PREFIX" "$WEB_PREFIX/" "$WEB_PREFIX/manifest.json"; do
    body="$tmpdir/web-disabled-body.txt"
    code="$(request GET "$GATEWAY_URL$path" "$body")"
    expect_status "web disabled GET $path" 404 "$code" "$body"
  done
}

smoke_web_ready() {
  local body headers code path rel
  local canary_paths=("$WEB_PREFIX/" "$WEB_PREFIX/manifest.json" "$WEB_PREFIX/index.html" "$WEB_PREFIX/strings/en-US.json")

  for path in "${canary_paths[@]}"; do
    body="$tmpdir/web-ready-body.bin"
    headers="$tmpdir/web-ready-headers.txt"
    code="$(request_with_headers GET "$GATEWAY_URL$path" "$body" "$headers" \
      -H "Origin: $WEB_CORS_ORIGIN")"
    expect_2xx "web ready GET $path" "$code" "$body"
    assert_canary_simple_cors_grant "web ready simple CORS $path" "$headers"
    printf 'ok: web ready CORS simple grant %s\n' "$path"
  done

  # Disallowed origin: still 2xx body, but no CORS grant; still Vary: Origin.
  body="$tmpdir/web-ready-bad-origin.bin"
  headers="$tmpdir/web-ready-bad-origin-headers.txt"
  code="$(request_with_headers GET "$GATEWAY_URL$WEB_PREFIX/manifest.json" "$body" "$headers" \
    -H 'Origin: https://evil.example')"
  expect_2xx 'web ready GET canary disallowed origin' "$code" "$body"
  assert_no_cors_grant 'web ready disallowed origin' "$headers"
  if ! header_has_vary_token "$headers" "Origin"; then
    printf 'web ready CORS failed: disallowed origin response must Vary: Origin\n' >&2
    exit 1
  fi
  printf 'ok: web ready CORS denies disallowed origin\n'

  # Missing Origin: 2xx, no grant, still Vary: Origin.
  body="$tmpdir/web-ready-no-origin.bin"
  headers="$tmpdir/web-ready-no-origin-headers.txt"
  code="$(request_with_headers GET "$GATEWAY_URL$WEB_PREFIX/manifest.json" "$body" "$headers")"
  expect_2xx 'web ready GET canary missing origin' "$code" "$body"
  assert_no_cors_grant 'web ready missing origin' "$headers"
  if ! header_has_vary_token "$headers" "Origin"; then
    printf 'web ready CORS failed: missing origin response must Vary: Origin\n' >&2
    exit 1
  fi
  printf 'ok: web ready CORS denies missing origin\n'

  # Optional non-canary: 2xx without CORS even with allowed Origin.
  if [[ -n "$SMOKE_WEB_NON_CANARY_PATH" ]]; then
    path="$WEB_PREFIX/${SMOKE_WEB_NON_CANARY_PATH#/}"
    body="$tmpdir/web-ready-non-canary.bin"
    headers="$tmpdir/web-ready-non-canary-headers.txt"
    code="$(request_with_headers GET "$GATEWAY_URL$path" "$body" "$headers" \
      -H "Origin: $WEB_CORS_ORIGIN")"
    expect_2xx "web ready GET non-canary $path" "$code" "$body"
    assert_no_cors_grant "web ready non-canary $path" "$headers"
    printf 'ok: web ready non-canary has no CORS %s\n' "$path"
  fi

  # Canary preflight: exact grant for app.emby.media + GET, empty request headers.
  for rel in "${WEB_CANARIES[@]}"; do
    path="$WEB_PREFIX/$rel"
    body="$tmpdir/web-ready-options.txt"
    headers="$tmpdir/web-ready-options-headers.txt"
    code="$(request_with_headers OPTIONS "$GATEWAY_URL$path" "$body" "$headers" \
      -H "Origin: $WEB_CORS_ORIGIN" \
      -H 'Access-Control-Request-Method: GET')"
    expect_status "web ready OPTIONS $path" 204 "$code" "$body"
    assert_preflight_grant "web ready OPTIONS $path" "$headers" "false"
    printf 'ok: web ready OPTIONS CORS %s\n' "$path"
  done

  # Requested custom headers => 204 with full preflight base but no grant.
  body="$tmpdir/web-ready-options-hdrs.txt"
  headers="$tmpdir/web-ready-options-hdrs-headers.txt"
  code="$(request_with_headers OPTIONS "$GATEWAY_URL$WEB_PREFIX/manifest.json" "$body" "$headers" \
    -H "Origin: $WEB_CORS_ORIGIN" \
    -H 'Access-Control-Request-Method: GET' \
    -H 'Access-Control-Request-Headers: X-Custom')"
  expect_status 'web ready OPTIONS requested headers rejected' 204 "$code" "$body"
  assert_preflight_no_grant 'web ready OPTIONS requested headers' "$headers"
  printf 'ok: web ready OPTIONS rejects requested headers\n'

  # PNA positive: same grant policy + exactly Allow-Private-Network: true.
  body="$tmpdir/web-ready-options-pna.txt"
  headers="$tmpdir/web-ready-options-pna-headers.txt"
  code="$(request_with_headers OPTIONS "$GATEWAY_URL$WEB_PREFIX/index.html" "$body" "$headers" \
    -H "Origin: $WEB_CORS_ORIGIN" \
    -H 'Access-Control-Request-Method: GET' \
    -H 'Access-Control-Request-Private-Network: true')"
  expect_status 'web ready OPTIONS PNA allowed origin' 204 "$code" "$body"
  assert_preflight_grant 'web ready OPTIONS PNA allowed' "$headers" "true"
  printf 'ok: web ready OPTIONS PNA grant for allowed origin\n'

  # PNA negative: no grant headers (including methods/PNA), still base no-store+Vary.
  body="$tmpdir/web-ready-options-pna-bad.txt"
  headers="$tmpdir/web-ready-options-pna-bad-headers.txt"
  code="$(request_with_headers OPTIONS "$GATEWAY_URL$WEB_PREFIX/index.html" "$body" "$headers" \
    -H 'Origin: https://evil.example' \
    -H 'Access-Control-Request-Method: GET' \
    -H 'Access-Control-Request-Private-Network: true')"
  expect_status 'web ready OPTIONS PNA disallowed origin' 204 "$code" "$body"
  assert_preflight_no_grant 'web ready OPTIONS PNA disallowed origin' "$headers"
  printf 'ok: web ready OPTIONS PNA denied for disallowed origin\n'
}

if [[ "$SMOKE_HEADER_SELFTEST" == "1" ]]; then
  smoke_header_selftest
  exit 0
fi

if [[ "$SMOKE_WEB_ONLY" == "1" ]]; then
  if [[ "$SMOKE_WEB" == "disabled" ]]; then
    smoke_web_disabled
  else
    smoke_web_ready
  fi
  printf 'smoke passed\n'
  exit 0
fi

body="$tmpdir/public-info.json"
code="$(request GET "$GATEWAY_URL$GATEWAY_BASE_PATH/System/Info/Public" "$body")"
expect_2xx 'anonymous gateway System/Info/Public' "$code" "$body"

for collection in users emby_servers backend_accounts user_mappings gateway_sessions audit_logs; do
  body="$tmpdir/pb-$collection.json"
  code="$(request GET "$PB_URL/api/collections/$collection/records" "$body")"
  expect_not_2xx "anonymous PB $collection records" "$code" "$body"
done

if [[ "$SMOKE_WEB" == "disabled" ]]; then
  smoke_web_disabled
elif [[ "$SMOKE_WEB" == "ready" ]]; then
  smoke_web_ready
fi

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
