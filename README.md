# Emby Auth Gateway

Emby Auth Gateway is a PocketBase-backed reverse proxy for Emby clients. Gateway users sign in with gateway credentials while the process uses one configured singleton upstream, shared backend credential, and one active endpoint.

## Architecture

- `cmd/gateway` embeds PocketBase and registers Emby-compatible gateway routes under the fixed path `/emby`.
- PocketBase stores the canonical gateway schema in `pb_data`: users, upstream configuration, sessions, playback state, preferences, policies, and audit data.
- The gateway exposes Emby-compatible endpoints for clients and proxies authenticated requests to the real Emby server.
- The real Emby server remains private to the gateway network in the recommended deployment shape.

PocketBase superusers are administrators. They can use the PocketBase Admin UI and API to manage collections and operational data. Records in `users` are not administrators; they are ordinary Emby client users that can authenticate through the gateway only.

## Configuration

Gateway environment variables:

| Name | Required | Default | Notes |
| --- | --- | --- | --- |
| `GATEWAY_PUBLIC_URL` | No, but set it in production | Request host/proxy headers | Externally reachable gateway Emby base URL, including the fixed `/emby` path, for example `https://media.example.com/emby`. Without it, URL rewriting follows the inbound request host, which can produce unusable `127.0.0.1` URLs behind some proxies. |
| `GATEWAY_SERVER_ID` | No | `emby-auth-gateway` | Synthetic server id returned to clients instead of the backend Emby server id. |
| `GATEWAY_WEB_ASSETS_DIR` | No | unset (Web disabled) | Absolute or relative path to the Web assets root. Blank/unset disables Web: `/emby/web` returns 404 and never falls through to the authenticated API. |

PocketBase runtime flags you will commonly use:

| Flag | Notes |
| --- | --- |
| `--http` | Listen address for `serve`, for example `0.0.0.0:8090` in containers. |
| `--dir` | PocketBase data directory. The default is `pb_data` under the working directory. |
| `--encryptionEnv` | Optional PocketBase app settings encryption environment variable. This is separate from gateway backend account storage. |

Backend client identity defaults stored with the singleton upstream:

| Field | Default |
| --- | --- |
| `backend_user_agent` | `SenPlayer/6.1.3` |
| `backend_authorization_client` | `SenPlayer` |
| `backend_authorization_device` | `Mac` |
| `backend_authorization_device_id` | Generated once by `setup` and saved as a UUID |
| `backend_authorization_version` | `6.1.3` |

Anonymous item-image origin always derives from the configured singleton upstream's active endpoint. The gateway probes only that endpoint tokenlessly, using its persisted client identity, and requires its live `ServerId` to match the singleton source. Only canonical `GET`/`HEAD /emby/Items/{id}/Images/{type}` routes, with an optional decimal image index, are anonymous. `/Users/.../Images/...`, media, metadata, and mutations remain authenticated. Explicit credentials or the resource cookie always use the authenticated path. Anonymous responses currently use `Cache-Control: no-store`; public caching is deferred.

Anonymous image validation is best-effort at startup and during metadata refresh. A missing, malformed, mismatched, or temporarily unavailable singleton endpoint does not block authenticated service; anonymous item images return `503 no-store` until a successful validation publishes the active origin.

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

## Gateway Setup

For a fresh installation, create the singleton upstream, then create gateway users. The identity flags are optional and default to the values above.

```sh
./bin/gateway setup upstream create \
  --emby-url http://localhost:8096/emby \
  --backend-username real-emby-user \
  --backend-password 'replace-with-a-strong-password'

./bin/gateway setup user \
  --gateway-username alice \
  --gateway-password 'replace-with-a-strong-password' \
  --synthetic-user-id gateway-alice-001
```

Use `--backend-user-agent`, `--backend-authorization-client`, `--backend-authorization-device`, and `--backend-authorization-version` on `setup upstream create` only when the backend requires a different client identity. The setup command generates and persists the upstream device ID.

### 0.7 Database Contract

For 0.7 releases, a fresh database initializes the canonical schema. An existing database must already match the exact supported final schema; startup validates it read-only and performs no migration or repair. Back up the complete PocketBase data directory before changing binaries.

## Start Commands

Run directly:

```sh
GATEWAY_PUBLIC_URL="http://localhost:8090/emby" \
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
never fall through to API auth, and `serve` never downloads or installs assets.

### Enablement

| Condition | Behavior |
| --- | --- |
| `GATEWAY_WEB_ASSETS_DIR` unset/blank | Web disabled: `/emby/web` returns **404**. |
| Assets dir set, missing or incomplete canaries | **503** on Web paths. |
| Assets dir set, canaries present | Ready: serves files from disk; `/emby/web` → **308** `/emby/web/`; host-root `/` → **308** `/emby/web/`. |

Gateway trusts the directory you point at. There is no catalog, installer CLI, or
per-file hash verification at runtime.

### Asset layout

`GATEWAY_WEB_ASSETS_DIR` is the web root itself:

```text
web-root/
  index.html
  manifest.json
  strings/en-US.json
  modules/...
  ...
```

Required canaries: `index.html`, `manifest.json`, `strings/en-US.json`.

### Publishing static packages

Static assets are produced by a separate workflow (not the Gateway binary release):

1. Actions → **Publish Emby Web Static**
2. Input `emby_version` = Emby Server / mbServer version (for example `4.9.5.0`)
3. Workflow pulls `emby/embyserver:<version>`, extracts `/system/dashboard-ui`, packs a tarball
4. Creates a GitHub Release named `emby-web-static-<version>-YYYYMMDD` (no `latest` label)
5. Same name already exists → workflow fails (no overwrite)

Local equivalent:

```sh
tools/emby-web-static/extract.sh --version 4.9.5.0 --out /tmp/web-root
tools/emby-web-static/pack.sh --version 4.9.5.0 --src /tmp/web-root --out /tmp/dist
```

### Deploy with Compose

```sh
# Unpack a release onto the host, then:
GATEWAY_WEB_ASSETS_HOST=/absolute/path/to/web-root docker compose -f docker-compose.yml -f docker-compose.web.yml up --build -d
```

Base `docker compose up` without the overlay remains API-only.

### Local binary

```sh
export GATEWAY_WEB_ASSETS_DIR=/path/to/web-root
export GATEWAY_PUBLIC_URL=http://localhost:8090/emby
./bin/gateway serve --http=127.0.0.1:8090
```

### Browser canaries and CORS

`app.emby.media` manual server checks anonymously fetch:

- `/emby/web/manifest.json`
- `/emby/web/index.html` (also served as `/emby/web/`)
- `/emby/web/strings/en-US.json`

Only those canaries grant CORS, and only to origin `https://app.emby.media`
(simple GET/HEAD: `Access-Control-Allow-Origin` + `Vary: Origin`; OPTIONS
preflight: `204`, methods `GET, HEAD`, no credentials). Ordinary Web assets do
not grant CORS.

Two JS modules receive serve-time host injection (`mb3admin.com` → request host);
on-disk files are never modified.

### Public reverse proxy

Expose the Emby surface (`/emby`, including `/emby/web` when enabled) to clients.
**Block** public access to PocketBase admin and API paths `/_/` and `/api` so
same-origin browser JS cannot reach administrator credentials. Redact sensitive
query tokens (`api_key`, `access_token`, `token`, `X-Emby-Token`) from access
logs case-insensitively.

## Releases

Release Docker images are published to GitHub Container Registry:

```sh
docker pull ghcr.io/xxxbrian/emby-auth-gateway:0.7.0
docker pull ghcr.io/xxxbrian/emby-auth-gateway:latest
```

Images are built for `linux/amd64` and `linux/arm64`. GitHub Releases also include Linux and macOS binary archives plus `checksums.txt`.

Publishing a GitHub Release automatically builds the Docker image and release binaries. The release workflow can also be run manually for an existing tag, with an option to control whether the image is tagged as `latest`.

## Verification

Anonymous public server info should be available through the gateway under fixed `/emby`:

```sh
curl -i "http://localhost:8090/emby/System/Info/Public"
```

PocketBase gateway collections should not be anonymously readable:

```sh
curl -i http://localhost:8090/api/collections/users/records
curl -i http://localhost:8090/api/collections/gateway_sessions/records
curl -i http://localhost:8090/api/collections/audit_logs/records
curl -i http://localhost:8090/api/collections/playback_events/records
curl -i http://localhost:8090/api/collections/user_item_data/records
curl -i http://localhost:8090/api/collections/item_child_counts/records
curl -i http://localhost:8090/api/collections/display_preferences/records
curl -i http://localhost:8090/api/collections/path_policies/records
curl -i http://localhost:8090/api/collections/upstream_sources/records
curl -i http://localhost:8090/api/collections/upstream_endpoints/records
```

Login through the gateway and use the returned gateway token:

```sh
TOKEN="$(curl -sS "http://localhost:8090/emby/Users/AuthenticateByName" \
  -H 'Content-Type: application/json' \
  -H 'X-Emby-Authorization: Emby Client="curl", Device="shell", DeviceId="smoke", Version="1"' \
  --data '{"Username":"alice","Pw":"gateway-client-password"}' \
  | jq -r '.AccessToken')"

curl -i "http://localhost:8090/emby/System/Info" -H "X-Emby-Token: $TOKEN"
curl -i -X POST "http://localhost:8090/emby/Sessions/Logout" -H "X-Emby-Token: $TOKEN"
curl -i "http://localhost:8090/emby/System/Info" -H "X-Emby-Token: $TOKEN"
```

The final request should return `401` after logout.

The scripted smoke test covers the same baseline:

```sh
USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh
```

Useful smoke variables:

- `GATEWAY_URL` defaults to `http://127.0.0.1:8090`.
- `PB_URL` defaults to `GATEWAY_URL`.
- Emby-compatible routes are always under fixed `/emby`.
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

# Expect ready Web canaries + CORS (gateway already serving a web root)
SMOKE_WEB=ready USERNAME=alice PASSWORD='gateway-client-password' ./scripts/smoke.sh

# Test-only Web surface (no credentials; CI fixture server)
SMOKE_WEB=ready SMOKE_WEB_ONLY=1 GATEWAY_URL=http://127.0.0.1:PORT ./scripts/smoke.sh
```

CI runs `TestSyntheticReadyDeploymentSmoke` (fixture web root + local HTTP
server + `SMOKE_WEB_ONLY=1`) before the full Go suite. That path never downloads
or embeds official Emby Web bytes.

**Deferred:** Firefox BiDi / real browser UI login-home-logout against official
assets remains a manual end-to-end gate when a matching web root is available.
Hermetic HTTP canary/CORS smoke is the automated stand-in in CI.

## Security Notes

- Keep the canonical PocketBase collections locked down. `users`, `gateway_sessions`, `audit_logs`, `playback_events`, `user_item_data`, `item_child_counts`, `display_preferences`, `path_policies`, `upstream_sources`, and `upstream_endpoints` should not be anonymously readable or writable. PocketBase superusers bypass collection rules and are the intended administrators.
- Gateway users are client identities only. Ordinary `users` records cannot access the PocketBase API and must not be used as an administrator boundary.
- Gateway tokens are stored only as SHA-256 hashes.
- Backend Emby passwords and backend Emby session tokens are stored as plaintext fields in PocketBase so administrators can configure and inspect backend records in the Admin UI. PocketBase superuser access or direct database file access is secret access.
- Do not expose the real Emby backend directly to untrusted clients when testing gateway isolation.
- On the public reverse proxy, expose `/emby` (and `/emby/web` when enabled) but block PocketBase `/_/` and `/api`.
- Do not commit `.env` files or real secrets. `.env.example` contains placeholders only.

## Troubleshooting

- Login returns `401`: verify the gateway username/password created with `setup user` and that the `users` record is enabled.
- Login returns `502 backend authentication failed`: verify the singleton active endpoint, its shared backend credentials, and network reachability. In Compose, the endpoint URL is commonly `http://emby:8096/emby`.
- Proxied requests return `401`: the gateway token may be missing, expired, revoked, or sent under an unsupported header/query name. Supported inputs include `X-Emby-Token`, `X-MediaBrowser-Token`, Emby authorization headers, `api_key`, `access_token`, and `token`.
- URLs in Emby responses point at the backend: set `GATEWAY_PUBLIC_URL` to the public gateway base URL including `/emby`.
- The smoke script fails on PB side-door checks with `2xx`: lock down the PocketBase collection API rules before treating the deployment as safe.
- `/emby/web/` returns 404: Web is disabled (`GATEWAY_WEB_ASSETS_DIR` unset/blank). Expected for API-only deployments.
- `/emby/web/` returns 503: assets dir is set but missing, not a directory, or missing canaries (`index.html`, `manifest.json`, `strings/en-US.json`).
