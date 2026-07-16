#!/usr/bin/env bash
# Extract Emby Web (dashboard-ui) from emby/embyserver:<version> into OUT_DIR.
#
# Steps:
# 1. docker pull IMAGE:VERSION
# 2. Optionally pin resolved image identity via --expect-digest / EMBY_WEB_EXPECT_DIGEST
# 3. docker cp /system/dashboard-ui into OUT_DIR
# 4. Refuse symlink/special/hardlink trees before any canary or module writes
# 5. Start a temporary container from the same image and fetch a small set of
#    runtime-only modules that are not present in the image tree (when missing),
#    writing via temp files into non-symlink destinations only.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: extract.sh --version <emby_version> --out <dir> [--image emby/embyserver]
                  [--platform linux/amd64] [--expect-digest <digest>]

Pulls IMAGE:VERSION (Emby Server / mbServer tag only, e.g. 4.9.5.0), copies
/system/dashboard-ui into OUT_DIR as a flat web root (index.html at top level),
then materializes known runtime-only modules from a temporary server instance.

VERSION must be an Emby image tag (letters, digits, dots, hyphens, underscores).
Digest refs (sha256:...) and the floating tag "latest" are not accepted; align
with pack.sh release naming and the publish-emby-web-static workflow.

--expect-digest (or env EMBY_WEB_EXPECT_DIGEST) pins the resolved image after
pull. Requires a full 64-hex sha256: RepoDigest (repo@sha256:<64hex>), bare
sha256:<64hex>, or bare <64hex> image id. Comparison is exact hash equality
only. When set, a mismatch or malformed pin fails closed before any copy.
EOF
}

# find_first prints the first matching path (or empty). Avoids find|grep -q
# pipelines that can SIGPIPE and leak non-zero status under pipefail.
# Fail-closed: nonzero find status is propagated (do not || true).
find_first() {
  find "$@" -print -quit
}

# refuse_unsafe_tree rejects symlinks, special nodes, and hardlinks under root.
# If find itself fails (unreadable tree, etc.), refuse rather than treating as clean.
refuse_unsafe_tree() {
  local root="$1"
  local hit

  if ! hit="$(find_first "$root" \( -type l -o -type p -o -type s -o -type b -o -type c \))"; then
    echo "find failed while scanning for unsafe nodes under: $root; refusing" >&2
    return 1
  fi
  if [[ -n "$hit" ]]; then
    echo "extract contains non-regular nodes (symlink/fifo/socket/device); refusing (first: $hit)" >&2
    find "$root" \( -type l -o -type p -o -type s -o -type b -o -type c \) 2>/dev/null >&2 || true
    return 1
  fi
  if ! hit="$(find_first "$root" ! -type d ! -type f)"; then
    echo "find failed while scanning for unexpected nodes under: $root; refusing" >&2
    return 1
  fi
  if [[ -n "$hit" ]]; then
    echo "extract contains unexpected non-file/non-dir nodes; refusing (first: $hit)" >&2
    find "$root" ! -type d ! -type f 2>/dev/null >&2 || true
    return 1
  fi
  if ! hit="$(find_first "$root" -type f -links +1)"; then
    echo "find failed while scanning for hardlinks under: $root; refusing" >&2
    return 1
  fi
  if [[ -n "$hit" ]]; then
    echo "hardlink refused: $hit" >&2
    find "$root" -type f -links +1 2>/dev/null >&2 || true
    return 1
  fi
  return 0
}

# sha256_hex extracts a full 64-char lowercase hex digest from a pin string.
# Accepts: sha256:<64hex>, <name>@sha256:<64hex>, or bare <64hex> (image id form).
# Rejects empty, short, non-hex, whitespace-only, and whitespace-altered pins.
sha256_hex() {
  local raw="$1"
  local norm

  norm="$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')"
  if [[ -z "$norm" || "$norm" =~ [[:space:]] ]]; then
    return 1
  fi
  if [[ "$norm" =~ ^sha256:([0-9a-f]{64})$ ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "$norm" =~ ^[^@]+@sha256:([0-9a-f]{64})$ ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return 0
  fi
  # Bare 64-hex image id form (no prefix).
  if [[ "$norm" =~ ^([0-9a-f]{64})$ ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

# digest_matches reports whether resolved image identity matches expect.
# Requires a full 64-hex sha256 in expect; compares exact hash equality only
# (no substring / partial / wildcard matching).
digest_matches() {
  local expect="$1"
  local resolved_digest="$2"
  local image_id="$3"
  local expect_hash resolved_hash id_hash

  expect_hash="$(sha256_hex "$expect")" || return 1

  if resolved_hash="$(sha256_hex "$resolved_digest")"; then
    if [[ "$expect_hash" == "$resolved_hash" ]]; then
      return 0
    fi
  fi
  if id_hash="$(sha256_hex "$image_id")"; then
    if [[ "$expect_hash" == "$id_hash" ]]; then
      return 0
    fi
  fi
  return 1
}

# install_runtime_module downloads to a temp file, validates, then installs
# only into a non-symlink destination under OUT_DIR.
install_runtime_module() {
  local rel="$1"
  local url="$2"
  local dest parent tmp
  dest="$OUT_DIR/$rel"
  parent="$(dirname "$dest")"

  mkdir -p "$parent"
  if [[ -L "$parent" ]]; then
    echo "runtime module parent is a symlink; refusing: $parent" >&2
    return 1
  fi
  if [[ -e "$dest" && -L "$dest" ]]; then
    echo "runtime module destination is a symlink; refusing: $dest" >&2
    return 1
  fi
  # Dest must not exist as a directory (or other non-file); only missing or regular file.
  if [[ -e "$dest" && ! -f "$dest" ]]; then
    echo "runtime module destination exists and is not a regular file; refusing: $dest" >&2
    return 1
  fi
  if [[ -d "$dest" ]]; then
    echo "runtime module destination is a directory; refusing: $dest" >&2
    return 1
  fi

  tmp="$(mktemp "${TMPDIR:-/tmp}/emby-web-module.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -f '$tmp'" RETURN

  echo "  GET /web/$rel"
  if ! curl -fsS \
    --max-time 30 \
    -H 'User-Agent: Emby-Web-Static-Extract/1.0' \
    -H 'Accept-Encoding: identity' \
    --path-as-is \
    "$url" \
    -o "$tmp"; then
    rm -f "$tmp"
    echo "runtime module download failed: $rel" >&2
    return 1
  fi

  if [[ ! -s "$tmp" ]]; then
    rm -f "$tmp"
    echo "runtime module empty: $rel" >&2
    return 1
  fi

  # Reject HTML error pages and tiny JS stubs mistaken for modules.
  local lower_head size
  lower_head="$(head -c 512 "$tmp" | tr '[:upper:]' '[:lower:]' || true)"
  size="$(wc -c <"$tmp" | tr -d ' ')"
  case "$rel" in
    *.js)
      if [[ "$lower_head" == *"<!doctype"* || "$lower_head" == *"<html"* ]]; then
        rm -f "$tmp"
        echo "runtime module looks like HTML (not JS): $rel" >&2
        return 1
      fi
      if [[ "$size" -lt 50 ]]; then
        rm -f "$tmp"
        echo "runtime JS module too small (${size} bytes): $rel" >&2
        return 1
      fi
      ;;
    *.css)
      if [[ "$lower_head" == *"<!doctype"* || "$lower_head" == *"<html"* ]]; then
        rm -f "$tmp"
        echo "runtime module looks like HTML (not CSS): $rel" >&2
        return 1
      fi
      # Small CSS is allowed (e.g. a few rules).
      ;;
  esac

  # Re-check destination before install (TOCTOU-ish guard).
  if [[ -e "$dest" && -L "$dest" ]]; then
    rm -f "$tmp"
    echo "runtime module destination is a symlink; refusing: $dest" >&2
    return 1
  fi
  if [[ -e "$dest" && ! -f "$dest" ]]; then
    rm -f "$tmp"
    echo "runtime module destination exists and is not a regular file; refusing: $dest" >&2
    return 1
  fi
  if [[ -d "$dest" ]]; then
    rm -f "$tmp"
    echo "runtime module destination is a directory; refusing: $dest" >&2
    return 1
  fi
  if [[ -L "$parent" ]]; then
    rm -f "$tmp"
    echo "runtime module parent is a symlink; refusing: $parent" >&2
    return 1
  fi

  mv -f "$tmp" "$dest"
  trap - RETURN
  return 0
}

IMAGE="${EMBY_WEB_IMAGE:-emby/embyserver}"
VERSION=""
OUT_DIR=""
PLATFORM="${EMBY_WEB_PLATFORM:-}"
EXPECT_DIGEST=""
EXPECT_DIGEST_SET=0
# Env pin: distinguish unset (no pin) from set-to-empty (invalid pin).
if [[ -n "${EMBY_WEB_EXPECT_DIGEST+x}" ]]; then
  EXPECT_DIGEST="$EMBY_WEB_EXPECT_DIGEST"
  EXPECT_DIGEST_SET=1
fi

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
    --expect-digest)
      EXPECT_DIGEST="${2:-}"
      EXPECT_DIGEST_SET=1
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

if [[ -z "$IMAGE_DIGEST" ]]; then
  echo "resolved SOURCE_DIGEST is empty; refusing" >&2
  exit 1
fi

if [[ "$EXPECT_DIGEST_SET" -eq 1 ]]; then
  if ! digest_matches "$EXPECT_DIGEST" "$IMAGE_DIGEST" "$IMAGE_ID"; then
    echo "image digest mismatch or invalid expect-digest: expected [$EXPECT_DIGEST], got digest=$IMAGE_DIGEST id=$IMAGE_ID" >&2
    exit 1
  fi
  echo "expect-digest matched: $EXPECT_DIGEST"
fi

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

# Refuse unsafe trees BEFORE canary -f checks or any module path writes.
refuse_unsafe_tree "$OUT_DIR"

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
    install_runtime_module "$rel" "${BASE}/web/${rel}"
  done

  docker rm -f "$RUN_CID" >/dev/null
  RUN_CID=""

  # Re-check after runtime module install.
  refuse_unsafe_tree "$OUT_DIR"
fi

COUNT="$(find "$OUT_DIR" -type f | wc -l | tr -d ' ')"
echo "extracted $COUNT files to $OUT_DIR"
printf '%s\n' "$VERSION" >"$OUT_DIR/VERSION"
printf '%s\n' "$REF" >"$OUT_DIR/SOURCE_IMAGE"
printf '%s\n' "$IMAGE_DIGEST" >"$OUT_DIR/SOURCE_DIGEST"
