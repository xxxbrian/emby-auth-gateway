#!/usr/bin/env bash
# Rehearse the destructive 0009 cutover using only throwaway archived sources and data.
set -euo pipefail

PRE_SHA="dbd0d4deb4f02cfb96c65471f1e8c60f2ac54da4"
CURRENT_REF="${CURRENT_REF:-HEAD}"

for command in bash git tar sqlite3 curl mise; do
  command -v "$command" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$command" >&2; exit 1; }
done

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || { printf 'run from a git worktree\n' >&2; exit 1; }
cd "$ROOT"
[[ -f scripts/rehearse-cutover.sh && -f tools/cutoverfake/main.go && -f tools/cutoverfake/main_test.go && -f README.md ]] || { printf 'required rehearsal files are missing\n' >&2; exit 1; }

[[ "$(git rev-parse --verify "${PRE_SHA}^{commit}")" == "$PRE_SHA" ]] || { printf 'required pre-cutover commit is unavailable: %s\n' "$PRE_SHA" >&2; exit 1; }
CURRENT_SHA="$(git rev-parse --verify "${CURRENT_REF}^{commit}")"
git show "${PRE_SHA}:internal/pbsetup/upstream_import_legacy.go" >/dev/null
if git cat-file -e "${PRE_SHA}:internal/pbmigrations/1700000009_gateway_session_singleton_cutover.go" 2>/dev/null; then
  printf 'pre-cutover ref unexpectedly contains migration 0009\n' >&2
  exit 1
fi
git cat-file -e "${CURRENT_SHA}:internal/pbmigrations/1700000009_gateway_session_singleton_cutover.go" >/dev/null

candidate_files=(scripts/rehearse-cutover.sh tools/cutoverfake/main.go tools/cutoverfake/main_test.go README.md)
for candidate_file in "${candidate_files[@]}"; do
  candidate_blob="$(git rev-parse --verify "${CURRENT_SHA}:${candidate_file}" 2>/dev/null)" || {
    printf 'candidate %s does not contain required rehearsal evidence: %s\n' "$CURRENT_SHA" "$candidate_file" >&2
    exit 1
  }
  index_blob="$(git rev-parse --verify ":${candidate_file}" 2>/dev/null)" || {
    printf 'index does not contain candidate rehearsal evidence: %s\n' "$candidate_file" >&2
    exit 1
  }
  worktree_blob="$(git hash-object "$candidate_file")"
  [[ "$index_blob" == "$candidate_blob" && "$worktree_blob" == "$candidate_blob" ]] || {
    printf 'candidate evidence differs from %s: %s\n' "$CURRENT_SHA" "$candidate_file" >&2
    exit 1
  }
done
candidate_readme="$(git show "${CURRENT_SHA}:README.md")"
[[ "$candidate_readme" == *'./scripts/rehearse-cutover.sh'* && "$candidate_readme" == *"$PRE_SHA"* ]] || {
  printf 'candidate %s README lacks the required cutover release gate\n' "$CURRENT_SHA" >&2
  exit 1
}

TMP="$(mktemp -d "${TMPDIR:-/tmp}/emby-cutover.XXXXXX")"
FAKE_PID=""
cleanup() {
  status=$?
  trap - EXIT INT TERM HUP
  if [[ -n "$FAKE_PID" ]]; then
    kill "$FAKE_PID" 2>/dev/null || true
    wait "$FAKE_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

archive_ref() {
  local ref=$1 destination=$2
  mkdir -p "$destination"
  git archive "$ref" | tar -x -C "$destination"
}

copy_data_dir() {
  local source=$1 destination=$2
  mkdir -p "$destination"
  tar -C "$source" -cf - . | tar -C "$destination" -xf -
}

sql() {
  sqlite3 -batch -bail "$DB" "$1"
}

expect() {
  local label=$1 actual=$2 wanted=$3
  [[ "$actual" == "$wanted" ]] || { printf 'assertion failed: %s (got %q, want %q)\n' "$label" "$actual" "$wanted" >&2; exit 1; }
}

one_id() {
  local table=$1 count
  count="$(sql "SELECT count(*) FROM ${table};")"
  expect "${table} count" "$count" 1
  sql "SELECT id FROM ${table};"
}

printf 'Cutover rehearsal\npre-cutover ref: %s\ncurrent ref: %s\n' "$PRE_SHA" "$CURRENT_SHA"
archive_ref "$PRE_SHA" "$TMP/pre-source"
archive_ref "$CURRENT_SHA" "$TMP/current-source"

OLD="$TMP/gateway-pre-cutover"
CURRENT="$TMP/gateway-current"
FAKE="$TMP/cutoverfake"
(cd "$TMP/pre-source" && mise exec -- go build -trimpath -ldflags='-s -w' -o "$OLD" ./cmd/gateway)
(cd "$TMP/current-source" && mise exec -- go build -trimpath -ldflags='-s -w' -o "$CURRENT" ./cmd/gateway)
(cd "$TMP/current-source" && mise exec -- go build -trimpath -ldflags='-s -w' -o "$FAKE" ./tools/cutoverfake)

READY="$TMP/fake-url"
"$FAKE" --ready-file "$READY" >"$TMP/fake.log" 2>&1 &
FAKE_PID=$!
FAKE_READY=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  if ! kill -0 "$FAKE_PID" 2>/dev/null; then
    printf 'fake upstream exited before readiness\n' >&2
    exit 1
  fi
  if [[ -s "$READY" ]]; then
    FAKE_URL="$(tr -d '\r\n' < "$READY")"
    if curl --fail --silent --show-error --max-time 2 "$FAKE_URL/System/Info/Public" >"$TMP/fake-public.json"; then
      FAKE_READY=1
      break
    fi
  fi
  sleep 1
done
[[ "$FAKE_READY" -eq 1 ]] || { printf 'fake upstream did not become ready after 10 seconds\n' >&2; exit 1; }
kill -0 "$FAKE_PID" 2>/dev/null || { printf 'fake upstream exited after readiness\n' >&2; exit 1; }

DATA="$TMP/data"
mkdir -p "$DATA"
"$OLD" --dir "$DATA" setup \
  --gateway-username rehearsal-gateway \
  --gateway-password rehearsal-gateway-password \
  --synthetic-user-id rehearsal-synthetic \
  --emby-server-name rehearsal-server \
  --emby-url "$FAKE_URL" \
  --backend-account-name rehearsal-account \
  --backend-username rehearsal-backend \
  --backend-password rehearsal-backend-password >"$TMP/legacy-setup.out" 2>&1

DB="$DATA/data.db"
[[ -f "$DB" ]] || { printf 'expected PocketBase database was not created\n' >&2; exit 1; }
SERVER_ID="$(one_id emby_servers)"
ACCOUNT_ID="$(one_id backend_accounts)"
USER_ID="$(one_id users)"
expect 'mapping count' "$(sql "SELECT count(*) FROM user_mappings WHERE gateway_user = '$USER_ID' AND backend_account = '$ACCOUNT_ID' AND enabled = 1;")" 1

# These are one representative row for each durable collection plus the active
# legacy session and account-scoped cache that 0009 must respectively revoke/remove.
sql "BEGIN IMMEDIATE;
INSERT INTO gateway_sessions (id,gateway_token_hash,gateway_user,gateway_username,synthetic_user_id,backend_account,client,device,device_id,version,remote_ip,expires_at,created,updated) VALUES ('cutoversession01','cutover-session-hash','$USER_ID','rehearsal-gateway','rehearsal-synthetic','$ACCOUNT_ID','cutover','device','device-id','1','127.0.0.1','2030-01-02 03:04:05.000Z','2026-01-01 00:00:00.000Z','2026-01-01 00:00:00.000Z');
INSERT INTO item_child_counts (id,backend_account_id,item_id,child_count,created,updated) VALUES ('cutoverchildrow1','$ACCOUNT_ID','cutover-parent',2,'2026-01-01 00:00:00.000Z','2026-01-01 00:00:00.000Z');
INSERT INTO user_item_data (id,gateway_user,synthetic_user_id,item_id,item_name,item_type,played,playback_position_ticks,play_count,is_favorite,fingerprint,last_seen_at,created,updated) VALUES ('cutoveritemdata1','$USER_ID','rehearsal-synthetic','cutover-item','Cutover Item','Episode',1,100,3,1,'cutover-fingerprint','2026-01-03 03:04:05.000Z','2026-01-01 00:00:00.000Z','2026-01-01 00:00:00.000Z');
INSERT INTO playback_events (id,gateway_user,synthetic_user_id,item_id,item_name,event,playback_position_ticks,played,occurred_at,created) VALUES ('cutoverplayback1','$USER_ID','rehearsal-synthetic','cutover-item','Cutover Item','progress',100,1,'2026-01-02 03:04:05.000Z','2026-01-02 03:04:05.000Z');
INSERT INTO display_preferences (id,gateway_user,synthetic_user_id,preference_id,client,payload_json,created,updated) VALUES ('cutoverdisplay01','$USER_ID','rehearsal-synthetic','cutover-pref','cutover','{}','2026-01-01 00:00:00.000Z','2026-01-01 00:00:00.000Z');
INSERT INTO audit_logs (id,gateway_user,synthetic_user_id,event,message,method,path,status,remote_ip,created) VALUES ('cutoverauditrow1','$USER_ID','rehearsal-synthetic','cutover-event','cutover audit','GET','/Items',200,'127.0.0.1','2026-01-01 00:00:00.000Z');
COMMIT;"
expect 'active legacy session has empty revocation' "$(sql "SELECT count(*) FROM gateway_sessions WHERE id = 'cutoversession01' AND revoked_at = '';")" 1

"$OLD" --dir "$DATA" --dev=false setup upstream import-legacy --server-record-id "$SERVER_ID" --account-record-id "$ACCOUNT_ID" --apply >"$TMP/import.out" 2>"$TMP/import.err"
IMPORT_STDOUT="$(<"$TMP/import.out")"
IMPORT_STDERR="$(<"$TMP/import.err")"
[[ -z "$IMPORT_STDERR" ]] || { printf 'legacy import wrote stderr\n' >&2; exit 1; }
[[ "$IMPORT_STDOUT" == *'"mode":"apply"'* && "$IMPORT_STDOUT" == *'"action":"create"'* ]] || { printf 'legacy import did not report applied create\n' >&2; exit 1; }
for secret in rehearsal-gateway-password rehearsal-backend-password cutoverfake-token cutover-session-hash cutoverfake-user; do
  [[ "$IMPORT_STDOUT" != *"$secret"* && "$IMPORT_STDERR" != *"$secret"* ]] || { printf 'legacy import exposed protected output\n' >&2; exit 1; }
done
SOURCE_ID="$(one_id upstream_sources)"
ENDPOINT_ID="$(one_id upstream_endpoints)"
expect 'active singleton endpoint' "$(sql "SELECT count(*) FROM upstream_endpoints WHERE id = '$ENDPOINT_ID' AND source = '$SOURCE_ID' AND key = 'primary' AND active = 1;")" 1

BACKUP="$TMP/pre-cutover-backup"
copy_data_dir "$DATA" "$BACKUP"
"$CURRENT" --dir "$DATA" migrate up >"$TMP/current-migrate.out" 2>&1

expect '0009 applied' "$(sql "SELECT count(*) FROM _migrations WHERE file LIKE '%0009%';")" 1
expect 'sessions revoked with canonical UTC timestamp' "$(sql "SELECT count(*) FROM gateway_sessions WHERE id = 'cutoversession01' AND revoked_at <> '' AND revoked_at GLOB '????-??-?? ??:??:??.???Z' AND strftime('%Y-%m-%d %H:%M:%fZ', revoked_at) = revoked_at;")" 1
expect 'child cache cleared' "$(sql "SELECT count(*) FROM item_child_counts;")" 0
expect 'retired session columns absent' "$(sql "SELECT count(*) FROM pragma_table_info('gateway_sessions') WHERE name IN ('backend_account','backend_token','backend_token_encrypted','backend_server_id','backend_base_url','backend_user_id','backend_username','backend_user_agent','backend_authorization_client','backend_authorization_device','backend_authorization_device_id','backend_authorization_version');")" 0
expect 'singleton item child index' "$(sql "SELECT count(*) FROM pragma_index_list('item_child_counts') WHERE name = 'idx_item_child_counts_item' AND [unique] = 1;")" 1
expect 'server retained' "$(sql "SELECT count(*) FROM emby_servers WHERE id = '$SERVER_ID';")" 1
expect 'account retained' "$(sql "SELECT count(*) FROM backend_accounts WHERE id = '$ACCOUNT_ID';")" 1
expect 'user retained' "$(sql "SELECT count(*) FROM users WHERE id = '$USER_ID';")" 1
expect 'source retained' "$(sql "SELECT count(*) FROM upstream_sources WHERE id = '$SOURCE_ID';")" 1
expect 'endpoint retained' "$(sql "SELECT count(*) FROM upstream_endpoints WHERE id = '$ENDPOINT_ID';")" 1
for table_and_id in 'user_item_data cutoveritemdata1' 'playback_events cutoverplayback1' 'display_preferences cutoverdisplay01' 'audit_logs cutoverauditrow1'; do
  set -- $table_and_id
  expect "$1 durable row retained" "$(sql "SELECT count(*) FROM $1 WHERE id = '$2';")" 1
done

ROLLBACK="$TMP/rollback-data"
copy_data_dir "$BACKUP" "$ROLLBACK"
"$OLD" --dir "$ROLLBACK" migrate up >"$TMP/rollback-bootstrap.out" 2>&1
DB="$ROLLBACK/data.db"
expect '0009 absent after restore' "$(sql "SELECT count(*) FROM _migrations WHERE file LIKE '%0009%';")" 0
expect 'restored session has empty revocation' "$(sql "SELECT count(*) FROM gateway_sessions WHERE id = 'cutoversession01' AND revoked_at = '';")" 1
expect 'restored child cache' "$(sql "SELECT count(*) FROM item_child_counts WHERE id = 'cutoverchildrow1' AND backend_account_id = '$ACCOUNT_ID';")" 1
expect 'restored legacy records' "$(sql "SELECT count(*) FROM emby_servers WHERE id = '$SERVER_ID';")" 1
expect 'restored singleton records' "$(sql "SELECT count(*) FROM upstream_sources WHERE id = '$SOURCE_ID';")" 1
expect 'restored durable data' "$(sql "SELECT count(*) FROM user_item_data WHERE id = 'cutoveritemdata1';")" 1

printf 'PASS cutover rehearsal pre=%s current=%s\n' "$PRE_SHA" "$CURRENT_SHA"
