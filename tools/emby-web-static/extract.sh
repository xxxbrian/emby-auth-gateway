#!/usr/bin/env bash
# Extract Emby Web (dashboard-ui) from emby/embyserver:<version> into OUT_DIR.
#
# Steps:
# 1. docker pull IMAGE:VERSION
# 2. docker cp /system/dashboard-ui into OUT_DIR
# 3. Start a temporary container from the same image and fetch a small set of
#    runtime-only modules that are not present in the image tree (when missing).
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: extract.sh --version <emby_version> --out <dir> [--image emby/embyserver] [--platform linux/amd64]

Pulls IMAGE:VERSION (Emby Server / mbServer tag only, e.g. 4.9.5.0), copies
/system/dashboard-ui into OUT_DIR as a flat web root (index.html at top level),
then materializes known runtime-only modules from a temporary server instance.

VERSION must be an Emby image tag (letters, digits, dots, hyphens, underscores).
Digest refs (sha256:...) and the floating tag "latest" are not accepted; align
with pack.sh release naming and the publish-emby-web-static workflow.
EOF
}

IMAGE="${EMBY_WEB_IMAGE:-emby/embyserver}"
VERSION=""
OUT_DIR=""
PLATFORM="${EMBY_WEB_PLATFORM:-}"

# Paths relative to /web/ that Emby may generate at runtime and omit from the
# image dashboard-ui tree. Fetched only when missing after docker cp.
RUNTIME_MODULES=(
  "modules/apphost.js"
  "modules/input/keyboard.js"
  "modules/virtual-scroller/virtual-scroller.js"
  "modules/virtual-scroller/virtual-scroller.css"
)

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --out)
      OUT_DIR="${2:-}"
      shift 2
      ;;
    --image)
      IMAGE="${2:-}"
      shift 2
      ;;
    --platform)
      PLATFORM="${2:-}"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$VERSION" || -z "$OUT_DIR" ]]; then
  usage >&2
  exit 2
fi

if [[ "$VERSION" == "latest" ]]; then
  echo "version must not be 'latest'; pass an explicit Emby Server version" >&2
  exit 1
fi
# Reject digests and any other colon form; version is Emby tag only (pack/workflow).
if [[ "$VERSION" == *:* ]]; then
  echo "version must be an Emby Server tag only (no digests or colons): $VERSION" >&2
  exit 1
fi
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid version for image tag / release name: $VERSION" >&2
  exit 1
fi

command -v docker >/dev/null 2>&1 || {
  echo "docker is required" >&2
  exit 2
}
command -v curl >/dev/null 2>&1 || {
  echo "curl is required" >&2
  exit 2
}

REF="${IMAGE}:${VERSION}"

PULL_ARGS=(pull)
if [[ -n "$PLATFORM" ]]; then
  PULL_ARGS+=(--platform "$PLATFORM")
fi
PULL_ARGS+=("$REF")

echo "pulling $REF${PLATFORM:+ (platform $PLATFORM)}"
docker "${PULL_ARGS[@]}"

# Record the resolved image identity for release provenance (mutable tags).
IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$REF")"
IMAGE_DIGEST="$(docker image inspect --format '{{index .RepoDigests 0}}' "$REF" 2>/dev/null || true)"
if [[ -z "$IMAGE_DIGEST" || "$IMAGE_DIGEST" == "<no value>" ]]; then
  IMAGE_DIGEST="$IMAGE_ID"
fi
echo "resolved image: $IMAGE_DIGEST"

mkdir -p "$OUT_DIR"
if [[ -n "$(ls -A "$OUT_DIR" 2>/dev/null || true)" ]]; then
  echo "out dir is not empty: $OUT_DIR" >&2
  exit 1
fi

COPY_CID=""
RUN_CID=""
cleanup() {
  if [[ -n "$RUN_CID" ]]; then
    docker rm -f "$RUN_CID" >/dev/null 2>&1 || true
  fi
  if [[ -n "$COPY_CID" ]]; then
    docker rm -f "$COPY_CID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

CREATE_ARGS=(create)
if [[ -n "$PLATFORM" ]]; then
  CREATE_ARGS+=(--platform "$PLATFORM")
fi
CREATE_ARGS+=("$REF")
COPY_CID="$(docker "${CREATE_ARGS[@]}")"

echo "copying /system/dashboard-ui"
docker cp "${COPY_CID}:/system/dashboard-ui/." "$OUT_DIR/"
docker rm -f "$COPY_CID" >/dev/null
COPY_CID=""

for f in index.html manifest.json strings/en-US.json; do
  if [[ ! -f "$OUT_DIR/$f" ]]; then
    echo "extract missing canary: $f" >&2
    exit 1
  fi
done

need_runtime=0
for rel in "${RUNTIME_MODULES[@]}"; do
  if [[ ! -f "$OUT_DIR/$rel" ]]; then
    need_runtime=1
    break
  fi
done

if [[ "$need_runtime" -eq 1 ]]; then
  echo "fetching runtime-only modules from temporary server"
  RUN_ARGS=(run -d --rm)
  if [[ -n "$PLATFORM" ]]; then
    RUN_ARGS+=(--platform "$PLATFORM")
  fi
  # Publish a random host port; Emby listens on 8096 inside the container.
  RUN_ARGS+=(-p 127.0.0.1::8096 "$REF")
  RUN_CID="$(docker "${RUN_ARGS[@]}")"
  HOST_PORT="$(docker port "$RUN_CID" 8096/tcp | head -n1 | sed -E 's/.*://')"
  if [[ -z "$HOST_PORT" ]]; then
    echo "failed to resolve published port for temporary Emby" >&2
    exit 1
  fi
  BASE="http://127.0.0.1:${HOST_PORT}"

  # Wait until the web surface answers (Emby first boot can take a while).
  ready=0
  for _ in $(seq 1 90); do
    if curl -fsS --max-time 2 "${BASE}/web/index.html" >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 2
  done
  if [[ "$ready" -ne 1 ]]; then
    echo "temporary Emby did not become ready in time" >&2
    docker logs "$RUN_CID" 2>&1 | tail -50 >&2 || true
    exit 1
  fi

  for rel in "${RUNTIME_MODULES[@]}"; do
    if [[ -f "$OUT_DIR/$rel" ]]; then
      continue
    fi
    # Fixed allowlist only; never fetch arbitrary URLs.
    dest="$OUT_DIR/$rel"
    mkdir -p "$(dirname "$dest")"
    echo "  GET /web/$rel"
    curl -fsS \
      --max-time 30 \
      -H 'User-Agent: Emby-Web-Static-Extract/1.0' \
      -H 'Accept-Encoding: identity' \
      --path-as-is \
      "${BASE}/web/${rel}" \
      -o "$dest"
    if [[ ! -s "$dest" ]]; then
      echo "runtime module empty: $rel" >&2
      exit 1
    fi
    # Reject HTML error pages and tiny JS stubs mistaken for modules.
    lower_head="$(head -c 512 "$dest" | tr '[:upper:]' '[:lower:]' || true)"
    size="$(wc -c <"$dest" | tr -d ' ')"
    case "$rel" in
      *.js)
        if [[ "$lower_head" == *"<!doctype"* || "$lower_head" == *"<html"* ]]; then
          echo "runtime module looks like HTML (not JS): $rel" >&2
          exit 1
        fi
        if [[ "$size" -lt 50 ]]; then
          echo "runtime JS module too small (${size} bytes): $rel" >&2
          exit 1
        fi
        ;;
      *.css)
        if [[ "$lower_head" == *"<!doctype"* || "$lower_head" == *"<html"* ]]; then
          echo "runtime module looks like HTML (not CSS): $rel" >&2
          exit 1
        fi
        # Small CSS is allowed (e.g. a few rules).
        ;;
    esac
  done

  docker rm -f "$RUN_CID" >/dev/null
  RUN_CID=""
fi

# Refuse any non-directory, non-regular node (symlinks, FIFOs, devices, sockets).
if find "$OUT_DIR" \( -type l -o -type p -o -type s -o -type b -o -type c \) | grep -q .; then
  echo "extract contains non-regular nodes (symlink/fifo/socket/device); refusing" >&2
  find "$OUT_DIR" \( -type l -o -type p -o -type s -o -type b -o -type c \) >&2
  exit 1
fi
# Also refuse anything that is neither a directory nor a regular file.
if find "$OUT_DIR" ! -type d ! -type f | grep -q .; then
  echo "extract contains unexpected non-file/non-dir nodes; refusing" >&2
  find "$OUT_DIR" ! -type d ! -type f >&2
  exit 1
fi

COUNT="$(find "$OUT_DIR" -type f | wc -l | tr -d ' ')"
echo "extracted $COUNT files to $OUT_DIR"
printf '%s\n' "$VERSION" >"$OUT_DIR/VERSION"
printf '%s\n' "$REF" >"$OUT_DIR/SOURCE_IMAGE"
printf '%s\n' "$IMAGE_DIGEST" >"$OUT_DIR/SOURCE_DIGEST"
