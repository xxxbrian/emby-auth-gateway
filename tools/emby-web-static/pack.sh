#!/usr/bin/env bash
# Pack a web root directory into emby-web-static-VERSION-YYYYMMDD.tar.gz + SHA256SUMS.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: pack.sh --version <emby_version> --src <web_root> --out <dir> [--date YYYYMMDD]

Creates:
  <out>/emby-web-static-<version>-<date>.tar.gz
  <out>/SHA256SUMS

VERSION must be an Emby Server tag only (letters, digits, dots, hyphens,
underscores); digests (sha256:...) and "latest" are not accepted.

Archive members are rooted at the web root (index.html at archive root).
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
    echo "src contains non-regular nodes (symlink/fifo/socket/device); refusing (first: $hit)" >&2
    find "$root" \( -type l -o -type p -o -type s -o -type b -o -type c \) 2>/dev/null >&2 || true
    return 1
  fi
  if ! hit="$(find_first "$root" ! -type d ! -type f)"; then
    echo "find failed while scanning for unexpected nodes under: $root; refusing" >&2
    return 1
  fi
  if [[ -n "$hit" ]]; then
    echo "src contains unexpected non-file/non-dir nodes; refusing (first: $hit)" >&2
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

VERSION=""
SRC=""
OUT_DIR=""
DATE_STAMP="${EMBY_WEB_DATE:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --src)
      SRC="${2:-}"
      shift 2
      ;;
    --out)
      OUT_DIR="${2:-}"
      shift 2
      ;;
    --date)
      DATE_STAMP="${2:-}"
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

if [[ -z "$VERSION" || -z "$SRC" || -z "$OUT_DIR" ]]; then
  usage >&2
  exit 2
fi

if [[ ! -d "$SRC" ]]; then
  echo "src is not a directory: $SRC" >&2
  exit 1
fi
if [[ ! -f "$SRC/index.html" ]]; then
  echo "src missing index.html: $SRC" >&2
  exit 1
fi

if [[ -z "$DATE_STAMP" ]]; then
  DATE_STAMP="$(date -u +%Y%m%d)"
fi

# Sanitize version for release names (Emby tag only; no digests/colons).
if [[ "$VERSION" == "latest" ]]; then
  echo "version must not be 'latest'" >&2
  exit 1
fi
if [[ "$VERSION" == *:* ]]; then
  echo "version must be an Emby Server tag only (no digests or colons): $VERSION" >&2
  exit 1
fi
if [[ ! "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "invalid version for release name: $VERSION" >&2
  exit 1
fi
if [[ ! "$DATE_STAMP" =~ ^[0-9]{8}$ ]]; then
  echo "invalid date (want YYYYMMDD): $DATE_STAMP" >&2
  exit 1
fi

NAME="emby-web-static-${VERSION}-${DATE_STAMP}"
mkdir -p "$OUT_DIR"
ARCHIVE="$(cd "$OUT_DIR" && pwd)/${NAME}.tar.gz"

if [[ -e "$ARCHIVE" ]]; then
  echo "archive already exists: $ARCHIVE" >&2
  exit 1
fi

# Defense at the archive boundary: refuse non-regular nodes and hardlinks.
refuse_unsafe_tree "$SRC"

# Portable tar: store files relative to SRC so extract is a flat web root.
tar -czf "$ARCHIVE" -C "$SRC" .

(
  cd "$OUT_DIR"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$(basename "$ARCHIVE")" >SHA256SUMS
  else
    sha256sum "$(basename "$ARCHIVE")" >SHA256SUMS
  fi
)

echo "packed $ARCHIVE"
cat "$OUT_DIR/SHA256SUMS"
