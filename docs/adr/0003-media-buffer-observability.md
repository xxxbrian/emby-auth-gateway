# ADR 0003: Media Buffer Observability

## Status

Accepted

Oracle Gate 1 accepted this ADR. It supersedes only ADR 0002's `Snapshot
telemetry` decision and the matching aggregate-only acceptance bullets. ADR 0002
continues to govern buffering mechanics, allocation, fairness, cancellation,
copy correctness, configuration, rollout, and rollback.

This ADR uses RFC 2119 meanings for normative `MUST`, `MUST NOT`, `SHOULD`, and
`MAY` terms.

## Decision Summary

The gateway MUST provide bounded operational visibility into adaptive media
buffering without allowing observation to influence buffering. Existing
aggregate `/overview` and aggregate telemetry-stream contracts remain intact.
New authenticated Admin APIs provide bounded live streams, direct stream detail,
recent completions, and aggregate media-buffer gauge history.

Live observation uses one preallocated raw-ID-ordered slot slice capped at 4,096
slots per boot. Buffering continues unchanged beyond that cap; bounded drop and
completeness telemetry makes partial observation explicit.

The buffering controller remains authoritative for allocation and fairness. A
concrete one-way sidecar projects bounded state into the existing telemetry
registry. Admin copies the transfer and buffer registries independently and
performs enrichment after those copies. No observer operation is in the 32 KiB
media-copy loop or feeds a buffering decision.

## Context And Problem

ADR 0002 currently exposes this bounded aggregate:

```text
enabled, hard budget H, allocated P, owned O, free F,
active requests N, base-only requests, indebted requests,
total request-debt bytes
```

The aggregate does not answer which producer is waiting for an optional buffer,
which consumer is waiting for data, whether allocation is pool-exhausted or
debt-limited, whether a stream is in a sustained read/write condition, or which
completed outcome occurred. It also cannot provide a safe Activity-to-buffer
join without a direct identity.

The current buffered copy has one producer goroutine reading the upstream body
and acquiring either its reusable base or an optional chunk. The consumer drains
the copy queue and writes downstream. The media gate is held for the complete
copy, optional controller accounting is mutex-authoritative, and the transfer
meter uses atomic byte updates with `TransferHandle.End` removing the active
meter entry. Observability MUST fit those existing ownership boundaries.

## Scope And Non-Goals

### Goals

The implementation MUST:

1. Preserve every ADR 0002 buffering decision and invariant.
2. Use one stable boot-scoped identity for each buffered stream.
3. Bind stream identity to the authoritative transfer before media I/O.
4. Expose bounded operator-relevant live state and bounded completion summaries.
5. Define exact producer, consumer, allocation, lifecycle, health, and outcome
   semantics that can be tested with a fake monotonic clock.
6. Provide exact aggregate and gauge-history contracts for the Overview and Buffer
   pages, including eventual sidecar totals.
7. Preserve privacy, authorization, disabled behavior, and performance budgets.

### Non-goals

This ADR MUST NOT introduce:

- a buffering algorithm, admission rule, fairness policy, queue protocol, or
  cancellation rule;
- runtime configuration, dynamic budgets, quotas, or per-stream tuning;
- persistent telemetry, a second metrics/event framework, per-chunk events, or
  per-stream historical series;
- frontend inference of identity, joins, rates, health, or state;
- raw errors, URLs, paths, queries, headers, tokens, IPs, session hashes, or
  unbounded identifiers in new buffer DTOs;
- a goroutine per stream, observer callback fanout, or observer feedback into
  controller decisions; or
- labels in `/overview` or `/metrics/stream`.

## Supersession Boundary

ADR 0002 remains authoritative for modes, budget selection, private bases, pool
accounting, targets, close-once bodies, server integration, producer/consumer
ownership, read/write semantics, result precedence, rollout, and rollback.

This ADR supersedes only:

- ADR 0002's `Snapshot telemetry` decision that visibility is limited to
  aggregate fields and forbids labels, sampling, and a new event stream; and
- ADR 0002 acceptance bullets requiring only aggregate Snapshot fields and
  describing aggregate-only telemetry as the complete operational surface.

ADR 0002 history MUST NOT be rewritten. The ADRs MUST be read together after
this proposal is accepted.

## Architectural Boundaries

The dependency direction MUST be:

```text
buffering mechanics
    -> concrete gateway media-buffer sidecar
    -> bounded gateway/admin provider adapters
    -> existing telemetry Registry and bounded DTOs
    -> Admin UI
```

The controller MUST remain unaware of Admin packages, routes, JSON field names,
history storage, chart models, and frontend types. It MAY hold an optional
concrete sidecar reference and perform fixed-field projection calls. Observation
is one-way: the controller writes state and never reads observation to decide
allocation, scheduling, queueing, cancellation, copying, or cleanup.

Package ownership MUST be:

- `internal/gateway`: controller-side sidecar, finite projection words, stream
  identity, capture-time sanitation, and bounded provider copies;
- `internal/telemetry`: existing Registry boot identity accessor, bounded live
  buffer registry, dedicated media-buffer gauge ring, completion retention, and
  completion-drop accounting;
- `internal/adminapi`: authentication, defensive DTO validation, pagination,
  response-size enforcement, and route behavior;
- `cmd/gateway`: explicit Registry/provider wiring; and
- `web/admin`: polling, presentation, semantic states, and accessibility. It
  MUST NOT perform authoritative joins or health calculations.

The controller and 32 KiB copy loop MUST NOT import Admin packages or acquire
telemetry Registry locks. Pre-I/O live-registry registration and terminal
nonblocking completion offer are explicit lifecycle exceptions. A nil provider
MUST be valid.

## Identity And Transfer Association

The implementation MUST reuse the existing telemetry `Registry` boot identity
and MUST add a cheap `BootID()` accessor for it. It MUST NOT create a second
process boot-ID generator. The accessor MUST return the same immutable boot ID
already emitted by telemetry.

The controller request sequence and transfer meter sequence are unsigned
`uint64` values scoped to that boot. All API and DTO identifiers MUST be encoded
as unsigned base-10 JSON strings. The stable stream identity is exactly:

```text
(boot_id, stream_id)
```

`stream_id` MUST be monotonic and never reused during a boot. A new boot MUST
never be inferred to continue an old stream or transfer.

At the pre-I/O lifecycle boundary, registration MUST capture every immutable
bounded identity field except `transfer_id`. It MUST append the stream to the
live registry before I/O, as specified below. Existing server orchestration MUST
put `(boot_id, stream_id)` into the `TransferMeta` passed to `BeginTransfer`, then
call `BeginTransfer` at its current ADR 0002 ordering. It MUST NOT move that call
earlier. The existing cheap `TransferHandle.ID()` accessor MUST be reused after
`BeginTransfer` returns; no new handle-ID accessor is required. One zero-to-ID
atomic/CAS bind MUST write `transfer_id` before counted I/O and before header
commitment. The transfer handle MUST retain that identity until
`TransferHandle.End`.

The authoritative transfer DTO carries only the nullable identity linkage:

```text
media_buffer: { boot_id, stream_id }
```

It MUST NOT carry a copied buffer row or attempt a join while holding the
`ByteMeter` lock. Admin MUST copy the active transfer registry and active buffer
registry independently, release both locks, and enrich the Activity response by
the identity pair. A missing or old stream is represented as stale/complete; no
session, item, timestamp, path, or row-order inference is permitted.

`transfer_id` MAY appear in a buffer DTO after the one-time bind as an already-known
bounded operator identity. It MUST be encoded as an unsigned base-10 boot-scoped
string. Existing transfer session fields remain unchanged for existing transfer
consumers; new buffer DTOs omit those session fields and never expose session
hashes.

## Active Registry Protocol

The live registry MUST use one dedicated `RWMutex` wholly outside controller,
queue, and `ByteMeter` locks. It MUST maintain exactly one raw-ID-ordered slot
slice. The normative observed-active maximum is exactly 4,096 streams per boot.
Registry construction MUST preallocate that slot slice with capacity exactly
4,096. No request-path slice growth is permitted. Stream IDs are monotonic, so
registration MUST append one slot; it MUST NOT sort, compact, or scan existing
membership.

Registration allocates and captures the bounded sidecar identity before taking
the membership lock. It MAY then briefly acquire the live-registry write lock to
first enforce `len(slots) < 4096` and then append exactly one monotonic slot. No
append path may bypass this guard. It MUST perform no I/O or controller work
and MUST NOT hold or acquire a controller or queue lock while holding the live
lock. If the guard fails because live rows or unreclaimed tombstones occupy all
4,096 slots, registration MUST drop observation nonblockingly, increment
cumulative `live_registration_drops`, create no live membership or transfer
linkage, and leave media buffering unchanged. The controller stream ID remains
internal and the authoritative Transfer DTO linkage remains null for a dropped
observation.
The registration path is benchmarked for lock wait and hold time at N=512 and
N=4,096. The identity capture, guarded live append, `BeginTransfer` call, late
bind, and header commitment all occur before the first counted read/write; the
32 KiB copy loop performs none of these registry operations.

The page API acquires the live-registry read lock only long enough to binary-search
the ordered slice by raw cursor ID, copy at most `limit` raw slot pointers, and
check at most one additional raw slot for `has_more`. It then unlocks before
reading pointer domains, copying rows, applying sanitation, or serializing. A
detail request acquires the read lock, binary-searches the same ordered slot slice
by raw stream ID, copies at most one sidecar pointer, and unlocks before reading
the row. Detail work is O(log 4096); page work is O(log N + limit + 1). Neither
route holds the live lock with controller, queue, ByteMeter, I/O, or JSON work.

The sampler MAY scan the ordered pointer membership while holding the live
registry read lock. It MUST allocate nothing and MUST read only bounded atomic or
single-writer domains. Sampler read-lock hold and wait time MUST be measured at
N=512 and N=4,096. A terminal atomic state allows the sampler to remove and
compact terminal membership asynchronously under the live-registry write lock,
outside the media gate. One maintenance acquisition MUST process at most the
fixed 4,096 slots. Stable compaction reuses the preallocated backing storage;
O(N) removal and compaction are permitted only in this terminal maintenance path
with N bounded by 4,096. It MUST preserve ascending raw stream IDs and MUST keep
the slot slice backing pointer and capacity unchanged. Registration never
compacts.

The completion-summary channel/ring and its `completion_drops` counter are
independent of active membership removal. A dropped completion MUST NOT retain a
live pointer. Terminal rows observed by a sampler remain in the ordered
membership until terminal maintenance removes them; the sampler observes their
terminal flag for maintenance but excludes them from active derivation. Active
page/detail APIs treat a terminal row as no longer available. The page omits a terminal pointer and may
return fewer than `limit` items without scanning a replacement; detail returns
404 `stream_not_found`. The only two request-path observer interactions are
registration and terminal completion offer. Removal is sampler-owned and
asynchronous. Registration or an offer may contend briefly with a sampler/API
reader, but neither may wait on media I/O or the media gate. A late page sees
either a current pointer or a bounded stale/terminal result; it never retries by
scanning. Terminal slots retain their immutable raw stream IDs until maintenance.

Terminal slots are maintenance-only. They MUST be excluded before deriving
observed-active counts, queued/writing totals, buffer-acquire, pool, consumer,
upstream, downstream, and close/join counts, health reasons, warning/critical
counts, active page/detail results, and series live totals. Controller active
request count already excludes an unregistered request. A terminal row observed
between completion and maintenance is stale, not an active stalled stream, and
MUST NOT create false critical close/join health.

## Capture-Time Privacy And Bounds

Identity is sanitized before it enters the sidecar, active registry, history, or
completion retention. At registration, user ID, username, device, item ID, and
finite reason strings MUST be allowlisted, normalized, UTF-8 safely truncated to
at most 256 bytes, and stripped of NUL and ASCII control characters except
normalized whitespace. The late-bound transfer ID MUST receive the same treatment
before its atomic bind. Empty optional values become null. Capture MUST perform
no metadata resolution, HTTP request, database access, or wait.

Capture MUST copy into fixed bounded sidecar storage associated with the existing
request registration. It MUST NOT add a heap allocation to controller/copy
projection operations. Registration is the only point where immutable identity
is accepted except for the explicit zero-to-ID transfer binding after
`BeginTransfer`; later domains retain or copy the bounded representation rather
than recleaning an unbounded source value.

Admin MUST apply the same bounds and validation defensively before serialization.
The API MUST exclude IP addresses, session hashes, URLs, paths, query strings,
headers, tokens, backend credentials, raw errors, stack traces, arbitrary
upstream metadata, and unbounded client strings from all new buffer DTOs.
Existing Transfer session fields are outside this new DTO contract and MUST NOT
be changed by this ADR.

## Coherence Domains And Projection

The live row is deliberately not a row-wide coherent snapshot. It consists of
five domains with different ownership:

1. Immutable identity: boot/stream ID, sanitized user/device/item ID, and media
   mode are written at registration and never changed. Transfer ID is late-bound
   exactly once after `BeginTransfer`.
2. Exact controller allocation: target, owned, and debt chunks are one exact
   packed allocation word projected while the controller lock is authoritative.
   The word is copied once; its fields are never assembled from separate
   controller reads. Private base and blocker timing are separate domains.
3. Timed lifecycle words: lifecycle, producer state, and consumer state are
   independent single-writer words containing the enum and its monotonic
   transition time. The producer writes producer state, the consumer writes
   consumer state, and the lifecycle owner writes lifecycle/completion state.
4. Eventual gauges: queued bytes, writing bytes, cumulative bytes, and transfer
   gauges are individually atomic. They may be observed at different moments and
   have no cross-domain row invariant.

5. Timed blocker word: the controller serializes one packed word containing only
   `allocation_blocker` and its monotonic transition milliseconds. One atomic
   store under the controller lock updates this word; one atomic load returns an
   internally coherent blocker and transition time. The allocation word and timed
   blocker word are independently coherent and may be observed eventually
   together. No timestamp is packed with all allocation values.

The sidecar MAY perform allocation-free atomic projection stores while holding
the authoritative controller or queue lock. It MUST categorically avoid any
observer/registry lock acquisition, callback, channel operation, allocation,
I/O, or wait while those locks are held. Sidecar scans and registry publication
occur only after controller and queue locks are released.

Only the controller allocation word is mutually coherent. A live row MUST NOT
claim that allocation, queue, transfer, lifecycle, and byte gauges were captured
atomically together. Admin and UI MUST present eventual domains as such through
freshness and stale states, never by inventing a cross-domain invariant.

## State Model

### Lifecycle

```text
starting, active, closing
```

A stream enters `closing` before source close, queue close, cancellation wakeup,
optional ownership cleanup, and producer join. Completion cannot be retained
until cleanup and unregister ordering below is complete.

### Producer

```text
idle, reading_base, reading_optional, waiting_for_buffer, done
```

`reading_base` is upstream read using the private 32 KiB base. `reading_optional`
is upstream read using an optional chunk. `waiting_for_buffer` means the producer
has requested an optional chunk and has not accepted one. No other producer state
is used for an outstanding wait.

### Consumer

```text
idle, waiting_for_data, writing, done
```

`waiting_for_data` means the consumer is waiting for a published producer event.
Queued bytes are an eventual byte gauge, not a consumer state.

### Allocation blocker

```text
none, pool_exhausted, at_target, debt
```

`allocation_blocker` is `none` unless the producer is waiting for an optional
buffer. `pool_exhausted` means no free chunk exists and allocating one would
exceed H. `at_target` means the request has reached its current target.
`debt` means the request owns more than target and cannot receive another chunk.
The controller remains authoritative for these classifications.

### Timed conditions

The sidecar derives independent monotonic intervals from the timed state words
and the timed blocker word:

| Condition | Exact predicate | Severity after threshold |
| --- | --- | --- |
| `buffer_acquire` | producer is `waiting_for_buffer` | informational at 2s; never a warning by itself |
| `pool_contention` | producer is `waiting_for_buffer` and blocker is `pool_exhausted` | warning at 2s |
| `consumer_starvation` | one uninterrupted `waiting_for_data` interval | warning at 2s |
| `upstream_stall` | producer is `reading_base` or `reading_optional` without read completion | warning at 10s |
| `downstream_stall` | consumer is `writing` without write completion | warning at 10s |
| `close_join_stall` | lifecycle is `closing` before producer join and cleanup completion | critical at 10s |

Each predicate starts when it first becomes true and resets when false. A
condition starts at the transition that made its predicate true; pool contention
starts at the later of the `waiting_for_buffer` and `pool_exhausted` transitions.
A counter update does not reset an unrelated condition. `consumer_starvation` is
specifically the uninterrupted `waiting_for_data` interval; queued bytes do not
start or clear it. `pool_contention` is specifically
`waiting_for_buffer && pool_exhausted`; `buffer_acquire` covers all optional
acquisition waits but is informational only. `at_target` and `debt` waits remain
informational unless another independent thresholded condition is active.

The selected current wait projection is the longest-running true condition.
Equal start times use this fixed priority:
`close_join_stall`, `downstream_stall`, `upstream_stall`, `pool_contention`,
`buffer_acquire`, `consumer_starvation`. `wait_condition` is `none` when no
condition is true. `wait_started_at` and `wait_duration_ms` describe only that
selected condition. All threshold-reached true conditions remain available in
`health_reasons` except the informational `buffer_acquire` condition, which is
never a health reason by itself.

## Health Model

Stream health is exactly:

```text
healthy, warning, critical
```

An enabled stream is `healthy` until one warning condition reaches its threshold.
Any sustained `close_join_stall` at 10 seconds makes it `critical`. No other
condition is critical. Positive debt, base-only operation, H at capacity, sticky
free memory, `buffer_acquire`, `at_target`, `debt`, and waits shorter than
threshold are informational. Only `pool_contention`, `consumer_starvation`,
`upstream_stall`, `downstream_stall`, and `close_join_stall` can change health.

Aggregate health is exactly:

```text
disabled, idle, healthy, warning, critical
```

`disabled` applies when buffering is disabled. `idle` applies when anchored
controller `N=0`. Otherwise aggregate health is the maximum severity within the
cycle's anchored observed subset: `critical` > `warning` > `healthy`. Aggregate
health reasons are the sorted unique warning/critical reason codes from that
subset; informational `buffer_acquire` is excluded. Warning and critical counts
use the same subset and are not inferred by the frontend.

When `N>0` and the anchored observed subset is empty, aggregate health is
`healthy`, meaning only that no observed warning or critical condition exists.
It MUST NOT be presented as proof that all N streams are healthy. In this case
`observation_completeness=limited` and `unobserved_active_requests=N` communicate
the unknown coverage.

## Aggregate Contract

The existing aggregate accounting remains:

```text
enabled
hard_budget_bytes = H
allocated_bytes   = P
owned_bytes       = O
free_bytes        = F
active_requests   = N
base_only_requests
indebted_requests
request_debt_bytes
```

The complete new aggregate media-buffer object MUST contain:

| Field | Type | Contract |
| --- | --- | --- |
| `enabled` | boolean | Existing enabled state. |
| `health` | enum | Aggregate health above. |
| `health_reasons` | array | Sorted unique finite reasons, maximum 8. |
| `hard_budget_bytes` | int64 | H, aligned to 32 KiB. |
| `allocated_bytes` | int64 | P, aligned and controller-coherent. |
| `owned_bytes` | int64 | O, controller-coherent. |
| `free_bytes` | int64 | F, controller-coherent. |
| `unallocated_optional_bytes` | int64 | H - P, controller-coherent. |
| `private_base_bytes` | int64 | N * 32768, outside H. |
| `queued_bytes` | int64 | Eventual sum over the anchored observed subset. |
| `writing_bytes` | int64 | Eventual sum over the anchored observed subset. |
| `active_requests` | int | N, controller-coherent. |
| `base_only_requests` | int | Cached controller count, O(1) snapshot. |
| `indebted_requests` | int | Cached controller count, O(1) snapshot. |
| `request_debt_bytes` | int64 | Cached controller sum, O(1) snapshot. |
| `buffer_acquire_count` | int | Anchored-subset streams whose acquire interval reached 2s. |
| `pool_contention_count` | int | Anchored-subset streams whose exact pool interval reached 2s. |
| `consumer_starvation_count` | int | Anchored-subset uninterrupted data waits at 2s. |
| `upstream_stall_count` | int | Anchored-subset upstream-read intervals at 10s. |
| `downstream_stall_count` | int | Anchored-subset downstream-write intervals at 10s. |
| `close_join_stall_count` | int | Anchored-subset closing intervals at 10s. |
| `warning_streams` | int | Anchored-subset stream warning count. |
| `critical_streams` | int | Anchored-subset stream critical count. |
| `completion_drops` | uint64 | Completions rejected by the bounded nonblocking path. |
| `observed_active_requests` | int | Size of the anchored observed subset. |
| `unobserved_active_requests` | int | N minus anchored observed subset size. |
| `live_registration_drops` | uint64 | Cumulative observation-registration drops for this boot. |
| `observation_completeness` | enum | `complete`, `limited`, or `unavailable`. |

For enabled controller composition, `P = O + F`, `0 <= O <= P <= H`, and
`P + unallocated = H`. Private bases, queued bytes, and writing bytes are not
added to H, P, O, F, or unallocated. Disabled status reports zero composition
and zero counts, `observed_active_requests=0`, `unobserved_active_requests=0`,
`live_registration_drops=0`, and `observation_completeness=unavailable`. An
enabled provider with no streams reports health `idle` and completeness
`complete`. For an enabled available provider, the coherent O(1) controller
snapshot's `active_requests=N` is the observation-cycle anchor. During the later
live-registry scan, nonterminal slots are processed in ascending raw stream ID;
only the first at most N nonterminal rows form the anchored observed subset.

The required formulas are exactly:

```text
observed_active_requests = min(N, number of scanned nonterminal rows selected for the anchored subset)
unobserved_active_requests = N - observed_active_requests
```

Selection itself guarantees `0 <= observed_active_requests <= N`; the
implementation MUST NOT apply a negative clamp after an invalid equation. Only
the anchored subset contributes queued/writing totals, every wait/stall count,
health/reasons, warning/critical counts, and every other eventual sidecar-derived
aggregate or series field. Terminal rows and nonterminal rows beyond N contribute
to none of those fields in the cycle.

`observation_completeness=complete` requires an available provider,
`unobserved_active_requests=0`, and no applicable current-cycle coverage
limitation. It is `limited` when the provider is available and either
`unobserved_active_requests>0` or another current-cycle coverage limitation
applies. Historical cumulative `live_registration_drops` alone MUST NOT
permanently force current completeness to `limited`. An absent provider is
`unavailable`.

The completeness status is independent of buffer health and carries no identity
or reason string. Drop counters and completeness reset with the Registry boot;
no cross-boot history or linkage is permitted. Counts and drops are privacy-safe
aggregate numbers only.

The controller fields and cached base-only, indebted, and debt counters are
updated during existing locked mutations. The controller aggregate snapshot
MUST read those cached values in O(1); it MUST NOT scan requests. The sidecar
scan occurs after controller unlock and supplies eventual sidecar totals from the
anchored subset for that observation cycle.

## Live Stream DTO

The active stream DTO MUST contain only these operator-actionable fields:

| Field | Type/nullability | Semantics |
| --- | --- | --- |
| `boot_id` | required string | Existing Registry boot ID. |
| `stream_id` | required unsigned decimal string | Controller request ID. |
| `transfer_id` | string or null | Directly captured transfer ID. |
| `user_id` | string or null | Sanitized known gateway user ID. |
| `username` | string or null | Sanitized known gateway username. |
| `device` | string or null | Sanitized known device. |
| `item_id` | string or null | Sanitized known Emby item ID. |
| `media_mode` | required enum | `direct`, `hls`, `range`, or `unknown`. |
| `state` | required enum | Lifecycle state. |
| `producer_state` | required enum | Producer state. |
| `consumer_state` | required enum | Consumer state. |
| `allocation_blocker` | required enum | Allocation blocker. |
| `target_bytes` | required int64 | Current optional target. |
| `owned_bytes` | required int64 | Current optional ownership. |
| `debt_bytes` | required int64 | `max(0, owned - target)`. |
| `private_base_bytes` | required int64 | 32768 while live, outside H. |
| `queued_bytes` | required int64 | Eventual currently queued bytes. |
| `writing_bytes` | required int64 | Eventual bytes in current downstream write. |
| `bytes_read` | required int64 | Cumulative logical upstream bytes. |
| `bytes_written` | required int64 | Cumulative logical downstream bytes. |
| `wait_condition` | required enum | Selected current timed condition or `none`. |
| `wait_started_at` | timestamp or null | Start of selected current condition. |
| `wait_duration_ms` | int64 | Duration of selected condition, zero for none. |
| `health` | enum | `healthy`, `warning`, or `critical`. |
| `health_reasons` | array | Sorted unique finite reasons, maximum 8. |
| `started_at` | timestamp | Registration time. |
| `age_ms` | int64 | Non-negative Registry-clock age. |

The live DTO MUST NOT contain rates, expected length, method, item name, queue
internals, request/grant flags, operation count fields, separate read/write
timestamps, or live peak fields. Peaks and accumulated or maximum waits are
completion-only fields. All numeric fields are non-negative and byte allocation
fields are aligned to 32 KiB where applicable.

## Completion Outcomes And Ordering

The completed summary contract is exactly:

| Field | Type/nullability | Semantics |
| --- | --- | --- |
| `boot_id` | required string | Existing Registry boot ID. |
| `stream_id` | required unsigned decimal string | Controller request ID. |
| `transfer_id` | string or null | Direct boot-scoped transfer ID. |
| `user_id`, `username`, `device` | string or null | Capture-sanitized identity. |
| `item_id` | string or null | Capture-sanitized Emby item ID. |
| `media_mode` | required enum | Final known media mode. |
| `final_state` | required enum | `closing`. |
| `final_producer_state` | required enum | `done` after producer join. |
| `final_consumer_state` | required enum | `done` after queue drain. |
| `final_allocation_blocker` | required enum | `none` after acquisition cleanup. |
| `outcome` | required enum | Finite outcome below. |
| `started_at`, `completed_at` | required timestamps | Registry-clock bounds. |
| `duration_ms` | required int64 | Non-negative completion duration. |
| `bytes_read`, `bytes_written` | required int64 | Final cumulative counters. |
| `peak_owned_bytes` | required int64 | Maximum optional ownership. |
| `peak_debt_bytes` | required int64 | Maximum optional debt. |
| `peak_queued_bytes` | required int64 | Maximum eventual queued bytes. |
| `peak_writing_bytes` | required int64 | Maximum eventual writing bytes. |
| `waits_ms` | required fixed object | Per-condition `total` and `max` durations. |
| `invariant_observed` | required boolean | A secondary cleanup invariant was observed. |

`waits_ms` has exactly the keys `buffer_acquire`, `pool_contention`,
`consumer_starvation`, `upstream_stall`, `downstream_stall`, and
`close_join_stall`; each value has non-negative int64 `total` and `max` fields.
Item name is never retained. No raw error, path, chunk, session, or sample list is
retained.

The finite outcomes are:

```text
success, canceled, upstream_error, downstream_error, short_write,
length_mismatch, invalid_read, invalid_write, no_progress, invariant_error
```

The copy path MUST use typed sentinels `ErrInvalidMediaRead` and
`ErrInvalidMediaWrite` for invalid reader/writer counts. The engine retains the
ADR 0002 primary selected result and direction separately from a secondary
`invariant_observed` cleanup observation. Source Close errors remain outside the
existing copy result and MUST NOT create a new outcome. Outcome classification is exact:

1. A selected downstream result classifies invalid count as `invalid_write`, short write
   to `short_write`, and any other selected downstream error to `downstream_error`.
2. A selected context cancellation is `canceled`.
3. A selected upstream result classifies the existing length sentinel as
   `length_mismatch`, invalid count to `invalid_read`, no-progress to
   `no_progress`, and any other selected upstream error to `upstream_error`.
4. `invariant_error` is used only when no primary selected result exists and a
   cleanup, ownership, queue, producer-join, or request-unregister invariant was
   observed.
5. No primary result and no invariant observation produces `success`.

Cleanup MUST never replace, override, or reclassify a primary selected result.
The existing selected direction and result precedence remain authoritative. The
bounded `invariant_observed` flag exposes the secondary observation without
changing the outcome.

Completion ordering MUST be:

1. Set lifecycle to `closing` and retain identity, selected result, peaks, and
   accumulated timed-condition state while those domains are still available.
2. Close the copy queue, cancel optional acquisition, close the source exactly
   once, wait for producer join, drain queued ownership, and finish copy cleanup.
3. Capture final byte counters, terminal producer/consumer/allocation state, and
   completed wait totals and the secondary invariant flag into the fixed bounded
   summary, atomically mark the slot terminal, then unregister the controller
   request. The terminal mark is a projection store, not a membership operation.
4. Call `TransferHandle.End` exactly once, removing the active transfer entry.
5. Offer the captured bounded summary to the completion path using one
   nonblocking operation. If full, increment `completion_drops` atomically and
   discard the summary. Never wait, allocate unboundedly, or delay media-gate
   release for completion publication.
6. Perform the existing audit, then return or panic according to the existing
   copy path. The offer MUST occur before audit and before return/panic.

The media gate MUST always be released by the existing copy cleanup path even if
the completion ring is full or unavailable. Completion retention is in memory,
bounded to 2,048 entries or 15 minutes, whichever is reached first, and resets
on boot. Eviction is deterministic by completion sequence.

Registration and the terminal nonblocking completion offer are the only two
request-lifecycle observer interactions on the request path. Neither appears in
the 32 KiB copy loop. Terminal atomic state plus asynchronous sampler removal
keeps active membership correct even when the completion offer drops.

## Sampling, Gauge Ring, And History

At most one process-wide media-buffer gauge sampler MAY exist. It is lifecycle
owned by the existing telemetry Registry and MUST stop with that Registry. It
uses one injected monotonic one-second cadence; late ticks coalesce and never
launch catch-up work.

The media-buffer gauge ring is a dedicated sample path. It MUST execute and write
one gauge sample even when byte and error deltas are zero. It MUST NOT be gated
by the existing ByteMeter traffic-delta fast path. A cycle is:

1. Under the controller lock, copy one exact allocation word and O(1) cached
   aggregate counters, including `active_requests=N` as the cycle anchor; no
   Registry/observer lock, callback, channel operation, allocation, I/O, or wait
   is permitted there.
2. Release the controller lock.
3. Acquire the dedicated live-registry read lock and scan the fixed-cap slots in
   ascending raw stream ID without allocation. Skip terminal slots. Select only
   the first at most N nonterminal rows as the anchored observed subset and derive
   eventual queue, writing, state, health, reason, and condition fields only from
   that subset. Nonterminal rows beyond N remain available to list/detail but are
   excluded from this cycle's aggregate. Release the lock before Registry
   publication. Terminal rows are removed later by maintenance and do not
   invalidate the scan.
4. Commit one bounded aggregate gauge sample and update the dedicated ring. This
   commit is the cycle completion point.

The controller aggregate is mutually coherent for H, P, O, F, unallocated,
active requests, base-only, indebted, and debt. Sidecar totals in the same cycle
are eventual and MUST NOT be presented as a cross-domain atomic row. A cycle is
complete only after one successful O(1) controller snapshot and one successful
active-registry scan have both been committed. A bucket is `present=false` when
the provider is absent/unavailable, a sampler cycle is canceled before commit, or
no cycle runs and commits in that bucket because a tick is late/coalesced.
Ordinary registration, completion, terminal marking, removal, compaction, and
eventual sidecar state changes MUST NOT create a gap. A late or coalesced tick
commits one complete cycle to the bucket in which it actually runs; it does not
replay missed cycles or synthesize a value. Every skipped bucket with no committed
cycle is `present=false`. Lateness never carries data into a skipped bucket.

Registration or completion between the controller snapshot and live scan is an
expected eventual-domain race. It MUST NOT trigger a cross-lock retry, joint
controller/live lock, lifecycle seqlock, whole-scan population assertion, or
post-hoc negative clamp. The N anchor and first-N selection make observed and
unobserved counts exact and non-negative for that cycle, which remains complete
and `present=true` when committed.

If `N=0`, newer live rows appended before the scan are excluded and the cycle
reports observed=0, unobserved=0, and health `idle`; those rows may contribute on
the next cycle. If `N>0` and no nonterminal row is selected, the cycle reports
observed=0, unobserved=N, health `healthy` for the empty observed subset, and
completeness `limited`. Rows beyond N remain immediately visible to list/detail
and first contribute to aggregate/history on a later cycle whose anchor selects
them.

History uses existing 15m, 1h, 6h, and 24h selectors with respectively 900
one-second, 60 one-minute, 360 one-minute, and 1,440 one-minute points. Timestamps
are bucket starts, points are oldest first, and a point is emitted only from the
latest complete gauge cycle in that bucket. A bucket without a complete cycle is
an explicit `present=false` gap. There is no carry-forward.

Each historical point has exactly this shape:

```text
t: RFC3339Nano UTC bucket-start timestamp
present: boolean
domains: { pool: "coherent", sidecar: "eventual" } or null for a gap
aggregate: exact aggregate media-buffer object or null for a gap
```

For `present=true`, `aggregate` includes health, health reasons, queued/writing,
every sustained-condition count, warning/critical counts, completion drops,
observed/unobserved active counts, live registration drops, and observation
completeness. Terminal rows are excluded from all series live totals. Limited
observation remains `present=true` with completeness `limited`; unavailable
provider state is represented by the permitted gap.
Only its controller pool composition is mutually coherent; sidecar totals are
eventual values from the latest complete cycle's anchored subset. For
`present=false`, `domains`
and `aggregate` are null and no zero value is presented as observed data.

## API And Error Contract

All new routes remain under the existing superuser-authenticated Admin API and
retain session, CSRF, same-origin, and rate-limit behavior.

### Existing aggregate routes

`/admin/api/v1/overview` and `/admin/api/v1/metrics/stream` remain aggregate-only.
They MUST use the exact aggregate contract above and MUST NOT contain stream IDs,
labels, completions, or per-stream history.

### Active stream list

```text
GET /admin/api/v1/media-buffer/streams?cursor=<opaque>&limit=50
```

The active registry membership is maintained in ascending raw `stream_id` order.
A cursor contains the boot ID and the last examined unsigned raw membership ID;
it is never the last emitted active ID. The server MUST binary-search for the
first raw ID strictly greater than the cursor. It MUST examine at most `limit`
raw slots, return only nonterminal rows among those slots, and MAY examine at
most one additional raw slot solely to establish `has_more`. It MUST NOT copy and
sort the whole membership, scan prior pages, or retry by scanning. Default limit
is 50; maximum is 200. The response contains `boot_id`, `items`, `next_cursor`,
`has_more`, and `observation_completeness`. IDs are unique and strictly
ascending. The response's completeness status is aggregate observation state;
it does not imply that every returned row was captured at one instant. Page work
is O(log N + limit + 1).

`next_cursor` MUST contain the last raw ID consumed among the at-most-`limit`
examined slots, including a terminal slot. The optional one-slot lookahead is not
part of the examined page window, is not consumed, and does not advance the
cursor; it only establishes `has_more`. An
empty page with `has_more=true` is valid when the consumed slots are terminal and
is forward-progressing. The next request starts strictly after that raw cursor.
Stable compaction/removal between pages is safe because IDs are monotonic and the
search remains strictly greater than the raw cursor.

Malformed cursors return HTTP 400 with `invalid_cursor`. A cursor for another
boot returns HTTP 409 with `stale_cursor`. Disabled buffering returns HTTP 200
with an empty bounded result and `observation_completeness=unavailable`. An
enabled but absent provider returns HTTP 503 with bounded code
`provider_unavailable`. A limited observation result remains HTTP 200 and carries
`observation_completeness=limited`; the UI MUST present it as incomplete rather
than implying complete stream coverage.

List/detail operate on current nonterminal slots, not the prior aggregate anchor.
They MAY therefore expose rows beyond the latest cycle's N-anchored subset. This
is expected eventual behavior and MUST NOT trigger a retry or cause Admin to add
those rows into the already-committed aggregate.

### Direct stream detail

```text
GET /admin/api/v1/media-buffer/streams/{stream_id}?boot_id=<boot>
```

Under the live-registry read lock, the route binary-searches the ordered slot
slice by raw stream ID, copies at most one pointer, and unlocks before row reads.
Detail work is O(log 4096). It returns one bounded nonterminal live row and the
current boot ID. Disabled buffering returns HTTP 200 with an empty detail object.
A provider that should be
present but is absent returns HTTP 503 `provider_unavailable`. A boot mismatch
returns HTTP 409 `stale_boot`. A valid current boot with a missing or completed
stream returns HTTP 404 `stream_not_found`. Invalid unsigned IDs return HTTP 400
`invalid_stream_id`. The route is the only Activity deep-link target; it never
performs an inferred join.

### Series and recent completions

```text
GET /admin/api/v1/media-buffer/series?window=15m|1h|6h|24h
GET /admin/api/v1/media-buffer/recent?limit=50
```

Unknown or absent window uses the existing 15m default. Series returns boot ID,
normalized window, interval, and bounded points. Recent defaults to 50 and caps
at 200, ordered by descending completion sequence. Both use the provider status
rules above, including disabled 200 empty and absent-provider 503.

### Transfer linkage

The existing active Transfer DTO adds only nullable `media_buffer: {boot_id,
stream_id}`. Admin obtains transfer and stream details through independent
bounded copies and enriches the Activity view after releasing all source locks.
The transfer DTO does not expose buffer allocation, health, user, item, or queue
fields. A missing current stream renders stale/complete rather than causing a
client retry or inferred match.

All new responses MUST have encoded-size tests for default and maximum payloads,
including sanitized maximum-length values and maximum row/point counts.

## Admin Information Architecture And Visual Intent

The navigation order remains:

```text
Overview | Users | Activity | Buffer | Traffic | System
```

Overview MUST retain a compact Buffer sentinel with aggregate health,
pool-composition summary, observation completeness, and a Buffer link. It MUST
not expose identities or duplicate detailed charts.

Buffer MUST be a dedicated read-only operational page containing aggregate
health/reasons, optional-pool composition, separate private-base composition,
queued/writing eventual totals with freshness, coherent pool history, all
sustained-condition counts, warning/critical counts, completion drops, Active
Streams and Recent Completions tabs, and direct Activity links. It MUST show
observation completeness, observed/unobserved active counts, and cumulative live
registration drops without exposing unobserved stream identity. The active table
shows only the live DTO fields. Completion rows show outcome, final counters,
peaks, waits, and direct identity.

The exact visual hierarchy, semantic state labels, and direct-link behavior are
normative. Exact radius values, chart primitives, hover implementation,
disclosure mechanism, and low-level component choices are Designer-owned
`SHOULD` guidance. The page SHOULD retain the current dark operational language,
compact density, and restrained surfaces. It MUST NOT add runtime controls,
marketing composition, or a new visual framework.

The UI MUST distinguish disabled, loading, empty, stale, partial API failure,
limited observation, unavailable observation, error, and permission-denied
states. `limited` MUST be presented as incomplete coverage independently of
buffer health. In particular, `health=healthy` with `N>0` and
`observed_active_requests=0` means no warning/critical state was observed; it MUST
NOT claim all N streams are healthy. A cumulative registration-drop count with
current completeness `complete` is historical context, not a current incomplete
state. Limited observation MUST NOT look like a healthy complete stream list. It MUST
remain usable at 390x844 without page-level
horizontal overflow; bounded table regions MAY scroll. Semantic headings,
accessible names, keyboard focus, visible focus, and non-color-only health
indicators are required. Raw errors and stale identity MUST never be shown as
current. Configuration remains read-only.

## Failure Isolation And Compatibility

Sidecar, Registry, sampler, gauge ring, completion ring, provider, or new route
failure MUST NOT fail a media copy, change its selected result, alter allocation
or fairness, delay source cleanup, or prevent media-gate release. Existing
aggregate routes continue to work with disabled state and stable zero values.

The feature is additive and in memory. No migration, persistence change,
deployment data migration, public runtime control, or ADR 0002 path change is
required. Existing clients that ignore new fields remain compatible.

## Performance And Verification Budgets

This is the single normative performance section. Hard gates are:

- zero allocation delta in observed controller/copy operations;
- no new goroutine per stream and no observer-created goroutine on the media path;
- no observer, Registry, ByteMeter, or sidecar-registry lock acquisition while
  authoritative controller or queue locks are held;
- no live-registry operation in the 32 KiB copy loop; pre-I/O registration and
  the post-End terminal nonblocking offer are the only request-path exceptions;
- exactly one O(1) controller snapshot and one fixed-cap ascending slot scan per
  gauge cycle, with no cross-lock retry, joint lock, seqlock, or corrective clamp;
- no observer channel send, callback, I/O, wait, or unbounded value creation in
  read, write, grant, release, queue, or controller operations; the sole terminal
  exception is the bounded nonblocking completion offer after transfer end; and
- O(1) controller aggregate snapshot with cached base-only, indebted, and debt
  counters.

The benchmark MUST use a fixed media payload, fixed GOMAXPROCS, the same host and
toolchain for both observation-absent and observation-present runs, at least 20
samples per comparison, and `benchstat` analysis. The target is `<5%` median
media-copy regression. A statistically significant regression `>10%` is a hard
rollout blocker. The benchmark MUST include controller
aggregate snapshot and lock behavior at N=1, 64, and 512 requests and prove O(1)
behavior. It MUST separately measure live-registry registration write-lock,
page/detail read-lock, sampler scan read-lock, and terminal removal/compaction
hold and wait time under N=512 registration/API/sampler/removal contention.
Sampler hold time MUST remain bounded at N=512. Allocation delta, observer lock
acquisition in forbidden paths, and new goroutine growth are hard failures
regardless of median. Fixed-cap N=4,096 benchmarks MUST additionally measure
Registry construction preallocation footprint, slot backing-pointer/capacity
stability, strict guard-before-append and cap/drop behavior, registration
write-lock wait, sampler RLock scan, full stable-compaction write-lock hold and
backing-slice reuse, registration wait during maintenance, binary-search API
page/detail latency, terminal exclusion, and media-copy latency while maintenance
runs. N=512 remains an intermediate contention case, not the active-cap limit.

## Considered And Rejected Alternatives

### Infer joins by session, time, or path

Rejected. Concurrent streams, retries, reconnects, and restart make those values
non-identities. Direct boot-scoped controller/transfer identity is deterministic.

### Put stream labels in Overview or SSE

Rejected. It changes the bounded aggregate contract and creates high-cardinality
payload growth. Stream data belongs in bounded list/detail routes.

### Copy and sort the whole active registry for every page

Rejected. It makes request work O(N). Ordered membership, binary-search cursor,
an at-most-`limit` raw-slot window, and one optional lookahead make pagination
bounded and forward-progressing across tombstones.

### Grow observation storage or compact during registration

Rejected. Request-path growth and O(N) compaction make latency depend on active
history. The fixed 4,096-slot preallocation, nonblocking registration drop, and
sampler-owned stable compaction make the cost and incomplete state explicit.

### Cursor by last emitted active row

Rejected. Terminal-only windows could fail to advance. The raw last-examined ID
cursor advances across tombstones and remains valid after stable compaction.

### Retry for cross-domain population agreement

Rejected. Registration and completion legitimately occur after the O(1)
controller snapshot. A retry, joint controller/live lock, lifecycle seqlock, or
corrective clamp would couple domains or hide an invalid equation. Anchoring to N
and selecting the first at most N nonterminal raw-ID-ordered rows gives bounded,
non-negative cycle counts while preserving eventual convergence.

### Give every stream a historical series or emit per-chunk events

Rejected. Both multiply memory and event cardinality. Finite state, gauges,
bounded completions, and aggregate gauge history are sufficient.

### Row-wide seqlock or generic observer callbacks

Rejected. Producer, consumer, controller, queue, and transfer writers have
different ownership and timing. Domain-specific words and atomic eventual gauges
avoid a multi-writer row lock, allocation, callback, and feedback chain.

### Let the existing byte-delta sampler own media-buffer gauges

Rejected. A zero byte/error interval is still an operational media-buffer state.
The dedicated gauge ring must sample every cadence.

### Infer health or join data in the frontend

Rejected. Poll timing, dropped updates, and independent registries make inference
incorrect. Backend state and direct identity are authoritative.

### Expose raw errors or runtime UI configuration

Rejected for privacy, cardinality, and control-feedback reasons. Finite outcomes,
reasons, and read-only configuration are sufficient.

### Rewrite ADR 0002 history

Rejected. This ADR has a narrow supersession boundary and leaves accepted
buffering behavior unchanged.

## Phased Implementation And Ownership

### Phase 0: ADR and Oracle gate

Owner: Fixer. This ADR is the only Phase 0 artifact. Follow-up Oracle MUST
confirm state semantics, domains, identity, completion ordering, bounds, privacy,
API errors, UI intent, and evidence before implementation.

### Phase 1: Core projection and identity

Owner: Fixer. Add the concrete sidecar, exact allocation and timed-blocker words,
cached O(1) controller counters, state/condition words, dedicated live-registry
lock/membership with one preallocated 4,096-slot slice, nonblocking registration drops,
Registry boot accessor, capture-time sanitation, late TransferMeta identity
binding, typed sentinels, and allocation/lock benchmarks.
The existing `TransferHandle.ID()` accessor is reused; only `Registry.BootID()`
and the typed invalid read/write sentinels are new Phase 1 symbols. No Admin API
or frontend changes.

### Phase 2: Registry, gauge ring, completion, and APIs

Owner: Fixer. Add the dedicated gauge ring, complete-cycle scan, terminal
membership exclusion/stable compaction, bounded completion path and drops,
observation completeness/count/drop aggregates, completion precedence, health
evaluation, N-anchored observed-subset selection, history, privacy-defense,
raw-cursor list/detail pagination, status errors, transfer identity linkage, and
backend tests.

### Phase 3: Admin UI

Owner: Designer. Implement the Overview sentinel, Buffer page, charts, active and
completion views, Activity detail links, responsive states, accessibility, E2E,
and embedded assets. Fixer may perform only mechanical integration preserving
accepted design.

### Phase 4: Integrated evidence

Owners: Fast-generic for command validation, Fixer for synthetic backend smoke,
Designer for visual remediation. Run backend/frontend validation, race tests,
privacy and encoded-size checks, benchmarks, Playwright coverage, and temporary
local smoke. No real or live Emby credentials are used.

## Deterministic Acceptance Matrix

Implementation MUST be accepted only with this evidence.

### State and projection

- producer barriers prove `idle`, `reading_base`, `reading_optional`,
  `waiting_for_buffer`, and `done`;
- consumer barriers prove `idle`, `waiting_for_data`, `writing`, and `done`;
- lifecycle barriers prove `starting`, `active`, `closing` and cleanup ordering;
- allocation blocker barriers prove `none`, `pool_exhausted`, `at_target`, and
  `debt` with pool contention only at the exact required predicate;
- consumer starvation requires one uninterrupted `waiting_for_data` interval;
- fake-clock barriers prove `buffer_acquire`, `at_target`, and `debt` waits remain
  informational and never alert without an independent thresholded condition;
- atomic race tests prove one allocation-word load coherently returns
  target/owned/debt chunks and one timed-blocker-word load coherently returns
  blocker/transition milliseconds, with no cross-word invariant;
- no obsolete private-base or queue-as-state value is emitted;
- registration beyond 4,096 observed slots drops observation without changing
  controller allocation, preserves the internal stream ID, and leaves Transfer
  linkage null;
- domain tests show exact allocation composition and eventual sidecar gauges have
  no cross-domain row invariant.

### Identity and completion

- Registry boot ID is reused and stable for one process and changes on restart;
- `TransferHandle.ID()` is verified as an existing reused symbol; only
  `Registry.BootID()` and typed invalid read/write sentinels are introduced in
  Phase 1;
- unsigned controller and transfer IDs are monotonic base-10 strings;
- registration captures all immutable identity except transfer ID; after the
  existing `BeginTransfer` point, one zero-to-ID bind completes before counted
  I/O and header commitment;
- table tests prove selected downstream, selected cancellation, selected upstream,
  no-primary invariant, and success outcome precedence, including typed
  invalid/short/length/no-progress cases;
- a simultaneous primary result and cleanup invariant preserves the primary
  outcome and sets `invariant_observed=true`;
- source Close errors remain outside outcome classification;
- completion cleans up/joins, captures terminal/peak state, unregisters, calls
  End, offers nonblocking completion, audits, returns/panics, and then releases
  the media gate by the existing defer;
- terminal marking occurs before unregister so terminal rows are excluded while
  controller/observed active counts remain bounded and consistent;
- ordering barriers prove the offer occurs before audit and before return/panic;
- full completion storage increments `completion_drops` without delaying media
  gate release, and terminal sampler removal prevents an active-membership leak;
- terminal completion/drop races leave no active membership leak;
- no completion exposes a separate join outcome, raw errors, or session fields.

### Health and history

- fake-clock tests cover all independent condition starts/resets, threshold
  equality, warning/critical severity, simultaneous conditions, and maximum
  severity aggregate projection;
- stream health and aggregate health emit exactly the enumerated values;
- dedicated gauge samples are written when byte/error deltas are zero;
- cached controller aggregate snapshot is O(1) at N=1,64,512;
- aggregate observation reports `complete`, `limited`, and `unavailable` exactly,
  with observed/unobserved counts and reset-on-boot `live_registration_drops`;
- formulas are exactly `observed=min(N, selected nonterminal rows)` and
  `unobserved=N-observed`, with selection enforcing non-negative values and no
  corrective clamp;
- register-between-scans and completion-between-scans barriers commit a present
  cycle without retry, joint locks, seqlocks, or whole-scan population equality;
- an `N=0` barrier with a newly appended row reports observed=0, unobserved=0,
  health `idle`, and excludes that row until a later cycle;
- an `N>0` barrier with zero selected rows reports observed=0, unobserved=N,
  health `healthy` for the observed subset, and completeness `limited` without
  claiming all streams healthy;
- extra nonterminal rows beyond N are absent from every current eventual
  aggregate/series field while remaining immediately visible to list/detail;
- the next anchored cycle converges when those rows fall within its selected
  first-N subset;
- cumulative registration drops alone do not force current completeness limited
  when current unobserved=0 and no current coverage limitation exists;
- history timestamps are bucket starts, oldest first, latest complete cycle only,
  with explicit gaps and no carry-forward;
- successful ordinary registration, terminal mutation, removal, compaction, and
  eventual sidecar mutation never create `present=false`; gaps are limited to an
  absent/unavailable provider, pre-commit cycle cancellation, or a skipped bucket
  with no committed late/coalesced cycle;
- terminal rows are excluded from every live total, condition/health count,
  warning/critical count, observed-active count, and series live total;
- terminal rows observed during a scan are removed later without false critical
  health, and late/coalesced ticks commit only to their actual bucket; skipped
  buckets with no committed cycle are explicit gaps with no carry;
- pool sums always hold for coherent controller fields, while eventual sidecar
  totals are explicitly marked eventual;
- points include all health/reason/count/completion-drop and queued/writing fields.

### Bounds, APIs, privacy

- Registry construction preallocates exactly one 4,096-capacity slot slice;
  registration never grows storage, compacts, or performs O(N) work;
- construction, guarded append, and full stable-compaction tests prove the slice
  backing pointer and capacity never change;
- registration proves `len(slots) < 4096` before one monotonic append under the
  dedicated live-registry write lock with no controller/queue/ByteMeter lock or
  I/O; no append path bypasses the guard and length never exceeds 4,096;
- full-cap and unreclaimed-tombstone registration drops are nonblocking, counted,
  privacy-safe, and leave media plus Transfer linkage unchanged;
- list pagination read-locks only through slice binary search and at most `limit`
  raw slot copies plus one lookahead check; detail read-locks only for slice
  binary search and one pointer copy;
- complexity evidence proves detail O(log 4096) and page
  O(log N + limit + 1) without linear detail scans;
- raw cursor progression crosses tombstone-only empty pages and remains safe after
  stable compaction; terminal/missing detail is 404 and old boot is 409;
- row reads and serialization occur after unlock, and no page-scanning retry is
  used for stale terminal rows;
- sampler scans pointers without allocation under read lock, while asynchronous
  terminal removal/compaction is the only O(N) write-lock path and is bounded by
  the fixed 4,096 slots;
- N=512 intermediate contention tests cover registration, page, detail, sampler,
  terminal removal, bounded lock hold/wait, and completion drop without active
  leak;
- N=4,096 tests cover preallocation footprint, cap/drop behavior, sampler RLock
  scan, full stable compaction write-lock hold, registration wait during
  maintenance, backing-slice reuse, page/detail binary-search latency, terminal
  exclusion, and media-copy latency during maintenance;
- default/max limits, ordered identity, malformed cursor 400, old boot 409,
  missing/completed detail 404, disabled 200 empty, and absent-provider 503 are
  tested;
- direct detail is bounded and is the Activity deep-link target;
- capture-time sanitation is tested before sidecar/history retention and Admin
  defense is tested again;
- all new DTOs omit forbidden privacy fields and existing Transfer session fields
  remain unchanged;
- encoded response-size tests pass at maximum rows, points, and string bounds;
- no list/detail route performs inferred joins or page-scanning retries.

### UI and release

- Overview sentinel links to Buffer and remains aggregate-only;
- Buffer shows coherent pool composition, eventual sidecar totals, all required
  health/reason/drop/count fields, active state, completions, and direct links;
- disabled, loading, empty, stale, partial API failure, limited/incomplete
  observation, unavailable observation, error, and denied states are tested;
- 390x844 and desktop/tablet Playwright checks cover overflow, focus, semantics,
  keyboard access, non-color health, and no overlap;
- observation-present/absent fixed-payload benchmarks use fixed GOMAXPROCS, one
  host, at least 20 samples, benchstat, and N=1/64/512 O(1) checks;
- N=512 live-registry benchmarks measure registration, page/detail, sampler, and
  terminal removal lock hold/wait under contention;
- N=4,096 fixed-cap benchmarks measure preallocation, registration/drop,
  backing-pointer/capacity stability, guarded append, sampler scan, full
  compaction reuse, binary-search page/detail, terminal exclusion, and media-copy
  latency during maintenance;
- zero allocation delta, no observer lock, and no per-stream goroutine are hard
  gates; statistically significant regression above 10% blocks rollout;
- race tests cover grant, release, queue publication, data wait, writing,
  cancellation, close, unregister, End, audit, completion drops, and gate release;
- synthetic smoke exercises concurrent streams, every state barrier, aggregate
  composition, eventual gauges, completions, drops, errors, disabled mode, and
  cleanup without real credentials.

## Rollout And Rollback

The implementation remains behind existing disabled-by-default buffering
configuration. Rollout begins with backend contracts and tests, then Designer UI,
then integrated evidence, followed by a bounded trusted-host canary. Any
allocation, lock, privacy, identity, cardinality, composition, completion-drop,
API-status, or UI-consistency failure blocks rollout. Restarting with the existing
enabled control set to false restores ADR 0002 behavior; observability MUST never
prevent that rollback.

## Oracle Questions

Follow-up Oracle MUST explicitly confirm:

1. The narrow supersession boundary preserves all ADR 0002 mechanics.
2. The producer/consumer barriers, allocation blockers, timed conditions, and
   severity projections match the current copy flow.
3. Domain coherence and atomic projection rules avoid observer lock acquisition
   and row-wide seqlock assumptions.
4. Cached O(1) controller aggregates and the dedicated zero-delta gauge ring are
   implementable without delaying the media gate.
5. Registry boot identity, unsigned ID encoding, TransferMeta binding, independent
   Admin enrichment, and completion ordering are sufficient.
6. Bounded pagination, detail/error statuses, capture-time privacy, response-size
   evidence, and completion drops meet operational bounds.
7. The reduced live DTO, complete aggregate/history contracts, UI requirements,
   performance methodology, and acceptance matrix are complete enough to start
   Phase 1.
8. The dedicated live-registry lock protocol, late transfer bind, secondary
   invariant flag, atomic timed-blocker word, complete-cycle gap rule, and
   downstream-before-audit completion order resolve all Gate 1 follow-up findings.
9. The single preallocated 4,096-slot slice, guarded append, completeness/drop
   contract, terminal compaction reuse, binary-search list/detail, raw tombstone
   cursor, late-tick gaps, and source-symbol expectations resolve all Gate 1 Third
   Review findings.
10. The coherent controller N anchor, first-N ascending nonterminal subset,
    exact observed/unobserved formulas, subset-only eventual fields, limited
    empty-observation health semantics, and next-cycle convergence resolve the
    final watch-point without coupling controller and live-registry locks.
