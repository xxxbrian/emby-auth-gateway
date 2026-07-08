# Emby Auth Gateway

Emby Auth Gateway is a PocketBase-backed reverse proxy for Emby clients. Clients sign in with gateway credentials, while the gateway signs in to a controlled real Emby backend account, stores the backend session securely, and rewrites backend user ids, tokens, server ids, and URLs before returning responses to clients.

## Architecture

- `cmd/gateway` embeds PocketBase and registers Emby-compatible gateway routes under the configured `GATEWAY_BASE_PATH`, which defaults to `/emby`.
- PocketBase stores gateway users, backend Emby servers, backend accounts, gateway-to-backend mappings, sessions, and audit logs in `pb_data`.
- The gateway exposes Emby-compatible endpoints for clients and proxies authenticated requests to the real Emby server.
- The real Emby server remains private to the gateway network in the recommended deployment shape.

PocketBase superusers are administrators. They can use the PocketBase Admin UI and API to manage collections and operational data. Records in `users` are not administrators; they are ordinary Emby client users that can authenticate through the gateway only.

## Configuration

Gateway environment variables:

- `GATEWAY_SECRET_KEY` is required. It is used to derive the encryption key for backend passwords and backend tokens stored in PocketBase. Use a long random value and keep it stable for an existing `pb_data` directory.
- `GATEWAY_PUBLIC_URL` is recommended. Set it to the externally reachable gateway Emby base URL, for example `https://media.example.com/emby` when `GATEWAY_BASE_PATH=/emby`. Response URL rewriting uses this value.
- `GATEWAY_BASE_PATH` configures the path where Emby-compatible gateway routes are served. It defaults to `/emby`.
- `GATEWAY_SERVER_ID` defaults to `emby-auth-gateway`. Clients see this synthetic server id instead of the backend Emby server id.

PocketBase runtime flags you will commonly use:

- `--dir` selects the PocketBase data directory. The default is `pb_data` under the working directory.
- `--http` selects the listen address for `serve`, for example `0.0.0.0:8090` in containers.
- `--encryptionEnv` can point PocketBase app settings encryption at a 32-character environment variable if you use PocketBase encrypted settings.

## Local Compose

Copy `.env.example` to your own local `.env` and replace `GATEWAY_SECRET_KEY` with a generated secret. The sample compose file starts a real Emby container and builds the gateway image from this repository.

```sh
docker compose up --build
```

Open Emby at `http://localhost:8096` and complete its first-run setup. Create the real backend Emby user that the gateway should control, then configure the gateway records with the setup command below.

## Administrator Setup

Create a PocketBase superuser for the gateway instance:

```sh
GATEWAY_SECRET_KEY="$GATEWAY_SECRET_KEY" go run ./cmd/gateway superuser create admin@example.com 'replace-with-a-strong-password'
```

For Docker Compose:

```sh
docker compose run --rm gateway superuser create admin@example.com 'replace-with-a-strong-password'
```

The PocketBase Admin UI is available at `http://localhost:8090/_/` when the gateway is running.

## Gateway Account Setup

Use `setup` to create or update the Emby server record, controlled backend account, gateway user, and mapping in one command.

Local Go run example:

```sh
GATEWAY_SECRET_KEY="$GATEWAY_SECRET_KEY" go run ./cmd/gateway setup \
  --gateway-username alice \
  --gateway-password 'gateway-client-password' \
  --synthetic-user-id gateway-alice-001 \
  --emby-server-name local-emby \
  --emby-url http://localhost:8096/emby \
  --backend-account-name alice-backend \
  --backend-username real-emby-user \
  --backend-password 'real-emby-password'
```

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
GATEWAY_SECRET_KEY="$GATEWAY_SECRET_KEY" \
GATEWAY_BASE_PATH="$GATEWAY_BASE_PATH" \
GATEWAY_PUBLIC_URL="http://localhost:8090$GATEWAY_BASE_PATH" \
go run ./cmd/gateway serve --http=127.0.0.1:8090
```

Run with Compose:

```sh
docker compose up --build gateway emby
```

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
- `USERNAME` and `PASSWORD` are required gateway credentials.
- `SMOKE_OPTIONAL_MEDIA=1` enables optional Items verification and requires `SYNTHETIC_USER_ID`.
- `SMOKE_M3U8_PATH` optionally verifies an m3u8 path when optional media checks are enabled.

## Security Notes

- Keep PocketBase internal collections locked down. `users`, `emby_servers`, `backend_accounts`, `user_mappings`, `gateway_sessions`, and `audit_logs` should not be anonymously readable or writable. PocketBase superusers bypass collection rules and are the intended administrators.
- Gateway users are client identities only. Ordinary `users` records cannot access the PocketBase API and must not be used as an administrator boundary.
- Gateway tokens are stored only as SHA-256 hashes.
- Backend Emby passwords and backend Emby session tokens are encrypted at rest using `GATEWAY_SECRET_KEY`.
- Do not expose the real Emby backend directly to untrusted clients when testing gateway isolation.
- Do not commit `.env` files or real secrets. `.env.example` contains placeholders only.

## Troubleshooting

- `GATEWAY_SECRET_KEY is required`: set the environment variable for `serve`, `setup`, and any command that touches encrypted gateway data.
- Login returns `401`: verify the gateway username and password created by `setup`, confirm the `users` record is enabled, and confirm `user_mappings`, `backend_accounts`, and `emby_servers` records exist and are enabled.
- Login returns `502 backend authentication failed`: verify `--emby-url`, backend username, backend password, and network reachability from the gateway to Emby. In Compose, the backend URL should be `http://emby:8096/emby`.
- Proxied requests return `401`: the gateway token may be missing, expired, revoked, or sent under an unsupported header/query name. Supported inputs include `X-Emby-Token`, `X-MediaBrowser-Token`, Emby authorization headers, `api_key`, `access_token`, and `token`.
- URLs in Emby responses point at the backend: set `GATEWAY_PUBLIC_URL` to the public gateway base URL including the configured `GATEWAY_BASE_PATH`.
- The smoke script fails on PB side-door checks with `2xx`: lock down the PocketBase collection API rules before treating the deployment as safe.
