#!/usr/bin/env bash
# Fresh local gateway + generated media + pinned, unmodified Emby Web assets.
# Optional EMBY_WEB_TEST_ASSETS reuses an already prepared vendor asset tree.
set -euo pipefail

AUDIO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
AUDIO_TEMP="$(mktemp -d "${TMPDIR:-/tmp}/eag-audio-e2e.XXXXXX")"
AUDIO_UPSTREAM_PID=""
AUDIO_GATEWAY_PID=""
cleanup() {
  for audio_pid in "${AUDIO_GATEWAY_PID}" "${AUDIO_UPSTREAM_PID}"; do
    if [[ -n "${audio_pid}" ]]; then kill "${audio_pid}" 2>/dev/null || true; wait "${audio_pid}" 2>/dev/null || true; fi
  done
  echo "Audio test artifacts: ${AUDIO_TEMP}"
}
trap cleanup EXIT

audio_go() {
  if command -v mise >/dev/null 2>&1; then
    mise exec "go@$(sed -n 's/^go //p' "${AUDIO_ROOT}/go.mod")" -- go "$@"
  else go "$@"; fi
}

cd "${AUDIO_ROOT}"
if [[ -n "${EMBY_WEB_TEST_ASSETS:-}" ]]; then
  AUDIO_WEB="${EMBY_WEB_TEST_ASSETS}"
else
  AUDIO_WEB="${AUDIO_TEMP}/web"
  curl --fail --location --retry 2 --connect-timeout 10 --max-time 120 \
    --output "${AUDIO_TEMP}/web.tar.gz" \
    'https://github.com/xxxbrian/emby-auth-gateway/releases/download/emby-web-static-4.9.5.0-20260716/emby-web-static-4.9.5.0-20260716.tar.gz'
  python3 - "${AUDIO_TEMP}/web.tar.gz" "${AUDIO_WEB}" <<'PY'
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

(cd web/admin && npm ci && npm run check && npx tsc --noEmit -p tsconfig.e2e.json && npm run build)
audio_go build -o "${AUDIO_TEMP}/gateway" ./cmd/gateway
audio_go build -o "${AUDIO_TEMP}/fixture" ./internal/transcode/testdata/fixture
"${AUDIO_TEMP}/fixture" --dir "${AUDIO_TEMP}/media" --http 127.0.0.1:0 >"${AUDIO_TEMP}/upstream.log" 2>&1 &
AUDIO_UPSTREAM_PID=$!
for _ in {1..120}; do
  [[ -s "${AUDIO_TEMP}/media/upstream-url" ]] && break
  kill -0 "${AUDIO_UPSTREAM_PID}" 2>/dev/null || { cat "${AUDIO_TEMP}/upstream.log"; exit 1; }
  sleep 0.25
done
AUDIO_UPSTREAM="$(cat "${AUDIO_TEMP}/media/upstream-url")"
AUDIO_PORT="$(python3 - <<'PY'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PY
)"
export AUDIO_E2E_BASE="http://127.0.0.1:${AUDIO_PORT}"
export AUDIO_E2E_ARTIFACTS="${AUDIO_TEMP}/artifacts"
"${AUDIO_TEMP}/gateway" --dev=false --dir "${AUDIO_TEMP}/pb" setup upstream create --emby-url "${AUDIO_UPSTREAM}" --backend-username fixture --backend-password fixture-password
for audio_user in audio-one audio-two audio-three; do
  "${AUDIO_TEMP}/gateway" --dev=false --dir "${AUDIO_TEMP}/pb" setup user --gateway-username "${audio_user}" --gateway-password viewer-password --synthetic-user-id "gateway-${audio_user}"
done
"${AUDIO_TEMP}/gateway" --dev=false --dir "${AUDIO_TEMP}/pb" superuser create admin@test.local adminpass123
env GATEWAY_PUBLIC_URL="${AUDIO_E2E_BASE}/emby" GATEWAY_WEB_ASSETS_DIR="${AUDIO_WEB}" \
  GATEWAY_AUDIO_TRANSCODING_ENABLED=true GATEWAY_AUDIO_TRANSCODING_WORKERS=2 \
  GATEWAY_AUDIO_CACHE_DIR="${AUDIO_TEMP}/cache" GATEWAY_AUDIO_CACHE_BUDGET=32MiB \
  "${AUDIO_TEMP}/gateway" --dev=false --dir "${AUDIO_TEMP}/pb" serve --http="127.0.0.1:${AUDIO_PORT}" >"${AUDIO_TEMP}/gateway.log" 2>&1 &
AUDIO_GATEWAY_PID=$!
for _ in {1..60}; do
  curl --silent --fail "${AUDIO_E2E_BASE}/emby/System/Info/Public" >/dev/null && break
  kill -0 "${AUDIO_GATEWAY_PID}" 2>/dev/null || { cat "${AUDIO_TEMP}/gateway.log"; exit 1; }
  sleep 0.25
done
cd web/admin
if [[ "${SKIP_BROWSER_INSTALL:-0}" != 1 ]]; then npx playwright install chromium; fi
npx playwright test --config playwright.audio.config.js
