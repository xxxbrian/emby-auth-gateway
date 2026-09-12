# ADR 0005: Optional Web subtitle preparation

## Status

Implemented, conservative first version, 2026-09-12. The broader shared-source
design remains a direction for future work. Existing playback isolation takes
precedence over recovering every subtitle.

## Context

Some upstream embedded SubRip delivery endpoints return an empty document even
though the original MKV contains subtitle packets. Native clients can use the
original tracks. Web playback instead requests separately delivered text.
Scanning a high-bitrate movie to repair that endpoint can transfer tens of
gigabytes, contend with playback, and repeat work when languages change.

The approved design calls for shared source ranges and separate subtitle
artifacts. The initial implementation deliberately limits recovery to indexed
Matroska text tracks. It can hide an unavailable Web track rather than introduce
unbounded work or destabilize established playback.

## Decisions

### Entry and isolation

- `GATEWAY_WEB_SUBTITLES_ENABLED` defaults to `false`. With it disabled, existing
  metadata, media readers, audio negotiation, and client behavior continue along
  their original paths.
- Only successful canonical local Web document requests establish a signed,
  host-bound `EmbyGatewayWebContext` cookie. It is separate from gateway login,
  scoped to `/emby`, HttpOnly and SameSite=Strict, and Secure outside loopback
  development. It expires after 24 hours; the signing key changes on restart.
- Admission additionally requires an authenticated session declaring `Emby Web`
  and no contradictory client/device/origin/fetch evidence. Browser-looking
  User-Agent, DeviceProfile, a VTT request, or cached content alone cannot opt in.
- Native and unidentified clients retain original subtitle metadata and never
  start probes or indexed preparation. Their media readers are not wrapped.
- Web filtering happens on the response projection, not shared catalog records.
  Playback progress, completion, and personal state remain gateway-local and
  independent of subtitle production.
- Startup failure in this optional module is reported in Admin and does not
  prevent the existing service from starting. If Web context can be verified,
  the Web projection hides tracks the unavailable runtime cannot deliver.

### Preparation and Web projection

`internal/subtitles` owns jobs, per-Web-viewer lifetimes, artifact grants, and
cache budgets. `internal/subtitleindex` is a bounded container reader. The
gateway supplies authenticated source and subtitle-document readers using its
existing credential, redirect, route-policy, and media-source checks.

Library listings project existing ready artifacts and do not probe the upstream.
Explicit item detail or PlaybackInfo requests may probe subtitle delivery for a
bounded number of sources. Healthy ASS, SRT, or WebVTT is validated and saved as
a complete document. A nonempty invalid document does not become available.

An empty embedded SubRip/SRT delivery can be recovered when a selected/default
candidate has a usable Matroska subtitle index. A small preview budget exists
because a hidden track cannot otherwise be selected in the stock Web UI. The
extractor uses subtitle positions and durations; a video seek index alone is
insufficient. Recovery additionally requires a current strong source ETag.
Missing/weak validators leave the local track hidden with
`source_validator_missing`; hashing the first 4 KiB cannot prove that subtitle
bytes elsewhere in a same-size file are unchanged. It produces complete WebVTT
artifacts without decoding video or invoking a full-file FFmpeg scan. Recovery
covers validated subtitle Cues; it cannot prove that the container has no
additional unindexed subtitle packets. A completed artifact is not a guarantee
of complete dialogue coverage for a file with an incomplete index.

Only ready, currently authorized artifacts appear in Web `MediaStreams` and
subtitle-presence indicators. Track indexes are preserved. Missing defaults or
requests resolve to subtitles Off so subtitle failure does not independently
reject audio/video negotiation. Availability can reappear on later metadata
refresh. The vendor Web code is unchanged: there is no promise of a live update
to an already-open subtitle menu or of immediate subtitles on a first visit.

HTTP delivery authenticates both the gateway session and a bounded opaque grant
scoped to its owner, playback, item, source, and track. Even a cached upstream
document rechecks the original delivery path policy and current upstream binding
without a new origin request. Indexed artifacts also validate source identity
before reuse. Actual playback takes over earlier preview
admissions for the same viewer, item, and source, so stopping it also releases
their work and grants. Selecting Off cancels obsolete preview recovery unless
another Web viewer still needs it. A viewer leaving releases only its work;
a native viewer never keeps Web preparation alive. Lease expiry handles abandoned
tabs whose final playback report did not arrive.

### Cache ownership and limits

The runtime retains two separate stores beneath `GATEWAY_SUBTITLE_CACHE_DIR`:

| Store | Default managed data budget | Retention and role |
| --- | ---: | --- |
| `eag-subtitle-artifacts-v1` | 128 MiB | Complete text documents; strong-ETag indexed results may be reused across restarts. Startup discards files older than seven days; capacity can evict unused files earlier. |
| `eag-source-cache-v1` | 256 MiB | Disposable immutable source ranges; cleared at startup. |

Both stores use ownership markers and exclusive locks. They never migrate or
clean PocketBase data. The standalone default parent is
`<system temp>/emby-gateway-subtitles`; Compose mounts a separate named volume at
`/app/subtitle_cache`. Durable storage is needed for restart reuse; a temporary
directory's contents may be removed by the host.

The source cache materializes only exact requests up to 4 MiB. Larger requests
keep streaming behavior. Covered ranges and concurrent duplicate requests share
work; no source read-ahead or whole-file preallocation is introduced. Reservations
precede writes, readers pin entries, and LRU eviction respects those pins. The
4096-entry limit also bounds files and metadata. Optional Web audio capture
stores only actually consumed 64 KiB pages carrying a current strong source
ETag. A single asynchronous writer receives at most sixteen queued pages (1 MiB),
with one 64 KiB writer page and at most 64 KiB per active captured reader. Playback
reads and source closes never perform cache filesystem I/O or acquire its catalog
lock. Busy/full queues, capacity limits, or storage failures drop capture without
changing the media body.
Native and normal direct-play streams do not use this capture path.

Keys represent source identity and generation, not signed URLs or login tokens.
Rotating a credential does not by itself change the bytes. Strong-ETag extracted
results can survive process restart. Weak or missing source identity does not
permit local indexed recovery; valid upstream-delivered documents remain scoped
to the process. Authorization is checked on reuse and
is not shared merely because source bytes are shared.

Production composition uses these fixed bounds:

| Limit | Value |
| --- | ---: |
| Subtitle workers | 1, separate from audio workers |
| Per-source synchronous preparation wait | 1.5 seconds |
| Total Web response preparation wait | 2 seconds across sources |
| Normal job deadline | 30 seconds |
| Normal indexed source reads / requests | 32 MiB / 512 |
| Detail-preview indexed extraction | 5 seconds / 8 MiB / 128 requests |
| Subtitle artifact | 8 MiB maximum |
| Sources / tracks per source | 64 / 64 |
| Subtitle grants | 2048 |
| Each cache's entry limit | 4096 |

Budgets include cache hits as extractor work where appropriate; actual source
body reads and reused cache bytes are distinct counters. Small validation reads
and upstream subtitle-document probes are separate from the indexed-read limit.
The timeout and byte/request limits are hard admission limits, not performance
guarantees. Existing audio disk and adaptive-buffer RAM budgets remain separate;
there is no new global scheduler or retroactive change to their reservations.

### Observation and packaging

Composition occurs in `cmd/gateway`. Telemetry exposes only an in-memory snapshot
through a provider; observations cannot schedule source access or preparation.
The superuser-only `GET /admin/api/v1/subtitles` uses bounded pagination and a
100 KiB response limit. Admin shows enablement/failure reason, source size,
workers, managed cache bytes, source reads/cache reuse, per-track mode, output
bytes/cues, and why a Web choice is hidden. It does not expose source URLs,
credentials, subtitle text, or cache keys as capabilities.

The service still ships as one Go process and the existing Docker image. The
subtitle reader adds no new executable, daemon, database schema, CGO requirement,
or FFmpeg dependency beyond the runtime already included for audio conversion.

## Explicitly deferred

- Full-file or resumable background scans, operator prewarming, and a shared
  multi-language scan job. There is no setting that enables scanning now.
- MP4 subtitle indexing, embedded ASS recovery, PGS/VobSub conversion, OCR, and
  video/subtitle burn-in.
- Incremental subtitle windows, partial coverage persistence, live Web-menu
  updates, and a universal low-latency guarantee for all indexed media.
- Wrapping the ordinary proxy or native media streams, general source-I/O
  priority scheduling, merging adjacent cache ranges, and a single disk budget
  governing all existing caches.

These may be evaluated independently later. A missing index or exhausted budget
currently yields a hidden Web choice rather than a sequential source scan.

## Verification and practical limits

Package tests cover source generations, multi-viewer ownership/cancellation,
cache bounds/eviction, persistent artifacts and weak-validator rejection,
malformed indexes,
empty/invalid delivery, and native/off isolation. Large sparse/logical fixtures
exercise 50–64 GiB offsets with bounded reads; they do not measure the complete
transfer or performance of a real 50 GB movie.

`bash web/admin/scripts/run-subtitle-e2e.sh` builds fresh temporary services and
uses a pinned unmodified Emby Web package. The browser closure covers rendered
subtitles, seeking, language changes, native requests before/after Web recovery,
and Admin. `SUBTITLE_E2E_MODE=rapid` selects the separate rapid-seek closure;
`SUBTITLE_E2E_FEATURE_ENABLED=false` permits the same flow with this feature off.
That manual 24-seek comparison reproduced a latched original-player playback
error with recovery disabled, despite media later becoming ready; the enabled
run was clean on the same binary and media. The rapid diagnostic is not a
regular CI gate and is not evidence that every existing seek race is fixed.
Runtime/codec tests, the existing audio closure, and the original Admin closure
remain separate regression gates. A fixture result must not be presented as
production throughput or universal browser coverage.

See [the design evidence and future direction](../design/shared-source-cache-and-subtitles.md),
[ADR 0001](0001-admin-control-plane.md), and [ADR 0004](0004-on-demand-audio-transcoding.md).
