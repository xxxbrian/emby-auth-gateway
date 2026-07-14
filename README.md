# Emby Auth Gateway

Emby Auth Gateway is a PocketBase-backed reverse proxy for Emby clients. Clients sign in with gateway credentials, while the gateway signs in to a controlled real Emby backend account, stores the backend session, and rewrites backend user ids, tokens, server ids, and URLs before returning responses to clients.

## Architecture

- `cmd/gateway` embeds PocketBase and registers Emby-compatible gateway routes under the configured `GATEWAY_BASE_PATH`, which defaults to `/emby`.
- PocketBase stores gateway users, backend Emby servers, backend accounts, gateway-to-backend mappings, sessions, and audit logs in `pb_data`.
- The gateway exposes Emby-compatible endpoints for clients and proxies authenticated requests to the real Emby server.
- The real Emby server remains private to the gateway network in the recommended deployment shape.

PocketBase superusers are administrators. They can use the PocketBase Admin UI and API to manage collections and operational data. Records in `users` are not administrators; they are ordinary Emby client users that can authenticate through the gateway only.

## Configuration

Gateway environment variables:

| Name | Required | Default | Notes |
| --- | --- | --- | --- |
| `GATEWAY_PUBLIC_URL` | No, but set it in production | Request host/proxy headers | Externally reachable gateway Emby base URL, including `GATEWAY_BASE_PATH`, for example `https://media.example.com/emby`. Without it, URL rewriting follows the inbound request host, which can produce unusable `127.0.0.1` URLs behind some proxies. |
| `GATEWAY_BASE_PATH` | No | `/emby` | Path where Emby-compatible gateway routes are served. When Emby Web is enabled, this must be `/emby`. |
| `GATEWAY_SERVER_ID` | No | `emby-auth-gateway` | Synthetic server id returned to clients instead of the backend Emby server id. |
| `GATEWAY_WEB_ASSETS_DIR` | No | unset (Web disabled) | Absolute or relative path to the Web assets root. Blank/unset disables Web: `/emby/web` returns 404 and never falls through to the authenticated API. When set, `GATEWAY_BASE_PATH` must be `/emby` or the process fails at startup. |

PocketBase runtime flags you will commonly use:

| Flag | Notes |
| --- | --- |
| `--http` | Listen address for `serve`, for example `0.0.0.0:8090` in containers. |
| `--dir` | PocketBase data directory. The default is `pb_data` under the working directory. |
| `--encryptionEnv` | Optional PocketBase app settings encryption environment variable. This is separate from gateway backend account storage. |

Backend client identity defaults written by `setup` into `emby_servers` records:

| Field | Default |
| --- | --- |
| `backend_user_agent` | `SenPlayer/6.1.3` |
| `backend_authorization_client` | `SenPlayer` |
| `backend_authorization_device` | `Mac` |
| `backend_authorization_device_id` | Generated once by `setup` and saved as a UUID |
| `backend_authorization_version` | `6.1.3` |

## Local Compose

Copy `.env.example` to your own local `.env`. The base compose file starts only the gateway; add `docker-compose.dev.yml` when you want the local Emby container.

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build
```

Open Emby at `http://localhost:8096` and complete its first-run setup. Create the real backend Emby user that the gateway should control, then configure the gateway records with the setup command below.

## Administrator Setup

Build the gateway first when running outside Compose:

```sh
go build -o ./bin/gateway ./cmd/gateway
```

Create a PocketBase superuser for the gateway instance:

```sh
./bin/gateway superuser create admin@example.com 'replace-with-a-strong-password'
```

For Docker Compose:

```sh
docker compose run --rm gateway superuser create admin@example.com 'replace-with-a-strong-password'
```

The PocketBase Admin UI is available at `http://localhost:8090/_/` when the gateway is running.

## Gateway Account Setup

Use `setup` to create or update the Emby server record, controlled backend account, gateway user, and mapping in one command.

Local binary example:

```sh
./bin/gateway setup \
  --gateway-username alice \
  --gateway-password 'gateway-client-password' \
  --synthetic-user-id gateway-alice-001 \
  --emby-server-name local-emby \
  --emby-url http://localhost:8096/emby \
  --backend-account-name alice-backend \
  --backend-username real-emby-user \
  --backend-password 'real-emby-password'
```

The backend client identity flags are optional because they default to the SenPlayer values above. Override them when a backend node requires different headers:

```sh
./bin/gateway setup \
  --gateway-username alice \
  --gateway-password 'gateway-client-password' \
  --synthetic-user-id gateway-alice-001 \
  --emby-url https://media.example.com \
  --backend-username real-emby-user \
  --backend-password 'real-emby-password' \
  --backend-user-agent 'SenPlayer/6.1.3' \
  --backend-authorization-client SenPlayer \
  --backend-authorization-device Mac \
  --backend-authorization-version 6.1.3
```

`setup` generates `backend_authorization_device_id` once per `emby_servers` record and preserves it on later updates. Administrators can still view or edit the saved value in the PocketBase Admin UI.

Docker Compose example:

```sh
docker compose run --rm gateway setup \
  --gateway-username alice \
  --gateway-password 'gateway-client-password' \
  --synthetic-user-id gateway-alice-001 \
  --emby-server-name compose-emby \
  --emby-url http://emby:8096/emby \
  --backend-account-name alice-backend \
  --backend-username real-emby-user \
  --backend-password 'real-emby-password'
```

## Start Commands

Run directly:

```sh
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH:-/emby}"
GATEWAY_BASE_PATH="$GATEWAY_BASE_PATH" \
GATEWAY_PUBLIC_URL="http://localhost:8090$GATEWAY_BASE_PATH" \
./bin/gateway serve --http=127.0.0.1:8090
```

Run with Compose:

```sh
docker compose up --build gateway
```

Run with the local Emby development container:

```sh
docker compose -f docker-compose.yml -f docker-compose.dev.yml up --build gateway emby
```

## Emby Web (optional)

Emby Web is an optional, read-only static surface at fixed paths `/emby/web` and
`/emby/web/...`. It is independent of the authenticated reverse proxy: Web routes
never fall through to API auth, and `serve` never downloads assets, mounts a
Docker socket, or installs on startup.

### Enablement

| Condition | Behavior |
| --- | --- |
| `GATEWAY_WEB_ASSETS_DIR` unset/blank | Web disabled: canonical `/emby/web/` (and descendants) return **404**. |
| Assets dir set, `GATEWAY_BASE_PATH` not `/emby` | Startup error (enabled Web requires `/emby`). |
| Assets dir set, tree missing/corrupt/untrusted | Web configured but unavailable: **503** on Web paths. |
| Assets dir set, verified trusted release | Ready: serves pinned files; `/emby/web` → **308** `/emby/web/`. |

`serve` loads one release into memory at process start. Changing the on-disk
pointer or installing a new release does not hot-reload; restart the gateway to
activate what is currently pointed at.

### Asset layout

Installer and server share one assets root (example path `/app/web_assets`):

```text
web_assets/
  current.json                          # atomic pointer (schema, release, catalog digest)
  releases/<version>-<catalog-digest>/
    install.json
    files/...                           # immutable verified tree
  staging/                              # install workspace
  install.lock
```

Releases are immutable. Activation only rewrites `current.json`. V1 does not
garbage-collect old releases.

### Install and status CLI

Commands are pure filesystem tools: no PocketBase bootstrap, no registry
override, and no arbitrary catalog files. Assets root comes from `--assets-dir`
or `GATEWAY_WEB_ASSETS_DIR`.

```sh
# Status (plain trusted identity; exit 0 only for installed/ready)
./bin/gateway web status --assets-dir /path/to/web_assets

# Full shared verifier (ready when verified:true)
./bin/gateway web status --assets-dir /path/to/web_assets --verify

# Install: trusted built-in catalog ID + exactly one prepared source
./bin/gateway web install \
  --assets-dir /path/to/web_assets \
  --catalog-id 'emby-web-4.9.5.0' \
  --from-dir /path/to/prepared/files

./bin/gateway web install \
  --assets-dir /path/to/web_assets \
  --catalog-id 'emby-web-4.9.5.0' \
  --from-archive /path/to/prepared.tar.gz

# Prepared static-tree URL (trailing slash required). Defaults: HTTPS + public IPs.
./bin/gateway web install \
  --assets-dir /path/to/web_assets \
  --catalog-id 'emby-web-4.9.5.0' \
  --from-url 'https://assets.example.com/emby-web/4.9.5.0/'

# Development-only URL relaxations (invalid unless --from-url is set)
./bin/gateway web install \
  --assets-dir /path/to/web_assets \
  --catalog-id 'emby-web-4.9.5.0' \
  --from-url 'http://127.0.0.1:8080/tree/' \
  --allow-http-url \
  --allow-private-url
```

### Compose volume and one-shot installer

Compose defines a separate named volume `gateway_web_assets` mounted at
`/app/web_assets` (read-only on the long-running `gateway` service; read-write on
the profile-gated `web` one-shot). Web serving is **opt-in**: leave
`GATEWAY_WEB_ASSETS_DIR` blank for API-only (404 on `/emby/web/`); set exactly
`GATEWAY_WEB_ASSETS_DIR=/app/web_assets` (with `GATEWAY_BASE_PATH=/emby`) after a
trusted install to enable serving from that volume. Do not mount a Docker socket;
do not bake official bytes into image layers; `serve` never installs on startup.

```sh
# Status against the shared volume (profile "web"; RW mount; no serve)
docker compose run --rm web status
docker compose run --rm web status --verify

# Install built-in catalog ID with a legally obtained prepared source (RO bind):
docker compose run --rm \
  -v /path/to/prepared:/source:ro \
  web install --catalog-id 'emby-web-4.9.5.0' --from-dir /source

docker compose run --rm \
  -v /path/to/tree.tar.gz:/source/tree.tar.gz:ro \
  web install --catalog-id 'emby-web-4.9.5.0' --from-archive /source/tree.tar.gz

docker compose run --rm web install \
  --catalog-id 'emby-web-4.9.5.0' \
  --from-url 'https://assets.example.com/emby-web/4.9.5.0/'
```

### Production catalog

Gateway releases do **not** redistribute official Emby Web bytes. Catalogs are
path/hash metadata only. The production built-in registry currently ships one
reviewed catalog:

| Field | Value |
| --- | --- |
| Catalog ID | `emby-web-4.9.5.0` |
| Version | `4.9.5.0` |
| Source image | `emby/embyserver` (linux/arm64 child digest pinned in catalog metadata) |
| Entries | 868 |

Operators must supply a **legally obtained** prepared source tree, archive, or
URL that matches the catalog. Official asset bytes are not in the repository,
release archives, CI artifacts, or gateway image layers. There is no
`--catalog-file` or digest override.

- Unknown catalog IDs still return the legal/reproduction gate error with no
  assets root, lock, or network side effects.
- Configured trees whose digest is not in the production registry load as
  corrupt/untrusted (503), never Ready.
- Provenance for the shipped metadata is recorded in
  `internal/embyweb/catalogs/PROVENANCE.md` (owner risk acceptance for metadata
  publication; not an Emby license grant).

### Upgrade, reactivation, rollback

1. Prepare a source tree/archive/URL that matches the target trusted catalog.
2. Run `web install --catalog-id <trusted-id> --from-...` against the assets root.
   - New release: stages, verifies every declared file, publishes under
     `releases/<version>-<digest>/`, then atomically updates `current.json`.
   - Identical existing release: verifies and **reactivates** the pointer
     (`reactivated: true`).
3. Confirm on disk **before** restart:
   `web status --verify` (expect `state: ready`, `verified: true`).
4. **Restart** the gateway process so `serve` pins the new `current.json` release
   (no hot reload).
5. Confirm the running process with anonymous HTTP canaries (or
   `SMOKE_WEB=ready` against the live gateway).

Rollback is the same path with a **previous trusted catalog ID** and matching
prepared source (or reactivation of an already-published release still present
under `releases/`). Only trusted catalog IDs are accepted; do not hand-edit
digests or drop unreviewed trees into the assets root.

### Browser canaries and CORS

`app.emby.media` manual server checks anonymously fetch:

- `/emby/web/manifest.json`
- `/emby/web/index.html` (also served as `/emby/web/`)
- `/emby/web/strings/en-US.json`

Only those canaries grant CORS, and only to origin `https://app.emby.media`
(simple GET/HEAD: `Access-Control-Allow-Origin` + `Vary: Origin`; OPTIONS
preflight: `204`, methods `GET, HEAD`, no credentials). Ordinary Web assets do
not grant CORS.

### Public reverse proxy

Expose the Emby surface (`/emby`, including `/emby/web` when enabled) to clients.
**Block** public access to PocketBase admin and API paths `/_/` and `/api` so
same-origin browser JS cannot reach administrator credentials. Redact sensitive
query tokens (`api_key`, `access_token`, `token`, `X-Emby-Token`) from access
logs case-insensitively.

## Releases

Release Docker images are published to GitHub Container Registry:

```sh
docker pull ghcr.io/xxxbrian/emby-auth-gateway:0.3.0
docker pull ghcr.io/xxxbrian/emby-auth-gateway:latest
```

Images are built for `linux/amd64` and `linux/arm64`. GitHub Releases also include Linux and macOS binary archives plus `checksums.txt`.

Publishing a GitHub Release automatically builds the Docker image and release binaries. The release workflow can also be run manually for an existing tag, with an option to control whether the image is tagged as `latest`.

## Verification

Anonymous public server info should be available through the gateway. The examples below use the default gateway base path:

```sh
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH:-/emby}"
curl -i "http://localhost:8090$GATEWAY_BASE_PATH/System/Info/Public"
```

PocketBase internal gateway collections should not be anonymously readable:

```sh
curl -i http://localhost:8090/api/collections/users/records
curl -i http://localhost:8090/api/collections/emby_servers/records
curl -i http://localhost:8090/api/collections/backend_accounts/records
curl -i http://localhost:8090/api/collections/user_mappings/records
curl -i http://localhost:8090/api/collections/gateway_sessions/records
curl -i http://localhost:8090/api/collections/audit_logs/records
```

Login through the gateway and use the returned gateway token:

```sh
GATEWAY_BASE_PATH="${GATEWAY_BASE_PATH:-/emby}"
TOKEN="$(curl -sS "http://localhost:8090$GATEWAY_BASE_PATH/Users/AuthenticateByName" \
  -H 'Content-Type: application/json' \
  -H 'X-Emby-Authorization: Emby Client="curl", Device="shell", DeviceId="smoke", Version="1"' \
  --data '{"Username":"alice","Pw":"gateway-client-password"}' \
  | jq -r '.AccessToken')"

curl -i "http://localhost:8090$GATEWAY_BASE_PATH/System/Info" -H "X-Emby-Token: $TOKEN"
curl -i -X POST "http://localhost:8090$GATEWAY_BASE_PATH/Sessions/Logout" -H "X-Emby-Token: $TOKEN"
curl -i "http://localhost:8090$GATEWAY_BASE_PATH/System/Info" -H "X-Emby-Token: $TOKEN"
```

The final request should return `401` after logout.

The scripted smoke test covers the same baseline:

```sh
USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh
```

Useful smoke variables:

- `GATEWAY_URL` defaults to `http://127.0.0.1:8090`.
- `PB_URL` defaults to `GATEWAY_URL`.
- `GATEWAY_BASE_PATH` defaults to `/emby`.
- `USERNAME` / `PASSWORD` (or `SMOKE_USERNAME` / `SMOKE_PASSWORD`) are required gateway credentials unless `SMOKE_WEB_ONLY=1`.
- `SMOKE_OPTIONAL_MEDIA=1` enables optional Items verification and requires `SYNTHETIC_USER_ID`.
- `SMOKE_M3U8_PATH` optionally verifies an m3u8 path when optional media checks are enabled.
- `CURL_OPTS` optional extra curl flags (word-split), for example `-k`.
- `SMOKE_WEB=disabled` checks canonical disabled Web behavior (`/emby/web/` → 404). Use when Web is not enabled.
- `SMOKE_WEB=ready` checks the three canaries and exact CORS for `https://app.emby.media` against an already-ready Web install (synthetic fixtures are fine; official bytes are not required). Default smoke leaves `SMOKE_WEB` unset and stays hermetic.
- `SMOKE_WEB_ONLY=1` (**test-only**): requires `SMOKE_WEB=disabled|ready`, skips credentials and API/login checks, runs only Web checks. Used by the hermetic Go deployment smoke (`TestSyntheticReadyDeploymentSmoke`).
- `SMOKE_WEB_NON_CANARY_PATH` (**test-only**, ready mode): relative path under `/emby/web/` that must return 2xx **without** CORS grant (proves canary-only CORS).

Examples:

```sh
# Default API smoke (no Web checks)
USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh

# Expect Web disabled (404 on /emby/web/)
SMOKE_WEB=disabled USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh

# Expect ready Web canaries + CORS (gateway already serving a verified tree)
SMOKE_WEB=ready USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh

# Test-only Web surface (no credentials; CI synthetic server)
SMOKE_WEB=ready SMOKE_WEB_ONLY=1 GATEWAY_URL=http://127.0.0.1:PORT ./scripts/smoke.sh
```

CI runs `TestSyntheticReadyDeploymentSmoke` (synthetic catalog install + local
HTTP server + `SMOKE_WEB_ONLY=1`) before the full Go suite. That path uses a
test-only synthetic catalog and never downloads or embeds official Emby Web
bytes.

**Deferred:** Firefox BiDi / real browser UI login-home-logout against official
assets remains a manual end-to-end gate when a matching prepared source is
available. Hermetic HTTP canary/CORS smoke is the automated stand-in in CI.

## Security Notes

- Keep PocketBase internal collections locked down. `users`, `emby_servers`, `backend_accounts`, `user_mappings`, `gateway_sessions`, and `audit_logs` should not be anonymously readable or writable. PocketBase superusers bypass collection rules and are the intended administrators.
- Gateway users are client identities only. Ordinary `users` records cannot access the PocketBase API and must not be used as an administrator boundary.
- Gateway tokens are stored only as SHA-256 hashes.
- Backend Emby passwords and backend Emby session tokens are stored as plaintext fields in PocketBase so administrators can configure and inspect backend records in the Admin UI. PocketBase superuser access or direct database file access is secret access.
- Do not expose the real Emby backend directly to untrusted clients when testing gateway isolation.
- On the public reverse proxy, expose `/emby` (and `/emby/web` when enabled) but block PocketBase `/_/` and `/api`.
- Do not redistribute official Emby Web dashboard bytes in this project’s releases; install only built-in trusted catalog IDs (currently `emby-web-4.9.5.0`) against a legally obtained prepared source.
- Do not commit `.env` files or real secrets. `.env.example` contains placeholders only.

## Troubleshooting

- Login returns `401`: verify the gateway username and password created by `setup`, confirm the `users` record is enabled, and confirm `user_mappings`, `backend_accounts`, and `emby_servers` records exist and are enabled.
- Login returns `502 backend authentication failed`: verify `--emby-url`, backend username, backend password, and network reachability from the gateway to Emby. In Compose, the backend URL should be `http://emby:8096/emby`.
- Proxied requests return `401`: the gateway token may be missing, expired, revoked, or sent under an unsupported header/query name. Supported inputs include `X-Emby-Token`, `X-MediaBrowser-Token`, Emby authorization headers, `api_key`, `access_token`, and `token`.
- URLs in Emby responses point at the backend: set `GATEWAY_PUBLIC_URL` to the public gateway base URL including the configured `GATEWAY_BASE_PATH`.
- The smoke script fails on PB side-door checks with `2xx`: lock down the PocketBase collection API rules before treating the deployment as safe.
- `/emby/web/` returns 404: Web is disabled (`GATEWAY_WEB_ASSETS_DIR` unset/blank). Expected for API-only deployments; in Compose leave the env blank until assets are installed and you intentionally opt in with `/app/web_assets`.
- `/emby/web/` returns 503: assets dir is set but the tree is missing, corrupt, or not in the production trusted registry; run `web status --verify` and reinstall a trusted catalog when available.
- Gateway refuses to start with Web enabled: set `GATEWAY_BASE_PATH=/emby` or clear `GATEWAY_WEB_ASSETS_DIR`.
- `web install` fails with legal/reproduction gate: the catalog ID is unknown; use a built-in ID such as `emby-web-4.9.5.0`.
- `web install` fails after resolving the catalog: the prepared source is missing, incomplete, or does not match the catalog hashes; fix the source tree/archive/URL (official bytes are never shipped by this project).
- Web install succeeded but browser still sees old assets: confirm `web status --verify` on disk, then restart the gateway so `serve` reloads `current.json`, then re-check HTTP canaries.
