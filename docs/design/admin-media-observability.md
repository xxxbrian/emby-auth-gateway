# Admin media details and observability

Implemented 2026-09-12. The Admin UI remains English. This document describes the delivered behavior and operational limits, replacing the earlier phased proposal.

## User-facing behavior

- Media references in Activity, Buffer, Transcoding and Users have a shared thumbnail/title component and a media detail drawer. Episode metadata includes series, season/episode, runtime and overview when supplied by Emby. The drawer also exposes selected media tracks and a copyable item ID.
- Media links preserve the underlying page and query. The native dialog supports keyboard focus, Escape, browser Back and reload. Loading, missing metadata, unavailable sources and broken images preserve the original record and saved title.
- Users can inspect Recently played, Continue watching and Favorites. Playback position, watched state and favorites belong to the selected Gateway user. Ticks use Emby's 10,000,000 ticks per second.
- Buffer defaults to 24h, retains history while idle, and presents a pipeline from upstream read through Gateway queue to client write. Backend health and finite reason codes determine the displayed warning. Raw lifecycle, allocation and I/O values remain in Technical data.
- History charts use timestamped samples, units, multi-series legends, explicit gaps and keyboard/touch sample inspection. Minute condition peaks preserve short warnings without replacing coherent controller snapshots. Chart coordinates resize to the container so axis labels retain readable size on phones.
- Completion history supports outcome/time filters, stable pages, direct details and explicit expiry/restart messages. Polling retains the selected page and independently refreshes an expanded active stream.
- Overview Errors links to recorded error details. The fixed 15m ratio is labeled and links to 15m; the historical chart links to its selected window. Traffic supports Errors/Audit, a collapsible filter panel, pagination and record details, including an interrupted response that originally started with HTTP 200/206.

## API contracts

All routes below use the existing superuser Admin session under `/admin/api/v1`. No PocketBase schema change or migration is required.

| Route | Behavior |
| --- | --- |
| `GET /media/items?ids=...&source_ref=...` | Batch of 1–50 IDs; independent per-item availability states |
| `GET /media/items/{id}?source_ref=...` | Whitelisted detail metadata; no upstream UserData, paths, credentials or playable URLs |
| `GET /media/items/{id}/images/{type}?source_ref=...&size=...` | Authenticated same-origin raster image; Primary/Thumb/Backdrop and small/large only |
| `GET /users/{id}/media?view=recent&limit=...&cursor=...` | Gateway-local media state; also accepts resume/favorites; default 50, maximum 100 |
| `GET /media-buffer` | Lightweight current aggregate, boot ID and timestamps |
| `GET /media-buffer/streams` | Bounded active pages; existing direct stream detail route retained |
| `GET /media-buffer/series?window=24h` | Existing 15m/1h/6h/24h windows; explicit missing samples and separate minute peaks |
| `GET /media-buffer/recent?from=...&to=...&outcome=...&cursor=...&limit=...` | Frozen-filter completion pagination; supports specific outcomes or errors; maximum 200 |
| `GET /media-buffer/recent/{completion_id}?boot_id=...` | Retained completion detail by boot-scoped sequence |
| `GET /audit?view=errors&from=...&to=...&cursor=...` | Database-side filtering before paging; optional event/error_kind/direction/user_id |
| `GET /audit/{id}` | Sanitized audit detail, including response_committed, is_error and severity |

Metadata status is `available`, `missing`, `unavailable` or `source_changed`. An available cached result can also have `stale=true`. Empty/unverified source references never trigger a lookup against an arbitrary current source.

Audit cursors use `(created,id)`, preserving records with equal timestamps. Legacy timestamp cursors remain supported. Queries retain the concurrency slot until the underlying PocketBase query completes, including after caller timeout; each query has a two-second deadline and audit windows remain at most 24h. Older retained audit dates can be inspected one window at a time.

## Media identity, caching and security

`internal/adminmedia` owns bounded DTO projection, batched lookup, deduplication and caches. `gateway.Server` implements the narrow read adapter using the existing singleton authenticator and refresh lifecycle. `cmd/gateway` composes these dependencies; telemetry does not perform HTTP or metadata lookups.

`source_ref` is a credential-free identity based on server and backend user. Token refresh does not change it. Actual media requests capture their selected source before I/O; buffered and synchronous transfers retain that identity through completion. Playback reports preserve the initially captured source; known audio conversion jobs use their original source. Unknown legacy identity remains unknown.

The control plane pins the server ID. User media reads additionally compare the existing saved fingerprint with freshly resolved or cached metadata through the canonical `gateway.MediaFingerprintMatches` helper. Incompatible reused IDs retain their saved local state and title without attaching the new resource. The read never repairs, rewrites or marks persisted state orphaned. Shared metadata never includes upstream or Gateway personal UserData.

Default resource budgets:

| Resource | Bound |
| --- | --- |
| Metadata cache | 1,000 entries, 8 MiB total |
| Pending metadata IDs | 1,000 |
| Upstream metadata concurrency | 4 |
| Metadata/image request deadline | 3 seconds |
| Successful server cache age | 10 minutes |
| Image cache | 64 entries, 16 MiB total |
| Image body | 4 MiB maximum |
| Image concurrency / admitted requests | 4 / 32 |
| Per-session monitoring / metadata / image requests | 120 / 240 / 600 per minute, independent buckets |

All image requests validate the Admin session even for cache hits. A revoked session cannot obtain cached images. The adapter constructs fixed upstream GET paths, enforces image type/size limits, rejects cross-origin redirects and sniffs passive raster types; SVG and arbitrary upstream URLs are not accepted. Browser responses remain `private, no-store`.

Frontend metadata batching only requests visible resources, limits in-flight batches, expires stale entries quickly and clears caches/selections on session loss. A generation check prevents old in-flight responses from repopulating a signed-out session.

Emby protocol references: [item query](https://dev.emby.media/reference/RestAPI/ItemsService/getUsersByUseridItems.html), [single item](https://dev.emby.media/reference/RestAPI/UserLibraryService/getUsersByUseridItemsById.html), [images](https://dev.emby.media/reference/RestAPI/ImageService/getItemsByIdImagesByType.html). Standard episode fields come from BaseItemDto rather than unsupported Fields values.

## Buffer semantics and retention

The pipeline presents Gateway queue bytes, allocated memory, the optional limit and cumulative bytes read/sent. The allowance is a ceiling; a full buffer or reduced allocation target is not automatically unhealthy. Controller allocation and eventual queue/I/O snapshots are not presented as an atomic accounting equation.

The queue bar is not the player's media timeline. The current telemetry does not provide a reliable total response length for every transfer, byte-to-time mapping for Range/HLS, or the client's buffered time ranges. No fabricated whole-video buffer percentage, buffered seconds or frontend-inferred throughput is shown.

Backend conditions remain authoritative:

| Condition | Threshold |
| --- | --- |
| Pool contention | Warning after 2 seconds |
| Consumer starvation | Warning after 2 seconds |
| Upstream read stall | Warning after 10 seconds |
| Downstream write stall | Warning after 10 seconds |
| Close/join stall | Critical after 10 seconds |
| Buffer acquisition / at target / debt | Informational unless an independent warning condition is present |

The selected wait duration describes only its associated condition. Observation completeness, missing samples and stale API data remain separate from stream health.

Completion records have a maximum age of 24 hours and a fixed capacity of 2,048. Capacity can shorten the available period during heavy traffic. Responses expose actual coverage, retained count, capacity and eviction count separately from nonblocking completion-offer drops. Completion cursors carry boot, sequence and frozen filters; stale boot returns 409, expired retained history returns 410.

Aggregate history uses 900 second points for 15m and up to 1,440 minute points for 24h. Minute peaks have their own active/severity/condition fields; the original coherent snapshot is not synthesized from independent maxima. History remains in memory and clears on restart. There is no cross-restart or guaranteed multi-day metric storage.

The controller, synchronous-disabled path, allocation/fairness decisions, copy loop and cancellation/close ownership are unchanged. The narrow observation-contract extension is documented in [ADR 0003](../adr/0003-media-buffer-observability.md).

## Error coverage

Recorded errors include failure events and error kinds even when their initial HTTP status is 200/206. Normal cancellations are excluded from the Errors view; rejections are identified separately by severity. Detail preserves error code, direction, duration, transferred bytes, upstream status and whether the response had already started.

Historical message text is length-limited and sanitized for URLs, bearer credentials and plain/JSON credential assignments. Query strings are removed from route display. Error counts and audit records are not a one-to-one ledger: some telemetry observations have no saved audit. The UI describes this scope and does not infer record/stream joins from approximate timestamps or paths.

## Validation

Final verification passed on 2026-09-12: Go 1.26.4 full `go test ./...` and
`go vet ./...`; the fresh embedded-Admin closure passed all 35 Playwright tests
(23 existing regressions and 12 new acceptance cases). Desktop/tablet/mobile
screenshots were also checked against the freshly built local Gateway with
controlled fixtures, with no page overflow, framework overlay or console errors.

Use the repository's required Go version explicitly when a machine-wide mise default differs:

```sh
env MISE_GO_VERSION=1.26.4 mise exec -- go test ./...
env MISE_GO_VERSION=1.26.4 mise exec -- go vet ./...
env MISE_GO_VERSION=1.26.4 SKIP_BROWSER_INSTALL=1 bash web/admin/scripts/run-admin-e2e.sh
```

The closure runs `npm ci`, Svelte and E2E type checks, builds committed embedded assets, builds a fresh Gateway and starts it with a temporary `--dir`. It never uses repository `pb_data`.

Backend tests cover metadata projection, partial failure, caching, source changes, image authentication/redirect/size rules, local user isolation and reused item IDs, audit filtering/timestamp pagination/redaction, completion retention/cursors, minute peaks and source provenance. Race and zero-allocation checks cover the affected observation paths. The worst-case 24h Buffer response fixture remains below the existing 2 MiB bound.

The new browser acceptance suite covers shared media details, degraded images, reload/back/focus, logout cache clearing, idle 24h history and gaps, backend health conditions, completion and active-page polling, interrupted HTTP 206 details, local user progress/favorites and mobile layout. Existing Admin and Buffer regressions remain part of the same closure.

Visual QA uses controlled fixtures at desktop 1440×1000, tablet 820×1180 and mobile 390×844. It checks meaningful content, console/framework errors, overflow, readable axes and actual interactions. No deployed Gateway or real upstream data was used for this validation.
