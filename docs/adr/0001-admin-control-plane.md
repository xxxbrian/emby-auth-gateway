# ADR 0001: Admin Control Plane

## Status

Accepted

## Context

The gateway needs a public superuser management surface for users, sessions, path
policies, upstream reconfiguration, and live operational metrics. The core product
remains the Go Emby auth gateway. PocketBase Admin UI is not a suitable primary
ops surface. Schema is 0.7 canonical/exact-validate with no application migrations.

## Decision

### Surface

- Mount a dedicated control plane under `/admin`.
- JSON API under `/admin/api/v1`.
- Svelte SPA embedded via `go:embed` under `/admin` (except reserved routes).
- Keep Emby routes under `/emby` and PocketBase under `/_/` + `/api`.

### Route exceptions

- `/admin/service/registration/*` remains the Emby Web compatibility subtree.
- Always reserve it: registration handler when Web is ready, deliberate 404 otherwise.
- SPA fallback must never capture that subtree.

### Auth

- Only PocketBase superusers may access admin APIs.
- Browser never persists PB superuser JWT.
- Login uses official PB superuser auth (including MFA), then immediately exchanges
  the JWT for an opaque admin session via `POST /admin/api/v1/session`.
- Server stores session id → PB JWT in memory only.
- Cookie: `__Secure-eag_admin_session`, `Secure`, `HttpOnly`, `SameSite=Strict`,
  `Path=/admin`, absolute 8h, idle 30m.
- Early path-scoped middleware injects `Authorization` before PB auth loading.
- Writes require CSRF token + same-origin checks: `Origin` must equal the
  current request origin (`scheme://Host`, with `X-Forwarded-Proto` when set),
  or when `Origin` is absent `Sec-Fetch-Site: same-origin` is required.
- Strict CSP; HTML/JSON/SSE are `Cache-Control: private, no-store`.

### Enablement

- Admin control plane is always mounted (no feature flag).
- No fixed `GATEWAY_ADMIN_ORIGIN` / `GATEWAY_PUBLIC_URL` is required to mount
  admin; CSRF is evaluated per request against the inbound Host.
- Telemetry may run independently; telemetry failure never blocks gateway start.

### Isolation

- Gateway emits only through concrete `observe.Emitter.TryEmit` (non-blocking).
- Full queue increments drop counters and never changes proxy responses.
- No global ResponseWriter wrapper for metrics.
- Admin reads use bounded SQL via `adminquery`, not `gateway.Store` expansion.
- Dependency direction: `observe` ← `gateway`/`telemetry`; composition only in `cmd/gateway`.

### Metrics

Product-focused signals only:

1. Upstream health/latency/auth
2. Shared-account capacity (active playbacks, reject rate)
3. Media transfers (distinct from playbacks)
4. Local userdata reliability
5. Auth/session anomalies
6. Runtime + SQLite pressure + telemetry drops

Active session = request activity within 5 minutes (memory last_seen).  
Active playback = progress within 90s TTL.

No schema changes for last_seen or metric series. Series live in memory only.

### Mutations

- Users: create; enable/disable; password reset. Username and synthetic_user_id
  immutable after create. Disable/password reset revoke all sessions transactionally.
- Sessions: revoke one or all for a user.
- Policies: CRUD + install-defaults + preview; pathpolicy validation; optional
  optimistic concurrency via `updated` (RFC3339) on update → 409 on mismatch.
- Upstream: reconfigure existing singleton only (no first-time create in admin).
  Requires fresh reauth ticket bound to admin session (password + MFA when
  enabled). Empty backend password reuses the stored secret for probe and
  reconfigure when a source exists. Block while active playbacks/transfers
  unless audited force. Reuse setup probe/ownership/CAS/cleanup.
- Audit: mutation success is not rolled back solely because `audit_logs` write
  fails; failures are logged. Force reconfigure returns `audit_warning` in JSON
  when audit write fails after a successful mutation.
- Never return backend password/token. Never accept token/device_id/generation in body.

### Retention

- `GATEWAY_ADMIN_AUDIT_RETENTION_DAYS` default 30.
- Cron DELETE on `audit_logs` (no new indexes in 0.7).
- Audit browsing is bounded by time window, page size, and query deadline.

### Frontend

- Svelte + Vite static SPA, committed generated assets under `internal/adminui/dist`.
- Official PB login/MFA, immediate session exchange, no token persistence.
- Pages: Overview, Users, Activity, Traffic, System (policies + upstream).

## Consequences

- Public admin increases attack surface; always-on mount requires a valid trusted origin at startup.
- Opaque sessions reset on process restart (acceptable for ops UI).
- Unindexed audit queries remain bounded; historical deep search is out of scope for 0.7.
- Upstream reconfigure can disrupt active streams; force path is explicit and audited.
