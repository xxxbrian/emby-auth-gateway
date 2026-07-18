# ADR 0002: Adaptive Media Buffering

## Status

Proposed

## Purpose

The gateway MUST provide preconfigured buffering that bridges bursty upstream
delivery without changing existing media-copy correctness. The design targets a
small trusted gateway. It does not pace output, repair packet loss, guarantee
downstream bandwidth, or impose user, session, device, or identity quotas.

This ADR is the normative v1 specification. MUST, MUST NOT, SHOULD, and MAY have
their RFC 2119 meanings.

## Modes

The product has two distinct modes:

- **Disabled mode** MUST call the existing synchronous `copyMediaBody` unchanged.
  It MUST NOT initialize or depend on the optional pool, controller, producer
  goroutine, or buffered engine. This is the immediate operational rollback.
- **Enabled mode** MUST use the buffered engine for eligible proxied media
  response bodies.

Call sites that provide only an `io.Reader`, or cannot provide an underlying
closer whose Close reliably unblocks Read, MUST remain on `copyMediaBody` in v1.
The current special plain-reader paths therefore remain synchronous even when
buffering is enabled.

The public configuration surface has exactly two controls: enable/disable and an
optional global pooled-memory budget override. There are no per-stream, queue,
scheduler, retention, or admission settings.

Disabled mode ignores budget discovery and validation. When enabled, budget
validation MUST finish before the server serves requests. Invalid or overflowing
explicit values and automatic values below one optional chunk MUST fail startup;
enabled mode MUST NOT silently switch to disabled mode after startup.

## Fixed optional budget

All sizes are integer bytes. Define:

```text
C        = 32 KiB
align(x) = floor(x / C) * C
S        = 512 MiB
```

C is both the optional allocation unit and the read quantum. S is a fixed
internal optional-buffer cap per request. A 512 MiB cap is large enough to retain
substantial upstream bursts while preventing one request from targeting an
unbounded portion of a large pool.

At startup, discover finite positive memory constraints:

```text
L = min(finite cgroup memory limit,
        finite GOMEMLIMIT,
        physical memory)
H_auto = align(min(2 GiB, floor(L / 8)))
```

The implementation MUST inspect cgroup v2 `memory.max`, cgroup v1 hard limits,
finite `GOMEMLIMIT`, and physical memory. Unlimited sentinels and `max` are not
finite candidates. Failure to discover one source omits only that source. If no
source is available while enabled, startup MUST fail.

One eighth of the tightest limit conservatively leaves memory for the Go runtime,
PocketBase, SQLite, HTTP state, stacks, private bases, and colocated cgroup use.
The 2 GiB cap matches the intended small deployment and avoids treating all host
memory as a prefetch objective.

An explicit override replaces `H_auto`. It MUST parse as a positive byte size and
is aligned down to C. Validation requires `H >= C`. H is fixed until restart.
V1 performs no runtime pressure sampling, budget resizing, or decommit.

H covers only optional pooled chunk backing slices. It excludes each request's
private base, socket buffers, goroutine stack, request state, queue descriptors,
and unrelated heap. Those unavoidable per-request and transport resources are
bounded operationally by HTTP/server connection limits and host capacity, not H.

## Private base

Every enabled request allocates one private 32 KiB base buffer with ordinary Go
allocation before acquiring the media gate. The base is outside H, never enters
the optional pool, and is never shared with another request.

There is no base admission, reserve, FIFO waiter, pre-header memory wait, or base
wait telemetry. A request can immediately read and write base-only. If its base
is queued or being written, its producer may request an optional chunk; if none
is available, it waits for its own base to return.

No admission reserve is needed. Immediate private bases guarantee progress, H is
fully protected by optional-pool predicates, and immediate equal-share target
shrink prevents newcomers from inheriting unlimited optional demand. Requests
above a reduced target drain locally without eviction.

## Pool accounting

Under the optional controller lock, define:

```text
P    = bytes in allocated optional chunk backing slices
O    = bytes in optional chunks owned by active requests
F    = bytes in free optional chunks
N    = active registered buffered requests
p_i  = optional bytes owned by request i
t_i  = optional target of request i
d_i  = max(0, p_i - t_i)             request-local debt
```

After every atomic controller operation:

```text
P = O + F
0 <= O <= P <= H
0 <= F <= P
sum(p_i) = O
```

The pool is lazy and sticky. When no free chunk exists and `P + C <= H`, a new
C-byte backing slice is allocated, P increases by C, and ownership is assigned.
P never decreases during process lifetime. Every released optional chunk becomes
free, increasing F and decreasing O by C; its backing slice is never dropped in
v1. Consequently `P <= H` truthfully bounds optional backing slices without
depending on garbage collection or retired-byte accounting.

There is no global budget debt in v1 because H is immutable and allocation is
checked before P increases. `P > H`, negative counters, or `P != O + F` is an
invariant failure, not a recoverable operating state.

## Targets and fairness

Register each enabled request after acquiring the media gate and before starting
telemetry or committing headers. Recompute every target immediately under the
controller lock on registration and unregister:

```text
if N == 0: no targets
if N > 0:  t_i = min(S, align(floor(H / N)))
d_i = max(0, p_i - t_i)
```

The aligned equal share intentionally leaves a remainder smaller than `N*C`
untargeted. A target is an optional occupancy ceiling, not a reservation or
prefetch requirement. There is no bitrate input, water filling, weighting,
membership hysteresis, or growth credit.

The exact optional-acquisition predicate for request i is:

```text
optionalCanAcquire(i) =
    requesting_i &&
    !closing_i &&
    d_i == 0 &&
    p_i + C <= t_i &&
    (F >= C || P + C <= H)
```

The controller maintains requesting producers in a rotating ring. One turn
examines each entry at most once and grants at most one chunk per eligible
request. After a grant, rotation resumes after that request. Non-requesting,
closing, indebted, and at-target requests are skipped. New requesters join at the
tail. This is deterministic one-chunk round robin, not DRR.

Granting a free chunk changes `F -= C`, `O += C`, and `p_i += C`. Granting a newly
allocated chunk changes `P += C`, `O += C`, and `p_i += C`. Release changes
`p_i -= C`, `O -= C`, and `F += C`, then wakes the scheduler.

If `d_i > 0`, only request i is blocked from optional acquisition. Other
under-target requests remain eligible. As owned chunks are consumed and released,
debt drains without eviction. A base-only requester automatically receives freed
optional chunks when its round-robin turn and predicate permit.

## Close-once response bodies

Every final selected `resp.Body` MUST be replaced by a shared `onceReadCloser`
before any outer handler defer is installed:

```go
type onceReadCloser struct {
    body io.ReadCloser
    once sync.Once
    err  error
}
```

Read delegates to body. Close calls `body.Close()` exactly once and returns the
stored result to every caller. The concrete wrapper, not a separate closer around
the same body, is assigned to `resp.Body`.

When retry or fallback logic replaces a response, the discarded response follows
its normal close path and the replacement body MUST receive a new
`onceReadCloser` before a new outer defer is installed. A wrapper MUST never be
reused across different response bodies. This applies to every final response
selection, not only successful first attempts.

The enabled engine receives the counted logical reader and the same
`onceReadCloser` used by `resp.Body` and its outer defer:

```go
copyBufferedMediaBody(
    ctx context.Context,
    dst io.Writer,
    src io.Reader,
    source *onceReadCloser,
    base []byte,
    expectedLength int64,
) mediaCopyResult
```

Concurrent Close MUST unblock a Read on the underlying HTTP response body. Plain
readers or noncompliant closers remain synchronous in v1.

## Server integration

For an eligible enabled media response, orchestration MUST be:

```text
select final response and wrap resp.Body with onceReadCloser before outer defer
identify media response and preserve clearMediaWriteDeadline
allocate private 32 KiB base; perform no other resource wait
acquire media reconfigure read gate and increment media in-flight count
register request and recompute optional targets
begin existing transfer telemetry and create counted reader/writer
set Content-Length and commit the upstream status
start producer and run handler consumer
set closing, close source, wake waits, join producer, reclaim optional chunks
unregister request and recompute targets
finish transfer telemetry and apply existing audit decision
panic(http.ErrAbortHandler) for a failed committed response
decrement media in-flight count and release media gate last
```

No pool or admission wait occurs before the gate. The only new pre-gate resource
operation is ordinary allocation of the private 32 KiB base. The gate remains
held from immediately before registration through producer join, chunk
reclamation, unregister, telemetry completion, audit handling, and committed
abort setup.

Telemetry begins only after base preparation, gate acquisition, and controller
registration, immediately before counted I/O and header commitment. Counted
wrappers count real upstream reads and downstream writes; allocator and queue
events do not count transfer bytes. The existing `clearMediaWriteDeadline`, audit
suppression, response headers, status, and `http.ErrAbortHandler` behavior MUST be
preserved.

The request goroutine is the only downstream writer. The producer goroutine is
the only logical upstream reader.

## Producer and consumer

The producer starts with the private base. After publishing that base, it requests
an optional chunk to read ahead. If denied, it waits until either the base returns,
an optional grant arrives, or closing begins. There is no startup prebuffer and no
output pacing; the consumer writes the first published bytes immediately.

If a read returns `(0, nil)`, the producer keeps the same chunk and reads again.
It MUST NOT publish an empty event, release the chunk, or wait for a consumer that
never received it. A positive read resets the empty-read count.

Each request has a synchronized closing flag guarded by its queue mutex or an
equivalent atomic protocol. Cleanup sets closing before Close or wake operations.
Publication MUST use this ordering:

```text
lock request queue
if closing: unlock; return optional chunk to pool or retain private base; exit
append data or terminal event
signal consumer
unlock
```

No event may be published after closing is visible. On cancellation or downstream
failure, the consumer sets closing, calls the shared Close, wakes queue and
scheduler waits, and synchronously joins the producer. There is no timeout path
that returns while the producer owns the source or a chunk. Only after join may
cleanup drain queued events, release every optional chunk exactly once, and drop
the private base reference.

## Ownership

The private base is always in exactly one request-local state:

```text
BASE_IDLE -> PRODUCER_READING -> QUEUED -> CONSUMER_WRITING -> BASE_IDLE
```

Closing may move any base state to `BASE_DONE` after the producer is joined and
the consumer no longer accesses it.

Each optional chunk is in exactly one state:

```text
FREE
PRODUCER_READING(request)
QUEUED(request)
CONSUMER_WRITING(request)
RELEASING
```

Legal optional transitions are:

```text
FREE -> PRODUCER_READING
PRODUCER_READING -> QUEUED | RELEASING
QUEUED -> CONSUMER_WRITING | RELEASING
CONSUMER_WRITING -> RELEASING
RELEASING -> FREE
```

A chunk generation MUST have one owner and be released exactly once. Producer and
consumer MUST never access the same chunk concurrently. Closing cleanup is
idempotent at the request level; duplicate generation release is an invariant
failure.

## Read and length semantics

The enabled producer MUST preserve the current `copyMediaBody` read behavior:

```text
read buffer length = 32 KiB for every normal read
```

It MUST NOT shorten reads to remaining Content-Length and MUST NOT use a one-byte
EOF probe. After reading exactly expectedLength with `readErr == nil`, it performs
another normal 32 KiB read. This preserves current overread classification and
`BytesRead` accounting.

For each read, validate `0 <= n <= len(chunk)` before changing `BytesRead`. Invalid
counts are upstream invalid-read failures. For valid `n > 0`, reset empty reads
and add the actual n to `BytesRead`. Compute:

```text
writeLength = n
lengthMismatch = false
if expectedLength >= 0 and BytesRead > expectedLength:
    writeLength = n - (BytesRead - expectedLength)
    lengthMismatch = true
```

Publish only `chunk[:writeLength]` when `writeLength > 0`. If lengthMismatch is
true, publish the allowed prefix first, then an upstream
`errMediaLengthMismatch`; the mismatch wins over the read's simultaneous error.
No byte beyond expectedLength may be written. The full returned n remains counted,
including a full 32 KiB overread after the exact boundary.

For `(n > 0, err != nil)` without mismatch, publish the n bytes before the
terminal. Non-EOF errors remain upstream failures. EOF below a known expected
length becomes `io.ErrUnexpectedEOF`; EOF exactly at expectedLength succeeds.
Unknown length succeeds at EOF after all positive bytes are written.

For `(0, nil)`, increment consecutive empty reads and fail upstream with
`io.ErrNoProgress` on the 100th. For `(0, io.EOF)`, return
`io.ErrUnexpectedEOF` only when a known expected length has not been reached;
otherwise succeed. Other zero-byte errors remain upstream failures.

## Write and result precedence

The consumer writes FIFO data events once. It validates
`0 <= n <= len(data)`, then adds n to `BytesWritten`. Invalid counts are downstream
invalid-write failures. A non-nil write error wins over short-write classification.
With nil error and `n < len(data)`, return `io.ErrShortWrite` downstream.

Result selection is total and independent of goroutine completion order:

```text
1. A downstream result returned by an attempted Write wins.
2. Otherwise cancellation wins if observed before terminal dequeue.
3. Otherwise the next FIFO upstream terminal wins after prior data is written.
4. Otherwise EOF completion succeeds.
```

The consumer MUST check cancellation before removing the next terminal under the
queue synchronization protocol; it MUST NOT randomly select between an already
canceled context and an already queued terminal. Cancellation after Write returns
does not replace that downstream result. A queued upstream terminal does not
outrank failure while writing earlier data. Close-induced producer errors are
ignored unless published before closing and no higher-precedence result exists.

Existing audit suppression MUST continue to check `r.Context().Err()`: cancellation
emits no media failure audit regardless of the internal result. A failed committed
response MUST call Close, join the producer, reclaim chunks, finish telemetry,
apply audit rules, and then panic with `http.ErrAbortHandler`.

## Worked examples

Examples use H=2 GiB, C=32 KiB, and S=512 MiB optional per request.

- **N=1:** target is 512 MiB optional. **N=2:** each target is 512 MiB.
  **N=3:** each target remains 512 MiB, leaving 512 MiB untargeted.
- **N=5:** each target is 409.59375 MiB optional. Five aligned targets consume
  2047.96875 MiB; one 32 KiB chunk remains outside all targets.
- **Arrivals and debt:** three incumbents at 512 MiB are joined by two base-only
  requests. All five targets immediately become 409.59375 MiB. Each incumbent
  has 102.40625 MiB local debt and receives no optional chunks. The arrivals may
  round-robin into the 512 MiB of unallocated pool capacity; incumbent debt does
  not block them.
- **Base-only promotion:** when P=H and F=0, a new request progresses with its
  private base. As any optional chunk is consumed and released, F increases and
  the scheduler grants free chunks in rotating order. Promotion requires no
  engine replacement or admission event.
- **Departures:** unregister recomputes targets immediately. Remaining requests
  become eligible up to their larger shares and reuse F before P can grow. P does
  not shrink when requests leave; at quiescence `O=0` and `F=P`.

## Snapshot telemetry

The optional controller MUST expose a non-blocking aggregate `Snapshot` through
the existing telemetry/admin integration. It contains only:

```text
enabled, H, P, O, F, active requests,
base-only requests, indebted requests, total request-debt bytes
```

Snapshot copies counters under the controller lock and performs no I/O. It MUST
not introduce a new metrics framework, background sampler, event stream, or
user/session/request labels. Existing transfer telemetry remains authoritative
for ingress, egress, duration, media mode, and errors.

## Deterministic acceptance matrix

Implementation is accepted only when deterministic tests prove:

- disabled mode calls unchanged `copyMediaBody`, creates no buffered components,
  and ignores unavailable or invalid buffered budget inputs;
- enabled startup selects and validates H, aligns to 32 KiB, rejects `H < C`, and
  produces exact automatic and explicit 2 GiB results;
- every enabled request allocates an immediate private base outside H and makes
  base-only progress with H exhausted, with no admission queue or pre-header wait;
- lazy concurrent allocation never makes P exceed H; P is monotonic; every
  release produces F; and atomic snapshots always satisfy `P=O+F`;
- registration and unregister immediately produce exact N=1,2,3,5 targets,
  arrival debt, departure growth, aligned remainder, and the fixed cap;
- local debt blocks only its request, and event-driven one-chunk round robin gives
  deterministic grants and automatic base-only promotion;
- every final first, retry, and fallback response body is wrapped before its outer
  defer; all Close callers share one result; Close unblocks a blocked producer;
- integration barriers prove deadline clearing, private-base preparation, gate,
  registration, telemetry, header commit, join, reclaim, unregister, audit/abort,
  and gate release occur in the normative order;
- read tables match current 32 KiB behavior for exact length, zero length, unknown
  length, full-quantum overread, short body, `(n>0,err)`, invalid counts, and 100
  empty reads with exact `BytesRead` and allowed-prefix output;
- write tables and event permutations prove byte order, invalid/short writes,
  exact `BytesWritten`, total error precedence, cancellation audit suppression,
  and committed abort only after producer join;
- cancellation at queue wait, optional wait, blocked Read, and blocked Write proves
  synchronized closing, no post-close publication, mandatory join, exact once-only
  ownership reclamation, and no goroutine or chunk leak;
- Snapshot exposes only the specified aggregate fields, and existing transfer
  counters change only on actual logical reads and downstream writes;
- deterministic burst/pause integration demonstrates queued optional data bridging
  a producer pause relative to disabled mode, and `-race` stress ends with all
  producers joined, `O=0`, `F=P`, and `P<=H`.

Tests MUST use explicit events, channels, and barriers for ordering. Sleeps MAY
appear only as deadlock sentinels that fail a test; correctness and fairness MUST
not depend on probabilistic timing. Fake clocks are required only if the
implementation introduces clock-dependent behavior. Full validation includes
focused tests, targeted `-race`, `go test ./...`, `go vet ./...`, and a gateway
build through `mise exec --`.

## Rollout and rollback

Implement sticky pool/controller tests first, then copy correctness and ownership,
then server and telemetry integration. Buffered mode remains disabled by default
until the deterministic matrix passes and a temporary local gateway validates
burst/pause behavior and cancellation.

First deployment uses H_auto on one trusted host. Any invariant failure, stuck
join, byte mismatch, close-ownership error, audit/abort regression, or unexplained
memory growth requires disabling buffering. Disabled mode immediately restores
the unchanged synchronous copier without using buffered components. No persisted
state or migration is introduced.

## Deferred work

V1 excludes runtime pressure adaptation, runtime H changes, decommit, bounded free
retention, bitrate metadata, weighted targets, water filling, DRR, growth credit,
membership hysteresis, user/session quotas, output pacing, startup prebuffer,
forced eviction, and asynchronous support for non-closeable readers. Each requires
a later ADR supported by v1 evidence.
