# ADR 0004: On-Demand Audio Transcoding

## Status

Implemented for audio conversion with Admin visibility. Video encoding remains
outside the scope. Implementation and native-player validation began on
2026-09-10; the validated input contract and operational bounds are below.

## Decision

Add a bounded, local audio-transcoding runtime behind the existing Emby API.
Use the unmodified Emby Web player, negotiate output from its device profile,
and serve negotiated MPEG-TS or fragmented-MP4 HLS on demand. Run FFmpeg as a child process of the Go
gateway inside the same Docker image. Extend the existing Admin surface with
operational visibility into conversion tasks and their segment cache.

Video packets are copied; audio is copied when compatible or encoded to AAC-LC
when necessary. Channel layout is negotiated. Video encoding, scaling, tone
mapping, and subtitle burn-in are outside this version.

The implementation should have one playback planner, one task owner, and one
segment catalog. Client-specific patches, a second job service, and duplicate
operational state machines are not part of the design.

## Evidence and compatibility target

The investigation on 2026-09-10 established:

- The inspected episode has HEVC/EAC3 and H.264/DTS-HD MA versions. Helium
  skipped the unsupported audio track while decoding video successfully.
- The upstream disables transcoding. Its media metadata and sampled media
  bytes match those returned through the gateway.
- CPU audio conversion and local HLS packaging were exercised on short samples.
  This is feasibility evidence, not deployed capacity or full-player validation.
- The currently served Emby Web builds `DeviceProfile` in
  `modules/browserdeviceprofile.js`. Desktop transcoding profiles default to
  two channels; TV/Chromecast branches use six.
- Its API client sends playback options in the query and `DeviceProfile` in
  the POST body. Parsing only a JSON `PlaybackInfoRequest` would miss options.
- Ordinary HLS seeks set the media element's time; they do not necessarily
  request new playback information.
- The current client stops encoding with
  `POST /Videos/ActiveEncodings/Delete?PlaySessionId=...&deviceId=...`.
- The unchanged desktop Web profile requests MPEG-TS HLS by default and mixes
  JSON booleans with boolean strings in `IsRequired`. These representations are
  normalized at the protocol boundary. Both HLS packagers use the same planner,
  timeline, scheduler, quota writer, and catalog.

Compatibility means the vendor Web assets can open, seek, pause, resume, switch
audio, and stop local conversion through these existing protocol operations.
The pinned original Web asset package was verified against its release SHA256;
its profile module matched the deployed module. Native playback, audio switching,
seeking, three independent users, and complete six-minute playback were verified
without modifying the player. A real short HEVC/EAC3 sample was additionally
verified in an isolated Helium instance, including 3840x2160 video, decoded audio,
and seek. These are not benchmarks of the deployed EPYC VM.

## Ownership

| Owner | Responsibility |
| --- | --- |
| `internal/gateway` | Emby request/response adaptation, user authorization, source resolution and authenticated upstream access, local UserData |
| `internal/transcode` (new) | Playback planning, source timeline, conversion tasks, FFmpeg lifecycle, segment catalog and resource limits |
| `internal/telemetry` | Bounded observation projection, aggregate series, recent outcomes |
| `internal/adminapi` and `web/admin` | Superuser-only read APIs and presentation |
| `cmd/gateway` | Configuration, construction, source/provider adapters, shutdown and deployment integration |

Keep planning and runtime concerns as small files in `internal/transcode`.
Introduce interfaces at the actual source-I/O, subprocess, and observation
boundaries. Do not create a general worker framework or video encoder stubs.
The task owner never imports Admin code or reads telemetry to make decisions.

Three identities have different lifetimes:

1. **Playback session:** the user's playback of an item, including pauses and
   seeks. It is distinct from a login session and an HTTP transfer.
2. **Output plan:** immutable source version, selected tracks, output encoding,
   and timeline. An audio/version change creates a new plan revision.
3. **Run:** one FFmpeg invocation producing a range for a plan. Seeking or
   recovery can replace a run without creating another user playback.

Each negotiated output has a distinct stream ID used in Emby URLs. A replacement
retains the logical playback ID. The client's explicit stop-encoding operation
retires the old output after negotiation; stopping an encoding without a
replacement leaves the same output address available for later demand.

Every published segment belongs to a plan and has a known source-time interval.
Runs may publish only while they own the corresponding production generation.
An obsolete run cannot overwrite a replacement run's output.

## Negotiation

Normalize query/body playback options at the Emby boundary, then pass typed
source facts, client capabilities, and configured service limits to a pure
planner. Conflicting inputs must not silently change the chosen source or user.

The planner evaluates container, protocol, video profile/level/bit depth, audio
codec/layout, subtitles, and bandwidth constraints. Codec-name membership alone
is insufficient. Evaluate the original source and the selected output container
separately; file playback support does not imply HLS/MSE support.

Prefer, in order, a compatible original stream, a compatible upstream result,
local remuxing, and local audio conversion. Local conversion remains possible
when the upstream cannot transcode. Upstream `SupportsDirectStream` flags do not
override a contradictory source/profile combination. Respect an explicitly
selected source; do not silently switch versions or lower resolution.

For audio conversion, choose AAC-LC only when the client accepts it in the
selected output protocol/container. Preserve supported source channels up to the
client's declared limit and any explicit user limit; do not upmix. Thus a
six-channel source can become AAC 5.1 or stereo depending on the request. The
gateway selects a bounded encoding-quality policy within those constraints.
Unknown capability and observed playback failure are distinct inputs, not a
reason to claim universal support or add global browser-name exceptions.

Plans have explicit outcomes: passthrough, remux, audio conversion, or a bounded
rejection reason such as video encoding required. Future video conversion can
extend this decision and the executor boundary. This version neither advertises
nor executes that capability.

Return coherent `MediaSources` flags and a local `TranscodingUrl` for the
negotiated plan. Preserve source metadata; do not relabel the original file's
audio codec as AAC. HLS manifests and initialization segments describe the actual
output. Metadata-only `PlaybackInfo` calls must not start FFmpeg or prefetch media.

## HLS timeline and source access

The gateway owns a complete VOD manifest and the mapping from each segment to
its source-time interval. FFmpeg writes one media stream to stdout. A format
packager recognizes indexed random-access boundaries and writes through the
shared catalog. FFmpeg does not independently manage segment files or a second
playlist. This makes byte reservation enforceable before disk writes.

For copied video, segment boundaries follow validated random-access points.
Nominal segment length is a target, not permission to invent equal durations.
Variable frame rates, open GOPs, in-band codec headers, audio encoder delay, and
nonzero timestamps belong in this timeline/packaging layer, not in route patches.
Initialization data must stay compatible across restarts of the same plan.

Start with finite, seekable MKV/MP4 sources and client-compatible H.264/HEVC video.
Read container metadata/indexes through bounded Range access and cache the
validated timeline for the source version. The original MKV headers expose
SeekHead pointers to tail Cues indexes. Range extraction and seek were exercised
against generated files and the complete remuxed clip used for Helium validation.
The metadata parser validates track association and random-access semantics
rather than treating every cue as a video keyframe.

A source without a trustworthy seekable timeline cannot be advertised as fully
seekable local HLS. Missing/corrupt indexes and unsupported containers have an
explicit capability result. Do not hide whole-file scans in the start path or
guess segment timestamps. The supported-input contract and its failure behavior
must be exercised before adding formats.

FFmpeg reads a narrowly scoped internal HTTP source with Range support. The
gateway remains responsible for upstream credentials, required headers, endpoint
selection, redirects, refresh, and source-version checks. This endpoint requires
no additional public port or deployment service. Credentials and expiring signed
URLs are not cache identities or Admin data.

Upstream reconfiguration uses the existing authoritative exclusion boundary.
Non-forced reconfiguration checks active conversion work directly. Each source
transfer acquires the media gate before validating its binding and releases it
when its body closes. A run does not recursively hold the same read lock.
Forced reconfiguration drains current I/O; subsequent reads reject a changed
origin/backend identity or media version. Credential/device rotation alone does
not change media identity. No observation is used as the only guard.

## Demand, scheduling, and lifecycle

Segment demand drives work. Playback reports supply playhead and pause intent,
but progress reporting is not sufficiently reliable to be the sole work lease.
Bound both the number and lifetime of retained sessions and outstanding demands.

| Event | Runtime behavior |
| --- | --- |
| Requested segment exists | Pin and serve the complete artifact |
| Requested segment is being produced | Join its bounded wait; do not launch duplicate work |
| Demand is near the current run | Continue production with bounded lookahead |
| Demand jumps outside the produced range | Schedule a run from the appropriate source random-access point |
| Pause or no new demand | Stop production at the lookahead limit; park the task and retain useful segments |
| Resume after parking | Use retained segments and restart only when more output is needed |
| Seek to an evicted range | Regenerate the required interval using the same timeline |
| Audio/source selection changes | Negotiate a new plan revision and retire obsolete work |
| Stop, revocation, expiry, or shutdown | Cancel work, close source I/O, join processes/readers, release reservations |

Use one serialized state owner per task. It chooses production from active
demands, not simply the last arriving packet or the largest segment number.
Canceled and out-of-order requests must not repeatedly move work away from a
valid current demand. Single-flight production is separate from individual HTTP
waiters; canceling one waiter does not cancel work still needed by another.

The public manifest retains the full timeline after cache eviction. A missing
artifact is generated on demand or returns a deliberate bounded error; an
advertised segment is not silently removed from the film's seekable range.
Publish complete files atomically and discard unfinished trailing output.

FFmpeg runs use task-owned contexts, not manifest/segment request contexts.
The Go owner drains bounded progress/error streams, requests termination, and
waits for exit, with a bounded forced-termination path. Parking releases CPU
slots; it must not keep an abandoned upstream download alive indefinitely.

Handle the verified stop-encoding route locally for local plans. Stopping an old
encoding run is distinct from ending playback or deleting a negotiated replacement
plan. This distinction is required by the client's audio-change sequence.
Every stop and segment operation remains scoped to the authenticated user's
playback; one user's stop cannot stop another user's worker.

Worker completion and generated-media position never mark an item watched.
UserData continues to reflect client playback reports through the existing local
playback pipeline, with Emby ticks kept in 100 ns units.

## Cache and resource bounds

The existing media buffer and the new segment cache have different ownership:

| Storage | Lifetime | Meaning |
| --- | --- | --- |
| Existing media buffer | HTTP media transfer | Optional RAM read-ahead of incoming bytes |
| Segment cache | Output plan, subject to eviction | Complete reusable media intervals for playback/seek |

Keep ADR 0002/0003 mechanics intact. Their memory pool is not a reservation pool
for FFmpeg processes or segment files. Account for upstream ingress and client
egress once; internal loopback copies must not inflate external traffic metrics.

The segment catalog is authoritative for keys, intervals, file ownership,
reservations, completed sizes, and active-reader pins. Enforce a global storage
budget including in-progress reservations, a 16,384-artifact metadata bound, and
bounded worker, queue, and source-probe concurrency. Process memory is separate
from the Go heap and cache storage budget.

Eviction removes complete artifacts without active readers or waiting demands
in oldest-use order. It cannot
race an active reader or publish a partial file. Sequential output is retained
within budget to make backward seeks useful. Session expiry removes ownership;
restart recovery cleans only the dedicated cache namespace and does not revive
stale authorization or orphaned tasks. Nothing is stored in repo `pb_data`.

V1 uses session/plan-scoped artifacts under a global budget. Separate user
playheads do not share mutable FFmpeg control. Storage deduplication can later
reuse proven-identical immutable artifacts without changing playback semantics.

Expose a small startup configuration: enablement, FFmpeg location if needed,
active-worker limit, cache directory, and cache budget. Keep ordinary timing and
lookahead policies as documented bounded defaults until measurements justify
public tuning. Overload has a bounded queue/deadline and an explicit result.
Direct playback does not consume a conversion slot.

## Admin information model

Add a `Transcoding` page alongside `Buffer`, using the existing visual language.
Extend Overview with a compact conversion summary and link Activity to the exact
conversion playback. Avoid changing the meaning of existing Buffer counters.

| Surface | Information |
| --- | --- |
| Overview | Running/available worker slots, queued tasks, cache used/budget, recent failures |
| Active task list | User/device, item/version, source audio to target audio/layout, video copy, reason, task state, last update |
| Task detail | Reported playhead, retained media intervals, requested interval, production position, lookahead, run/restart count, wait or failure reason |
| Resource detail | FFmpeg availability/version, worker slots, cache usage/pinned/reserved bytes, measured worker CPU/RSS when available |
| Recent outcomes | Bounded completed/stopped/failed records and normalized reasons |

Task states are a small set: preparing, queued, producing, ready, stopped, failed.
`ready` is displayed as Idle: no conversion process is active. Cached output is
shown separately; neither state implies that the user is playing. Client pause
status is separate and timestamped. Unknown/stale
playhead data is shown as such, not inferred from bytes downloaded. Generated
intervals are not mislabeled as the browser's buffered ranges.

Report processing speed from media-time progress during an active run, with
seeking offsets accounted for. A parked worker is not "too slow". CPU/RSS values
must be measured; unavailable platform measurements are null, not zero or a
guess from the FFmpeg speed multiplier. Do not present FFmpeg RSS as Go heap.

The runtime owns all scheduling/cache truth. It offers a bounded observation
projection to the existing telemetry layer; Admin reads that projection. Admin
polling never starts probes, scans cache directories, starts work, or feeds
scheduling decisions. Slow or dropped observation must not alter playback.
Snapshots and task outcomes remain in memory, with a bounded recent-outcome ring.
Existing traffic history remains in the telemetry registry. No PocketBase schema
changes are required for telemetry.

Use the existing superuser auth and no-store API policy. Proposed read surfaces
are `/admin/api/v1/transcoding/jobs`, a job detail route, and a recent-outcomes
route; aggregate data is added under `transcoding` in Overview. Use bounded
pagination and boot-scoped stable IDs, carried explicitly into Activity links.
Avoid joining tasks by user/item names or timestamps.

Poll only while the page is visible, cancel stale requests, and remain within
the existing Admin request budget. Reuse table, capacity-bar, and detail patterns
where suitable; do not copy Buffer's internal state machine into the UI. Raw
upstream URLs, credentials, full commands, and unbounded FFmpeg logs are excluded.
This scope is visibility; runtime-setting and destructive cache controls are
separate features.

## Packaging and extension boundary

Use `os/exec` with argument arrays. Bundle FFmpeg/ffprobe and their runtime
dependencies in the existing Docker image for each target architecture; retain
the Go `CGO_ENABLED=0` build. Add target-architecture package/build validation to
the current CI rather than introducing a separate deployed worker service.
Standalone binary users supply an executable path or install FFmpeg themselves.

Explicitly enabled conversion requires verified executable, decoder, encoder,
and muxer availability plus a usable bounded cache directory. Missing required
capabilities fail enabled configuration at startup instead of being advertised
to clients. Disabled conversion leaves the existing synchronous proxy and
buffering choices valid. Dependencies are resolved at build/install time.

Future video support can extend the planner/executor and have its own resource
budget. Do not add unused encoder implementations, GPU setup, video quality
controls, or claims of video-transcoding support to this change.

## Implementation and acceptance

First prove the protocol with the unchanged deployed Web asset version and a
temporary local gateway/data directory. This is a compatibility experiment, not
a frontend fork. Then implement the planner/timeline and task/cache owner, wire
bounded Admin projections and UI, and complete packaging/integration validation.

Required evidence includes:

- A source/profile decision matrix, including six-channel preservation, stereo
  negotiation, source selection, unsupported video, and query/body inputs.
- Original Emby Web playback, audio selection, and stop-encoding behavior without
  asset modifications; compatible native clients retain direct playback.
- Cold start/resume; near/far forward and backward seeks; repeated rapid seeks;
  pause past worker parking and cache expiry; source/audio changes.
- Long-running A/V/subtitle timing, VFR and fractional-frame-rate sources,
  random-access prerequisites, and matching initialization data after restarts.
- Duplicate/out-of-order/canceled segment requests; pinned-file eviction;
  exhausted cache/worker budgets; queue timeouts; abrupt source/worker failure.
- Multiple users/devices playing the same item independently, revocation,
  process shutdown, source reconfiguration, and restart cleanup.
- No false watched/resume updates from preparation, worker EOF, or cache hits.
- Admin stale/disabled/unavailable states, exact Activity linkage, and unchanged
  playback with slow polling, dropped observation, or absent telemetry.
- Linux amd64/arm64 images containing usable matching FFmpeg runtimes, existing
  binary builds, required Go checks, and Admin check/build/E2E after UI changes.

The native-player closure is `web/admin/scripts/run-audio-e2e.sh`. It builds the
current SPA and gateway, generates media, creates a temporary database, and runs
the original Web package against real conversion. The ordinary Admin closure
remains a separate regression. Go tests include MP4/MKV indexes, both packagers,
channel negotiation, protected cache entries, demand cancellation, replacement,
source reconfiguration, credential rotation, and Admin authentication/pagination.

Local conversion requires finite MKV/MP4, one supported video track, and a
reliable seek index. Index reads are bounded to 24 MiB, keyframe count to 200,000,
and duration to 72 hours. Selected subtitles must have an existing external
delivery URL; subtitle burn-in or loss of embedded selected subtitles is rejected.
The inspected real episode's default and external subtitles use external delivery.
The default runtime uses four workers, a four-segment lookahead, a 4 GiB cache,
128 retained output plans, six-hour idle expiry, and a 90-second work deadline.
CPU/RSS process sampling is implemented for Linux; other platforms report null.

## References

- [Emby PlaybackInfo contract](https://dev.emby.media/reference/RestAPI/MediaInfoService/postItemsByIdPlaybackinfo.html)
- [Deployed Web device-profile builder](https://emby.xvv.net/emby/web/modules/browserdeviceprofile.js)
- [Deployed Web playback manager](https://emby.xvv.net/emby/web/modules/common/playback/playbackmanager.js)
- [Deployed Web API client](https://emby.xvv.net/emby/web/modules/emby-apiclient/apiclient.js)
- [FFmpeg stream copy](https://ffmpeg.org/ffmpeg.html#Streamcopy)
- [FFmpeg HLS muxer](https://ffmpeg.org/ffmpeg-formats.html#hls)
- [Matroska Cues](https://www.matroska.org/technical/cues.html)
- [Jellyfin dynamic HLS reference](https://github.com/jellyfin/jellyfin/blob/master/Jellyfin.Api/Controllers/DynamicHlsController.cs)
- [ADR 0001](0001-admin-control-plane.md), [ADR 0002](0002-adaptive-media-buffering.md), [ADR 0003](0003-media-buffer-observability.md)
