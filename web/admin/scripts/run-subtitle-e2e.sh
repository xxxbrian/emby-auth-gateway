#!/usr/bin/env bash
# Fresh local gateway + generated media + pinned, unmodified Emby Web assets.
# Optional EMBY_WEB_TEST_ASSETS reuses an already prepared vendor asset tree.
set -euo pipefail

SUBTITLE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SUBTITLE_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/eag-subtitle-e2e.XXXXXX")"
SUBTITLE_UPSTREAM_PID=""
SUBTITLE_GATEWAY_PID=""
cleanup() {
  for subtitle_pid in "${SUBTITLE_GATEWAY_PID}" "${SUBTITLE_UPSTREAM_PID}"; do
    if [[ -n "${subtitle_pid}" ]]; then kill "${subtitle_pid}" 2>/dev/null || true; wait "${subtitle_pid}" 2>/dev/null || true; fi
  done
  echo "Subtitle test artifacts: ${SUBTITLE_TEMP}"
}
trap cleanup EXIT

subtitle_go() {
  if command -v mise >/dev/null 2>&1; then
    mise exec "go@$(sed -n 's/^go //p' "${SUBTITLE_ROOT}/go.mod")" -- go "$@"
  else go "$@"; fi
}

cd "${SUBTITLE_ROOT}"
if [[ -n "${EMBY_WEB_TEST_ASSETS:-}" ]]; then
  SUBTITLE_WEB="${EMBY_WEB_TEST_ASSETS}"
else
  SUBTITLE_WEB="${SUBTITLE_TEMP}/web"
  curl --fail --location --retry 2 --connect-timeout 10 --max-time 120 \
    --output "${SUBTITLE_TEMP}/web.tar.gz" \
    'https://github.com/xxxbrian/emby-auth-gateway/releases/download/emby-web-static-4.9.5.0-20260716/emby-web-static-4.9.5.0-20260716.tar.gz'
  python3 - "${SUBTITLE_TEMP}/web.tar.gz" "${SUBTITLE_WEB}" <<'PY'
import hashlib, pathlib, sys, tarfile
archive, destination = map(pathlib.Path, sys.argv[1:])
expected = '449c56ae9ebb2d2617870a722218431e2d57fbd8c8cea2ecece6a66fc16a3ff5'
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise SystemExit('Emby Web archive checksum mismatch')
destination.mkdir()
with tarfile.open(archive) as package:
    if not all(entry.isfile() or entry.isdir() for entry in package.getmembers()):
        raise SystemExit('Unexpected links or special files in Web archive')
    package.extractall(destination, filter='data')
PY
fi

if [[ "${SUBTITLE_E2E_SKIP_ADMIN_BUILD:-0}" != 1 ]]; then
  (cd web/admin && npm ci && npm run check && npx tsc --noEmit -p tsconfig.e2e.json && npm run build)
fi
if [[ -n "${SUBTITLE_E2E_GATEWAY_BINARY:-}" ]]; then
  cp "${SUBTITLE_E2E_GATEWAY_BINARY}" "${SUBTITLE_TEMP}/gateway"
else
  subtitle_go build -o "${SUBTITLE_TEMP}/gateway" ./cmd/gateway
fi
subtitle_go build -o "${SUBTITLE_TEMP}/fixture" ./internal/subtitleindex/testdata/fixture
"${SUBTITLE_TEMP}/fixture" --dir "${SUBTITLE_TEMP}/media" --http 127.0.0.1:0 --file "${SUBTITLE_E2E_MEDIA_FILE:-}" >"${SUBTITLE_TEMP}/upstream.log" 2>&1 &
SUBTITLE_UPSTREAM_PID=$!
for _ in {1..120}; do
  [[ -s "${SUBTITLE_TEMP}/media/upstream-url" ]] && break
  kill -0 "${SUBTITLE_UPSTREAM_PID}" 2>/dev/null || { cat "${SUBTITLE_TEMP}/upstream.log"; exit 1; }
  sleep 0.25
done
SUBTITLE_UPSTREAM="$(cat "${SUBTITLE_TEMP}/media/upstream-url")"
export SUBTITLE_E2E_UPSTREAM="${SUBTITLE_UPSTREAM}"
SUBTITLE_PORT="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)"
export SUBTITLE_E2E_BASE="http://127.0.0.1:${SUBTITLE_PORT}"
export SUBTITLE_E2E_ARTIFACTS="${SUBTITLE_TEMP}/artifacts"
"${SUBTITLE_TEMP}/gateway" --dev=false --dir "${SUBTITLE_TEMP}/pb" setup upstream create --emby-url "${SUBTITLE_UPSTREAM}" --backend-username fixture --backend-password fixture-password
for subtitle_user in subtitle-one subtitle-two; do
  "${SUBTITLE_TEMP}/gateway" --dev=false --dir "${SUBTITLE_TEMP}/pb" setup user --gateway-username "${subtitle_user}" --gateway-password viewer-password --synthetic-user-id "gateway-${subtitle_user}"
done
"${SUBTITLE_TEMP}/gateway" --dev=false --dir "${SUBTITLE_TEMP}/pb" superuser create admin@test.local adminpass123
env GATEWAY_PUBLIC_URL="${SUBTITLE_E2E_BASE}/emby" GATEWAY_WEB_ASSETS_DIR="${SUBTITLE_WEB}" \
  GATEWAY_AUDIO_TRANSCODING_ENABLED=true GATEWAY_AUDIO_TRANSCODING_WORKERS=2 \
  GATEWAY_AUDIO_CACHE_DIR="${SUBTITLE_TEMP}/audio-cache" GATEWAY_AUDIO_CACHE_BUDGET=32MiB \
  GATEWAY_WEB_SUBTITLES_ENABLED="${SUBTITLE_E2E_FEATURE_ENABLED:-true}" GATEWAY_SUBTITLE_CACHE_DIR="${SUBTITLE_TEMP}/subtitle-cache" \
  GATEWAY_SUBTITLE_CACHE_BUDGET=32MiB GATEWAY_SUBTITLE_SOURCE_CACHE_BUDGET=32MiB \
  "${SUBTITLE_TEMP}/gateway" --dev=false --dir "${SUBTITLE_TEMP}/pb" serve --http="127.0.0.1:${SUBTITLE_PORT}" >"${SUBTITLE_TEMP}/gateway.log" 2>&1 &
SUBTITLE_GATEWAY_PID=$!
for _ in {1..60}; do
  curl --silent --fail "${SUBTITLE_E2E_BASE}/emby/System/Info/Public" >/dev/null && break
  kill -0 "${SUBTITLE_GATEWAY_PID}" 2>/dev/null || { cat "${SUBTITLE_TEMP}/gateway.log"; exit 1; }
  sleep 0.25
done
cd web/admin
if [[ "${SKIP_BROWSER_INSTALL:-0}" != 1 ]]; then npx playwright install chromium; fi
npx playwright test --config playwright.subtitle.config.js
