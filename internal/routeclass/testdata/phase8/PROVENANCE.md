# Phase 8 Route Corpus Provenance

This directory holds Phase 8 route corpus artifacts:

1. **Official OpenAPI inventories** (non-authorizing evidence)
2. **Observed Web traffic** (`observed-web-stable.jsonl`)
3. **Curated supported routes** (`supported-routes.json`) — the only authorization set

Official inventory presence never admits a route. Observed traffic without a curated
decision does not admit a route. Supported routes are curated by hand and are
**not** generated from OpenAPI.

## Authoritative official sources

| Label | Repository | Git commit (full) | Source path | Content SHA-256 | Git blob |
| --- | --- | --- | --- | --- | --- |
| Stable `4.9.5.0` | [MediaBrowser/Emby.SDK](https://github.com/MediaBrowser/Emby.SDK) | `bdd0dd7c0801f6e069dff2795d80cddae6f91791` | `Resources/OpenApi/openapi_v3.json` | `aa7faf56902160845758ac4c4c7e337f3ad453de4b9ac5b8646981c86d8f597c` | `bd3ee511e906ff4ebf8837b5383fea4608ee2b25` |
| Beta `4.10.0.20` | [MediaBrowser/Emby.SDK](https://github.com/MediaBrowser/Emby.SDK) | `d8a719fdc58e552aa1019fc9b5b24a2a3111eef8` | `Resources/OpenApi/openapi_v3.json` | `399f9d2fc6a67ae013dad9b66d2cdad00ca2884f6359c4e4565f57debd119b6f` | `602b50d30cbc182f8229f12a89150e047090caae` |

Authoritative raw URLs:

- Stable: `https://raw.githubusercontent.com/MediaBrowser/Emby.SDK/bdd0dd7c0801f6e069dff2795d80cddae6f91791/Resources/OpenApi/openapi_v3.json`
- Beta: `https://raw.githubusercontent.com/MediaBrowser/Emby.SDK/d8a719fdc58e552aa1019fc9b5b24a2a3111eef8/Resources/OpenApi/openapi_v3.json`

Browseable blob URLs:

- Stable: `https://github.com/MediaBrowser/Emby.SDK/blob/bdd0dd7c0801f6e069dff2795d80cddae6f91791/Resources/OpenApi/openapi_v3.json`
- Beta: `https://github.com/MediaBrowser/Emby.SDK/blob/d8a719fdc58e552aa1019fc9b5b24a2a3111eef8/Resources/OpenApi/openapi_v3.json`

OpenAPI document metadata at those pins:

- Both documents declare OpenAPI `3.0.1`.
- Stable `info.version` is `4.9.5.0` → **422 paths / 535 operations**.
- Beta `info.version` is `4.10.0.20` → **428 paths / 543 operations**.

Raw multi-megabyte OpenAPI documents are **not** retained in this repository.
Inventory JSON is generated offline after verifying the content SHA-256 of the
fetched OpenAPI snapshot. Runtime code must not parse OpenAPI.

## Maintained-client corroboration pin (non-authorizing)

- Repository: [MediaBrowser/plugin.video.emby](https://github.com/MediaBrowser/plugin.video.emby)
- Commit: `4051767ec4fde5075b07cac7cdfab7e1061d9d89` (dated 2026-07-19)
- Role: corroborates that modern maintained clients still use PlaybackInfo,
  NextUp, lifecycle check-ins, capabilities, WebSocket, direct/transcode/subtitle,
  personal-state, and remote-control route families.
- **Not** treated as route authorization, observed Emby Web evidence, or a
  substitute for curated `supported-routes`.

## Corpus separation

| Kind | Artifact role | Authorizes production routes? |
| --- | --- | --- |
| `official` | Exhaustive method/path inventory from pinned SDK OpenAPI | **No** |
| `observed` | Normalized Web/client capture (`observed-web-stable.jsonl`) | Evidence only; not automatic admission |
| `curated` | `supported-routes.json` authorization set | **Yes** (only this set) |
| `maintained-client` | Cross-check pin such as plugin.video.emby | **No** |

Rules:

1. Official inventory presence never admits a route.
2. Observed traffic without a curated decision does not admit a route.
3. Maintained-client usage without official required-workflow evidence and
   controlled integration/live evidence does not admit a route.
4. Official-only, beta-only, and projection-only routes remain denied until a
   separate curated decision.
5. Administration, plugin, device, and package families remain denied regardless
   of OpenAPI presence unless a later curated exception is explicit.
6. Supported routes are curated and **not** generated from OpenAPI.

## Observed Web capture (stable 4.9.5.0)

### Capture environment

| Field | Value |
| --- | --- |
| Capture date | 2026-07-20 |
| Browser | Playwright Chrome for Testing `151.0.7922.10` (Chromium bundle `1232`) |
| Gateway build | Phase 7 binary lineage at commit `0138971` plus subsequent query-policy work on the personal-domain-architecture worktree; disposable local serve at `http://127.0.0.1:18091/emby` |
| Web assets | Emby Web **4.9.5.0** via `GATEWAY_WEB_ASSETS_DIR` |
| Web image digest (linux/arm64 child) | `sha256:1e76e14a9c99507eb9f54361126f22c4658fc1588b2a710a99ba42f2335ff59a` |
| Catalog generator digest | `e87b73c3dcfb138d9db1a9ccfb8ea9824514c9fd311615d1df9d25c690bdb87e` |
| Evidence inputs (approved temp) | `.../T/opencode/phase8-current-evidence-fresh2/observed-home.tsv`, `observed-search-detail.tsv`, `observed-detail-image.tsv` |
| Disposable gateway user | `phase8-evidence-user` (never treated as a real account name in templates) |

### Scenarios covered

| Scenario | Source TSV | Workflow |
| --- | --- | --- |
| `login-home` | `observed-home.tsv` | Public bootstrap, login, capabilities, display preferences, system info, views, library items, resume, latest sections |
| `search-detail` | `observed-search-detail.tsv` | Same bootstrap/home prefix, then search (`SearchTerm` on user Items), item detail, user, endpoint, PlaybackInfo, special features, similar, theme media |
| detail image | `observed-detail-image.tsv` (empty) | Direct tokenless binary image GET returned HTTP 200 in browser; not written to the redacted TSV. Covered by existing resource-cookie / anonymous image tests. |

### Static web assets (excluded from API JSONL)

The browser successfully loaded pinned dashboard assets under `/emby/web/*` during
both scenarios (strings, themes, login/home/item HTML templates, section tabs).
Those static asset requests are **documented here only** and are **not** rows in
`observed-web-stable.jsonl`. API workflow evidence is limited to non-static Emby
API calls.

Examples observed as HTTP 200 with cache-bust query key `v`:

- `/emby/web/strings/en-GB.json`
- `/emby/web/modules/common/strings/en-GB.json`
- `/emby/web/modules/themes/dark/theme.json`
- `/emby/web/list/list.html`
- `/emby/web/startup/manuallogin.html`
- `/emby/web/home/home.html`
- `/emby/web/modules/tabbedview/sectionstab.template.html`
- `/emby/web/item/item.html` (search-detail only)

### Direct image 200 evidence

- `observed-detail-image.tsv` is intentionally empty: the capture pipeline did not
  emit a redacted image row, but the browser verified a tokenless
  `GET /Items/{ItemId}/Images/{ImageType}` style response with HTTP 200.
- No query parameter **values** were retained; the curated image route therefore
  lists empty `queryNames` rather than inventing keys or values.
- Executable regressions: `TestResourceCookieAuthenticatesImageWithoutForwardingCookie`,
  `TestResourceCookieRouteAndCredentialPolicy`,
  `TestAnonymousItemImageForwardsOnlyValidatedTokenlessRequest`.

### Normalization and redaction rules (observed JSONL)

1. Input TSV columns only: method, path, status, sorted query-key **names**
   (never values, tokens, cookies, or bodies).
2. Drop `/emby` mount prefix; store Emby-relative `path` and `pathTemplate`.
3. Canonicalize fixed path segments to official API casing
   (for example `authenticatebyname` → `AuthenticateByName`,
   `system/info/public` → `/System/Info/Public`).
4. Replace disposable user segment `phase8-evidence-user` with `{UserId}` in
   `pathTemplate` only; keep the concrete disposable id in `path`.
5. Replace opaque item id segments with `{ItemId}` in `pathTemplate`; keep
   concrete ids in `path`.
6. Replace `DisplayPreferences/usersettings` preference id with `{Id}` in
   `pathTemplate`; keep concrete `usersettings` in `path`.
7. Preserve scenario (`login-home` / `search-detail`) and capture sequence order
   among API records (0-based `sequence` within each scenario).
8. `ownership` / `operation` / `projection` / `egress` are JSON `null` on raw
   observed records (curation happens only in `supported-routes.json`).
9. `provenanceKind` is `observed`; `transport` is `http`.
10. Uncaptured optional fields use explicit placeholders rather than fabricated
    facts: empty `requestContentTypes` / `responseContentTypes`;
    `initiator` / `bodyShapeHash` = `not-captured`;
    `rangeBehavior` / `websocketFrameCategory` = `unavailable`.
11. Exclude all `/emby/web/*` static assets from the JSONL.
12. Browser token was logged out and storage cleared after capture; raw HAR/URL
    dumps with query values are not retained.

### Ingress credential query names (documented, not egress purpose)

Clients often place Emby client identity and tokens in the query string. Observed
query **names** retained in JSONL include:

- Client identity (not secrets): `X-Emby-Client`, `X-Emby-Client-Version`,
  `X-Emby-Device-Id`, `X-Emby-Device-Name`, `X-Emby-Language`
- Credential name observed on authenticated calls: `X-Emby-Token`
  (name only; values never stored)

Curated `supported-routes.json` `queryNames` keep exact observed **neutral** names
for each template and **exclude** credential names (`X-Emby-Token`, `api_key`,
password aliases). Credential handling remains an ingress concern documented here
and in gateway auth tests; it is not an egress purpose.

## Supported routes (curated)

File: `supported-routes.json`

- Authoritative ordinary-client allow set is `routeclass.Inventory()` (exact
  method+pathTemplate rules with ownership/operation/authMode/projection/egress).
  `supported-routes.json` expands each inventory method to one curated record.
  Drift tests require 1:1 inventory↔corpus alignment without production JSON
  parsing. Personal templates list exact successful methods only (e.g. DisplayPreferences
  GET+POST, not PATCH). Download/stream/HLS/subtitle/audio use `resource-cookie-or-session`; binary item images use `anonymous-or-resource-cookie-or-session` (tokenless anonymous plus cookie/session).
- Each route has non-Legacy ownership / operation / projection / egress values
  aligned with existing gateway concepts (`LocalPublic`, `LocalPersonal`,
  `LocalSession`, `MetadataProxy`, `MediaProxy`; operations such as
  `Authenticate`, `Personal`, `PlaybackInfo`; projections such as `BaseItem`,
  `SystemInfo`, `PlaybackInfo`; egress `none` / `metadata` / `media` /
  `negotiation`).
- Evidence refs point at `observed-web-stable.jsonl#scenario:sequence` or
  `gateway-local-test` with named existing tests / official inventory /
  maintained-client pins.
- **Not** generated from OpenAPI. Official inventory is evidence only.

### Compatibility-decision media (W1) vs stable-Web observed

| Evidence class | Role | Authorizes alone? |
| --- | --- | --- |
| Stable-Web observed JSONL | Login/home/search/detail API traffic | Evidence for curated admission |
| Executable gateway tests | Handler/adapter/resource-cookie/m3u8 rewrite coverage | Required for gateway-local and media compatibility admissions |
| Official inventory (stable pin) | Confirms template spelling for stream/HLS/subtitle/download | **Not** automatic admission |
| Maintained-client pin (`plugin.video.emby`) | Corroborates modern direct/HLS/subtitle families | **Not** automatic admission |

W1 admits **exact segment-count** media templates only:

- `GET /Items/{ItemId}/Download`
- `GET /Videos/{ItemId}/stream` and `GET /Videos/{ItemId}/stream.{Container}` (finite container allowlist)
- `GET /Videos/{ItemId}/master.m3u8`, `GET /Videos/{ItemId}/main.m3u8`
- Exact-depth HLS: `/Videos/{ItemId}/hls/{SegmentId}.{SegmentContainer}`,
  `/Videos/{ItemId}/hls/{PlaylistId}/{SegmentId}.{SegmentContainer}`, and `hls1` twins
- Official subtitle stream aliases under `Videos` and `Items` (numeric index; optional
  start ticks; `Stream.{Format}` with finite format allowlist)
- `GET /Audio/{ItemId}/stream` and `GET /Audio/{ItemId}/stream.{Container}`

**Hard denials retained:** generic `/Videos/{id}/{FileName}` (e.g. `original.mkv`),
arbitrary multi-depth HLS, `/Items/{id}/Images` metadata list, HideFromResume,
ScheduledTasks, admin/plugin/device/package, broad first-segment families.

### Known gaps (do not broaden support to fill)

- No `POST /Sessions/Playing` / Progress / Stopped capture in this corpus.
- No stable-Web observed capture of stream/HLS/subtitle/download (W1 uses
  compatibility evidence only).
- No WebSocket (`/embywebsocket`) frame capture.
- No global `GET /Items` search path: the Web client searched via
  `GET /Users/{UserId}/Items` with `SearchTerm`. Global `/Items` is **not**
  admitted solely because OpenAPI lists it.
- No `HideFromResume`, `ScheduledTasks`, admin/plugin/device/package routes.
- No generic StreamFileName / arbitrary media-tree descendants.
- `observed-detail-image.tsv` empty; image admission rests on browser 200 plus
  existing resource-cookie tests, not invented query values.

## Official inventory extraction and normalization

Extraction source: OpenAPI v3 `paths` map only. Path-item keys that are not HTTP
methods (`parameters`, `$ref`, summary fields, vendor extensions) are ignored.

For each path template and each HTTP method among
`get`, `post`, `put`, `delete`, `patch`, `head`, `options`, `trace`:

1. Emit **one row per operation**.
2. Uppercase the method (`GET`, `POST`, …).
3. Preserve the OpenAPI path template string **exactly**, including brace
   parameter casing (for example `{UserId}` vs `{Id}`). Do not percent-decode,
   rewrite segments, or fold case.
4. Set `path` and `pathTemplate` to that same template string.
5. Copy `operationId` (empty string if absent) and `tags` in document order.
6. Normalize `security` to a sorted unique list of alternative scheme-name lists
   (each alternative is the sorted scheme names from one OpenAPI security
   requirement object). Missing `security` becomes `[]`.
7. Collect `queryNames` from operation `parameters` with `in: query`, preserve
   declared name casing, then sort unique.
8. Collect `requestContentTypes` from `requestBody.content` keys, sort unique.
9. Collect per-status `responseContentTypesByStatus` from `responses.<status>.content`
   keys (statuses with no content map are omitted), sort status keys, sort unique
   content types per status.
10. Set `responseContentTypes` to the sorted unique union of all per-status types.
11. Set curated placeholders `ownership`, `operation`, `projection`, and `egress`
    to JSON `null`.
12. Set `provenanceKind` to `"official"`.

Document-level rules:

- `schemaVersion` is `1`.
- `kind` is `"official-inventory"`.
- `pathCount` is the number of distinct path templates that have at least one
  extracted operation.
- `operationCount` is the number of operation rows.
- Rows are sorted by `(method, pathTemplate, operationId)` ascending.
- Operation keys are `(method, pathTemplate)` and must be unique within a
  fixture.
- JSON is UTF-8, two-space indent, trailing newline, deterministic field order
  as produced by the offline generator (not `sort_keys` across the whole tree;
  arrays that are sets are sorted as described above).

## Fixture files

| File | Purpose |
| --- | --- |
| `schema-v1.json` | Versioned schema for official / observed / supported shapes |
| `official-stable-4.9.5.0.json` | Exhaustive stable inventory (422 paths / 535 ops) |
| `official-beta-4.10.0.20.json` | Exhaustive beta inventory (428 paths / 543 ops) |
| `observed-web-stable.jsonl` | Normalized observed Web API records (stable workflow) |
| `supported-routes.json` | Curated authorization set (narrow; not OpenAPI-generated) |
| `PROVENANCE.md` | This document |

## Stable subset and beta-only operations

Every stable `(method, pathTemplate)` key is present in the beta inventory.

Exactly eight beta-only operations (not present in stable):

1. `GET /Parties/Messages`
2. `POST /Parties/Messages`
3. `GET /Persons/{Id}/Credits`
4. `POST /Users/{UserId}/CopyData`
5. `POST /Users/{UserId}/HomeSections`
6. `POST /Users/{UserId}/HomeSections/Delete`
7. `POST /Users/{UserId}/HomeSections/Move`
8. `POST /Users/{UserId}/SearchedItems/`

Beta-only and official-only entries remain **non-authorizing** until a curated
supported-routes decision.

## Regeneration checklist (offline)

### Official inventories

1. Download the pinned OpenAPI file into an approved temporary directory.
2. Verify SHA-256 matches the table above before any generation.
3. Optionally confirm the git blob id of the path at the pinned commit.
4. Run the offline extractor with the normalization rules above.
5. Assert path/operation counts (stable 422/535, beta 428/543).
6. Write only the normalized inventory JSON into this directory.
7. Discard the raw OpenAPI document; do not commit it.
8. Do not add runtime OpenAPI parsing to the gateway.

### Observed / supported corpus

1. Capture browser traffic against disposable gateway data and pinned 4.9.5.0 web
   assets; redacted TSV may contain only method, path, status, sorted query names.
2. Normalize with the observed rules above; never store query values or tokens.
3. Curate `supported-routes.json` by hand from stable-observed templates plus
   gateway-local tests; do not auto-generate from OpenAPI.
4. Document gaps rather than admitting unobserved routes.
5. Keep official inventory tests and fixtures intact.
