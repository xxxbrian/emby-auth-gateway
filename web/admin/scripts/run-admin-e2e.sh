#!/usr/bin/env bash
# Build the CURRENT admin SPA + a fresh gateway binary, then run Playwright e2e.
#
# Always:
#   1. npm ci (or npm install) in web/admin
#   2. npm run check
#   3. npm run build  (writes internal/adminui/dist)
#   4. go build a fresh gateway into a temp dir (never reuse bin/gateway)
#   5. start that binary with a temp PocketBase dir + superuser + ADMIN env
#   6. run Playwright against it
#   7. tear down
#
# Env:
#   ADMIN_E2E_PORT       Listen port when starting gateway (default: 18090)
#   ADMIN_E2E_EMAIL      Superuser email (default: admin@test.local)
#   ADMIN_E2E_PASSWORD   Superuser password (default: adminpass123)
#   SKIP_BROWSER_INSTALL Set to 1 to skip `npx playwright install chromium`
#
# This closure runner always builds SPA + a fresh gateway binary.
# Do not point it at an external base; that would risk testing a stale embed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ADMIN_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT="$(cd "${ADMIN_DIR}/../.." && pwd)"

EMAIL="${ADMIN_E2E_EMAIL:-admin@test.local}"
PASSWORD="${ADMIN_E2E_PASSWORD:-adminpass123}"
PORT="${ADMIN_E2E_PORT:-18090}"

STARTED_GATEWAY=0
GATEWAY_PID=""
TMP_PB_DIR=""
TMP_BIN_DIR=""
GATEWAY_BIN=""

cleanup() {
  if [[ "${STARTED_GATEWAY}" -eq 1 && -n "${GATEWAY_PID}" ]]; then
    kill "${GATEWAY_PID}" 2>/dev/null || true
    wait "${GATEWAY_PID}" 2>/dev/null || true
  fi
  if [[ -n "${TMP_PB_DIR}" && -d "${TMP_PB_DIR}" ]]; then
    rm -rf "${TMP_PB_DIR}"
  fi
  if [[ -n "${TMP_BIN_DIR}" && -d "${TMP_BIN_DIR}" ]]; then
    rm -rf "${TMP_BIN_DIR}"
  fi
}
trap cleanup EXIT

is_reachable() {
  local base="$1"
  curl -sf -o /dev/null --connect-timeout 2 --max-time 3 "${base}/admin/" 2>/dev/null
}

pick_free_port() {
  # Prefer requested PORT when free; otherwise bind an ephemeral port.
  python3 - <<PY
import socket
preferred = ${PORT}
s = socket.socket()
try:
    s.bind(("127.0.0.1", preferred))
    print(preferred)
except OSError:
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
finally:
    s.close()
PY
}

build_admin_spa() {
  echo "=== Building current admin SPA ==="
  cd "${ADMIN_DIR}"

  if [[ -f package-lock.json ]]; then
    echo "Running npm ci..."
    npm ci
  else
    echo "No package-lock.json; running npm install..."
    npm install
  fi

  echo "Running npm run check..."
  npm run check

  echo "Typechecking e2e specs..."
  npx tsc --noEmit -p tsconfig.e2e.json

  echo "Running npm run build (updates internal/adminui/dist)..."
  npm run build
}

build_fresh_gateway() {
  TMP_BIN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/admin-e2e-bin.XXXXXX")"
  GATEWAY_BIN="${TMP_BIN_DIR}/gateway"
  echo "=== Building fresh gateway binary at ${GATEWAY_BIN} ==="
  if command -v mise >/dev/null 2>&1; then
    (cd "${ROOT}" && mise exec -- go build -o "${GATEWAY_BIN}" ./cmd/gateway)
  else
    (cd "${ROOT}" && go build -o "${GATEWAY_BIN}" ./cmd/gateway)
  fi
}

start_gateway() {
  build_fresh_gateway
  PORT="$(pick_free_port)"
  BASE="http://127.0.0.1:${PORT}"
  TMP_PB_DIR="$(mktemp -d "${TMPDIR:-/tmp}/admin-e2e-pb.XXXXXX")"

  echo "=== Starting gateway for admin e2e ==="
  echo "  binary:  ${GATEWAY_BIN}"
  echo "  data:    ${TMP_PB_DIR}"
  echo "  base:    ${BASE}"
  echo "  email:   ${EMAIL}"

  "${GATEWAY_BIN}" --dir "${TMP_PB_DIR}" superuser create "${EMAIL}" "${PASSWORD}"

  # Admin UI is always on for this closure runner; public URL includes /emby base path.
  export GATEWAY_ADMIN_ENABLED=1
  export GATEWAY_ADMIN_ORIGIN="${BASE}"
  export GATEWAY_PUBLIC_URL="${BASE}/emby"

  # Explicit env for child process (document required pair).
  env \
    GATEWAY_ADMIN_ENABLED=1 \
    GATEWAY_ADMIN_ORIGIN="${BASE}" \
    GATEWAY_PUBLIC_URL="${BASE}/emby" \
    "${GATEWAY_BIN}" --dir "${TMP_PB_DIR}" serve --http="127.0.0.1:${PORT}" &
  GATEWAY_PID=$!
  STARTED_GATEWAY=1

  local i
  for i in $(seq 1 60); do
    if is_reachable "${BASE}"; then
      echo "Gateway ready at ${BASE}"
      return
    fi
    if ! kill -0 "${GATEWAY_PID}" 2>/dev/null; then
      echo "Gateway process exited before becoming ready" >&2
      exit 1
    fi
    sleep 0.5
  done
  echo "Timed out waiting for ${BASE}/admin/" >&2
  exit 1
}

# 1–3: always install, typecheck, and build the SPA into the embed dir.
build_admin_spa

# 4–5: always start a fresh temporary gateway with the just-built embed.
if [[ -n "${ADMIN_E2E_BASE:-}" ]]; then
  echo "ADMIN_E2E_BASE is ignored by this closure runner (would risk stale SPA)." >&2
  echo "Unset ADMIN_E2E_BASE and re-run." >&2
  exit 1
fi
start_gateway

export ADMIN_E2E_BASE="${BASE}"
export ADMIN_E2E_EMAIL="${EMAIL}"
export ADMIN_E2E_PASSWORD="${PASSWORD}"

cd "${ADMIN_DIR}"

if [[ "${SKIP_BROWSER_INSTALL:-0}" != "1" ]]; then
  npx playwright install chromium
fi

# 6: run Playwright against the (freshly built) SPA served by the gateway.
echo "=== Running Playwright admin e2e against ${ADMIN_E2E_BASE} ==="
npm run test:e2e
# 7: cleanup via trap EXIT
