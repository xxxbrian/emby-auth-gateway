<script lang="ts">
import { onMount, onDestroy } from 'svelte';
import { apiRequest, ApiError } from '../lib/api';
import type {
    BufferAggregate,
    BufferStream,
    BufferStreamsResponse,
    BufferStreamDetailResponse,
    BufferCompletion,
    BufferRecentResponse,
    BufferSeriesResponse,
    BufferSeriesPoint,
    ObservationCompleteness,
    SeriesPoint,
} from '../lib/types';
import LineChart from '../lib/LineChart.svelte';
import CapacityBar from '../lib/CapacityBar.svelte';

type BufferTab = 'active' | 'recent';

// --- State ---
let aggregate = $state<BufferAggregate | null>(null);
let streams = $state<BufferStream[]>([]);
let streamsBootId = $state<string | null>(null);
let streamsCursor = $state<string | null>(null);
let streamsHasMore = $state(false);
let streamsCompleteness = $state<ObservationCompleteness>('unavailable');
let streamsLoading = $state(false);
let recent = $state<BufferCompletion[]>([]);
let series = $state<BufferSeriesPoint[]>([]);

let activeTab = $state<BufferTab>('active');
let timeWindow = $state('15m');
let expandedStream = $state<string | null>(null);
let expandedCompletion = $state<string | null>(null);
let searchFilter = $state('');
let error = $state<string | null>(null);
let streamsError = $state<string | null>(null);
let seriesError = $state<string | null>(null);
let recentError = $state<string | null>(null);
let loading = $state(true);
let staleAt = $state<number | null>(null);
let staleTick = $state(0);

// Deep-link support
let deepLinkStreamId = $state<string | null>(null);
let deepLinkBootId = $state<string | null>(null);
let deepLinkNotFound = $state(false);
let deepLinkError = $state<string | null>(null);

// AbortControllers for in-flight requests
let aggAbort: AbortController | null = null;
let streamsAbort: AbortController | null = null;
let seriesAbort: AbortController | null = null;
let recentAbort: AbortController | null = null;

let aggTimer: ReturnType<typeof setInterval> | undefined;
let streamsTimer: ReturnType<typeof setInterval> | undefined;
let seriesTimer: ReturnType<typeof setInterval> | undefined;
let recentTimer: ReturnType<typeof setInterval> | undefined;
let staleTimer: ReturnType<typeof setInterval> | undefined;

// --- Helpers ---
function parseDeepLink() {
    const hash = window.location.hash;
    const match = hash.match(/[?&]stream=([^&]+)/);
    if (match) {
        const raw = decodeURIComponent(match[1]);
        const colonIdx = raw.indexOf(':');
        if (colonIdx > 0) {
            deepLinkBootId = raw.substring(0, colonIdx);
            deepLinkStreamId = raw.substring(colonIdx + 1);
        } else {
            deepLinkStreamId = raw;
        }
        const cleaned = hash.replace(/[?&]stream=[^&]+/, '').replace(/\?$/, '');
        if (cleaned !== hash) {
            window.history.replaceState(null, '', cleaned || '#/buffer');
        }
    }
}

function fmtBytes(v: number | null | undefined): string {
    if (v == null || v <= 0) return '0 B';
    if (v < 1024) return `${v} B`;
    if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
    if (v < 1024 * 1024 * 1024) return `${(v / (1024 * 1024)).toFixed(1)} MB`;
    return `${(v / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

function fmtAge(ms: number | null | undefined): string {
    if (ms == null || ms < 0) return '-';
    const sec = Math.floor(ms / 1000);
    if (sec < 60) return `${sec}s`;
    const min = Math.floor(sec / 60);
    if (min < 60) return `${min}m`;
    const hr = Math.floor(min / 60);
    return `${hr}h ${min % 60}m`;
}

function fmtDuration(ms: number | null | undefined): string {
    if (ms == null || ms < 0) return '-';
    if (ms < 1000) return `${ms}ms`;
    const sec = ms / 1000;
    if (sec < 60) return `${sec.toFixed(1)}s`;
    const min = Math.floor(sec / 60);
    if (min < 60) return `${min}m ${Math.floor(sec % 60)}s`;
    const hr = Math.floor(min / 60);
    return `${hr}h ${min % 60}m`;
}

function fmtTimestamp(iso: string | null | undefined): string {
    if (!iso) return '-';
    try { return new Date(iso).toLocaleString(); } catch { return String(iso); }
}

function healthClass(h: string | null | undefined): string {
    if (h === 'critical') return 'status-err';
    if (h === 'warning') return 'status-warn';
    if (h === 'healthy') return 'status-ok';
    return 'text-secondary';
}

function healthLabel(h: string | null | undefined): string {
    if (h === 'critical') return 'Critical';
    if (h === 'warning') return 'Warning';
    if (h === 'healthy') return 'OK';
    if (h === 'idle') return 'Idle';
    if (h === 'disabled') return 'Disabled';
    return 'Unknown';
}

function outcomeClass(o: string | null | undefined): string {
    if (o === 'success') return 'status-ok';
    if (o === 'canceled') return 'status-warn';
    return 'status-err';
}

function waitLabel(w: string | null | undefined): string {
    if (!w || w === 'none') return '-';
    return w.replace(/_/g, ' ');
}

function blockerLabel(b: string | null | undefined): string {
    if (!b || b === 'none') return '-';
    return b.replace(/_/g, ' ');
}

function modeLabel(m: string | null | undefined): string {
    if (!m || m === 'unknown') return '-';
    return m.toUpperCase();
}

function apiErrorMsg(err: unknown): string {
    if (err instanceof ApiError) {
        if (err.code === 'provider_unavailable') return 'Buffer provider unavailable';
        if (err.code === 'stale_boot') return 'Stale boot ID (gateway restarted)';
        if (err.code === 'stale_cursor') return 'Page expired, refreshing';
        if (err.code === 'stream_not_found') return 'Stream not found';
        return err.message;
    }
    if (err instanceof Error) return err.message;
    return String(err);
}

// --- Data fetching with AbortController ---
async function fetchAggregate() {
    aggAbort?.abort();
    const ctrl = new AbortController();
    aggAbort = ctrl;
    try {
        const data = await apiRequest<{ media_buffer?: BufferAggregate }>(`/overview?window=${timeWindow}`, { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        if (data.media_buffer) {
            aggregate = data.media_buffer;
            staleAt = null;
        }
        error = null;
        loading = false;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        if (!aggregate) {
            error = apiErrorMsg(err);
            loading = false;
        } else {
            staleAt = staleAt ?? Date.now();
        }
    }
}

async function fetchStreams(append = false) {
    streamsAbort?.abort();
    const ctrl = new AbortController();
    streamsAbort = ctrl;
    streamsLoading = true;
    const cursor = append && streamsCursor ? `&cursor=${encodeURIComponent(streamsCursor)}` : '';
    try {
        const res = await apiRequest<BufferStreamsResponse>(`/media-buffer/streams?limit=50${cursor}`, { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        if (append) {
            streams = [...streams, ...(res.items || [])];
        } else {
            streams = res.items || [];
        }
        streamsBootId = res.boot_id;
        streamsCursor = res.next_cursor;
        streamsHasMore = res.has_more;
        streamsCompleteness = res.observation_completeness;
        streamsError = null;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        if (err instanceof ApiError && err.code === 'stale_cursor') {
            // Refresh from beginning
            streamsCursor = null;
            streamsError = null;
            streamsLoading = false;
            await fetchStreams(false);
            return;
        }
        streamsError = apiErrorMsg(err);
    } finally {
        streamsLoading = false;
    }
}

async function fetchSeries() {
    seriesAbort?.abort();
    const ctrl = new AbortController();
    seriesAbort = ctrl;
    try {
        const res = await apiRequest<BufferSeriesResponse>(`/media-buffer/series?window=${timeWindow}`, { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        series = res.points || [];
        seriesError = null;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        seriesError = apiErrorMsg(err);
    }
}

async function fetchRecent() {
    recentAbort?.abort();
    const ctrl = new AbortController();
    recentAbort = ctrl;
    try {
        const res = await apiRequest<BufferRecentResponse>('/media-buffer/recent?limit=50', { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        recent = res.items || [];
        recentError = null;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        recentError = apiErrorMsg(err);
    }
}

/** Resolve deep link via direct detail fetch. */
async function resolveDeepLink() {
    if (!deepLinkStreamId) return;
    const bootParam = deepLinkBootId ? `?boot_id=${encodeURIComponent(deepLinkBootId)}` : '';
    try {
        const res = await apiRequest<BufferStreamDetailResponse>(`/media-buffer/streams/${encodeURIComponent(deepLinkStreamId)}${bootParam}`);
        if (res.item) {
            // Ensure this stream is in our list for expansion
            const exists = streams.find(s => s.stream_id === res.item!.stream_id);
            if (!exists) {
                streams = [res.item, ...streams];
            }
            expandedStream = res.item.stream_id;
            deepLinkStreamId = null;
            deepLinkBootId = null;
            deepLinkNotFound = false;
            deepLinkError = null;
        } else {
            deepLinkNotFound = true;
        }
    } catch (err) {
        if (err instanceof ApiError) {
            if (err.code === 'stream_not_found') {
                deepLinkNotFound = true;
            } else if (err.code === 'stale_boot') {
                deepLinkError = 'Gateway restarted since this link was created';
            } else if (err.code === 'provider_unavailable') {
                deepLinkError = 'Buffer provider unavailable';
            } else {
                deepLinkError = apiErrorMsg(err);
            }
        } else {
            deepLinkError = apiErrorMsg(err);
        }
    }
}

function setWindow(w: string) {
    timeWindow = w;
    fetchAggregate();
    fetchSeries();
}

function switchTab(tab: BufferTab) {
    activeTab = tab;
    if (tab === 'recent' && recent.length === 0 && !recentError) {
        fetchRecent();
    }
}

function toggleExpand(streamId: string) {
    expandedStream = expandedStream === streamId ? null : streamId;
}

function toggleCompletionExpand(streamId: string) {
    expandedCompletion = expandedCompletion === streamId ? null : streamId;
}

function loadMoreStreams() {
    if (streamsHasMore && !streamsLoading) {
        fetchStreams(true);
    }
}

function dismissDeepLink() {
    deepLinkNotFound = false;
    deepLinkStreamId = null;
    deepLinkBootId = null;
    deepLinkError = null;
}

function activityBufferLink(bootId: string, streamId: string): string {
    return `#/activity?tab=transfers&buffer=${encodeURIComponent(bootId + ':' + streamId)}`;
}

function abortAll() {
    aggAbort?.abort();
    streamsAbort?.abort();
    seriesAbort?.abort();
    recentAbort?.abort();
}

// --- Derived ---
let allocatedSeries = $derived.by((): SeriesPoint[] => {
    const result: SeriesPoint[] = [];
    for (const p of series) {
        if (p.present && p.aggregate) {
            result.push({ t: p.t, v: p.aggregate.allocated_bytes });
        } else {
            result.push({ t: p.t, v: 0, gap: true } as SeriesPoint & { gap: boolean });
        }
    }
    return result;
});
let activeSeries = $derived.by((): SeriesPoint[] => {
    const result: SeriesPoint[] = [];
    for (const p of series) {
        if (p.present && p.aggregate) {
            result.push({ t: p.t, v: p.aggregate.observed_active_requests });
        } else {
            result.push({ t: p.t, v: 0, gap: true } as SeriesPoint & { gap: boolean });
        }
    }
    return result;
});
let contentionSeries = $derived.by((): SeriesPoint[] => {
    const result: SeriesPoint[] = [];
    for (const p of series) {
        if (p.present && p.aggregate) {
            result.push({ t: p.t, v: p.aggregate.pool_contention_count + p.aggregate.consumer_starvation_count });
        } else {
            result.push({ t: p.t, v: 0, gap: true } as SeriesPoint & { gap: boolean });
        }
    }
    return result;
});
let queuedSeries = $derived.by((): SeriesPoint[] => {
    const result: SeriesPoint[] = [];
    for (const p of series) {
        if (p.present && p.aggregate) {
            result.push({ t: p.t, v: p.aggregate.queued_bytes });
        } else {
            result.push({ t: p.t, v: 0, gap: true } as SeriesPoint & { gap: boolean });
        }
    }
    return result;
});

let filteredStreams = $derived.by(() => {
    if (!searchFilter.trim()) return streams;
    const q = searchFilter.toLowerCase();
    return streams.filter(s =>
        (s.username && s.username.toLowerCase().includes(q)) ||
        (s.item_id && s.item_id.toLowerCase().includes(q)) ||
        (s.device && s.device.toLowerCase().includes(q)) ||
        (s.stream_id && s.stream_id.includes(q))
    );
});

let staleAgo = $derived.by(() => {
    void staleTick; // subscribe to tick updates
    if (!staleAt) return null;
    const sec = Math.floor((Date.now() - staleAt) / 1000);
    return `${sec}s ago`;
});

let isDisabled = $derived(aggregate?.enabled === false);
let isIdle = $derived(aggregate?.health === 'idle');

onMount(() => {
    parseDeepLink();
    fetchAggregate();
    fetchStreams();
    fetchSeries();
    fetchRecent();

    // Resolve deep link after initial streams load
    if (deepLinkStreamId) {
        resolveDeepLink();
    }

    aggTimer = setInterval(fetchAggregate, 2000);
    streamsTimer = setInterval(() => fetchStreams(false), 5000);
    seriesTimer = setInterval(fetchSeries, 30000);
    recentTimer = setInterval(fetchRecent, 10000);
    staleTimer = setInterval(() => { staleTick++; }, 1000);
});

onDestroy(() => {
    abortAll();
    if (aggTimer) clearInterval(aggTimer);
    if (streamsTimer) clearInterval(streamsTimer);
    if (seriesTimer) clearInterval(seriesTimer);
    if (recentTimer) clearInterval(recentTimer);
    if (staleTimer) clearInterval(staleTimer);
});
</script>

<div class="page-header">
    <h1 class="page-title">Buffer</h1>
    <div class="segmented-control" role="tablist" aria-label="Time window">
        {#each ['15m', '1h', '6h', '24h'] as w}
            <button type="button" class="tab {timeWindow === w ? 'active' : ''}" role="tab" aria-selected={timeWindow === w} onclick={() => setWindow(w)}>{w}</button>
        {/each}
    </div>
</div>

<div class="page-body">
    {#if staleAgo}
        <div class="stale-banner" role="alert">Data may be stale &middot; last successful update {staleAgo}</div>
    {/if}

    {#if error && !aggregate}
        <div class="error-message">{error}</div>
    {/if}

    {#if loading && !aggregate}
        <div class="text-secondary">Loading&hellip;</div>
    {:else if isDisabled}
        <div class="disabled-notice text-secondary">Buffer management is not enabled.</div>
    {:else if aggregate}
        <!-- Aggregate panel: controller-coherent composition -->
        <div class="panel">
            <div class="panel-note">Controller snapshot (coherent)</div>
            <div class="data-grid-agg">
                <div class="metric-box">
                    <div class="metric-label">Pool Health</div>
                    <div class="metric-value {healthClass(aggregate.health)}">{healthLabel(aggregate.health)}</div>
                    {#if aggregate.health_reasons.length > 0}
                        <div class="text-xs text-secondary mt-2">{aggregate.health_reasons.join(' \u00b7 ')}</div>
                    {/if}
                </div>
                <div class="metric-box">
                    <div class="metric-label">Allocated</div>
                    <div class="metric-value mono">{fmtBytes(aggregate.allocated_bytes)} <span class="text-xs text-secondary">/ {fmtBytes(aggregate.hard_budget_bytes)}</span></div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Active</div>
                    <div class="metric-value active-metric">
                        <span class="active-observed mono">{aggregate.observed_active_requests}</span>
                        <span class="active-label text-xs text-secondary">observed</span>
                        {#if aggregate.unobserved_active_requests > 0}
                            <span class="active-sep text-xs text-secondary">+</span>
                            <span class="active-unobs mono text-xs status-warn">{aggregate.unobserved_active_requests}</span>
                            <span class="active-label text-xs status-warn">unobserved</span>
                        {/if}
                    </div>
                    <div class="text-xs text-secondary mt-2">{aggregate.active_requests} total requests</div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Observation</div>
                    <div class="metric-value" style="font-size: 14px;">
                        {#if aggregate.observation_completeness === 'complete'}
                            <span class="status-ok">Complete</span>
                        {:else if aggregate.observation_completeness === 'limited'}
                            <span class="status-warn">Limited</span> <span class="text-xs text-secondary">({aggregate.unobserved_active_requests} not observed)</span>
                        {:else}
                            <span class="text-secondary">Unavailable</span>
                        {/if}
                    </div>
                    {#if aggregate.live_registration_drops > 0}
                        <div class="text-xs text-secondary mt-2">{aggregate.live_registration_drops} registration drops (boot cumulative)</div>
                    {/if}
                </div>
            </div>
        </div>

        <!-- Capacity composition -->
        {#if !isIdle}
            <div class="panel">
                <div class="metric-label mb-2">Pool Composition <span class="panel-note">(controller-coherent)</span></div>
                <CapacityBar owned={aggregate.owned_bytes} free={aggregate.free_bytes} unallocated={aggregate.unallocated_optional_bytes} />
                <div class="data-grid mt-4" style="grid-template-columns: repeat(auto-fit, minmax(110px, 1fr));">
                    <div class="stat-pair"><span class="stat-label">Owned</span><span class="stat-value mono">{fmtBytes(aggregate.owned_bytes)}</span></div>
                    <div class="stat-pair"><span class="stat-label">Free</span><span class="stat-value mono">{fmtBytes(aggregate.free_bytes)}</span></div>
                    <div class="stat-pair"><span class="stat-label">Unallocated</span><span class="stat-value mono">{fmtBytes(aggregate.unallocated_optional_bytes)}</span></div>
                    <div class="stat-pair"><span class="stat-label">Debt</span><span class="stat-value mono">{fmtBytes(aggregate.request_debt_bytes)}</span></div>
                    <div class="stat-pair" title="Eventual sidecar gauge"><span class="stat-label">Queued*</span><span class="stat-value mono">{fmtBytes(aggregate.queued_bytes)}</span></div>
                    <div class="stat-pair" title="Eventual sidecar gauge"><span class="stat-label">Writing*</span><span class="stat-value mono">{fmtBytes(aggregate.writing_bytes)}</span></div>
                    <div class="stat-pair"><span class="stat-label">Private bases</span><span class="stat-value mono">{fmtBytes(aggregate.private_base_bytes)}</span></div>
                    <div class="stat-pair"><span class="stat-label">Base-only</span><span class="stat-value mono">{aggregate.base_only_requests}</span></div>
                </div>
                <div class="text-xs text-secondary mt-2">* Eventual sidecar totals from observed subset; not atomically coherent with pool composition.</div>
            </div>

            <!-- Condition counts -->
            {#if aggregate.warning_streams > 0 || aggregate.critical_streams > 0 || aggregate.pool_contention_count > 0 || aggregate.consumer_starvation_count > 0 || aggregate.upstream_stall_count > 0 || aggregate.downstream_stall_count > 0 || aggregate.close_join_stall_count > 0}
                <div class="panel">
                    <div class="metric-label mb-2">Sustained Conditions</div>
                    <div class="data-grid" style="grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));">
                        {#if aggregate.warning_streams > 0}
                            <div class="stat-pair"><span class="stat-label">Warning streams</span><span class="stat-value mono status-warn">{aggregate.warning_streams}</span></div>
                        {/if}
                        {#if aggregate.critical_streams > 0}
                            <div class="stat-pair"><span class="stat-label">Critical streams</span><span class="stat-value mono status-err">{aggregate.critical_streams}</span></div>
                        {/if}
                        {#if aggregate.pool_contention_count > 0}
                            <div class="stat-pair"><span class="stat-label">Pool contention</span><span class="stat-value mono status-warn">{aggregate.pool_contention_count}</span></div>
                        {/if}
                        {#if aggregate.consumer_starvation_count > 0}
                            <div class="stat-pair"><span class="stat-label">Consumer starvation</span><span class="stat-value mono status-warn">{aggregate.consumer_starvation_count}</span></div>
                        {/if}
                        {#if aggregate.upstream_stall_count > 0}
                            <div class="stat-pair"><span class="stat-label">Upstream stall</span><span class="stat-value mono status-warn">{aggregate.upstream_stall_count}</span></div>
                        {/if}
                        {#if aggregate.downstream_stall_count > 0}
                            <div class="stat-pair"><span class="stat-label">Downstream stall</span><span class="stat-value mono status-warn">{aggregate.downstream_stall_count}</span></div>
                        {/if}
                        {#if aggregate.close_join_stall_count > 0}
                            <div class="stat-pair"><span class="stat-label">Close/join stall</span><span class="stat-value mono status-err">{aggregate.close_join_stall_count}</span></div>
                        {/if}
                        {#if aggregate.completion_drops > 0}
                            <div class="stat-pair"><span class="stat-label">Completion drops</span><span class="stat-value mono text-secondary">{aggregate.completion_drops}</span></div>
                        {/if}
                    </div>
                </div>
            {/if}
        {/if}

        <!-- Series charts (sampled history) -->
        {#if !isIdle}
            <div class="panel">
                <div class="metric-label mb-2">History <span class="panel-note">(sampled, 1s cadence; gaps = no committed cycle)</span></div>
                <div class="data-grid" style="grid-template-columns: repeat(2, 1fr);">
                    <div class="metric-box">
                        <div class="metric-label">Allocated</div>
                        <div class="metric-value mono text-sm">{fmtBytes(aggregate.allocated_bytes)}</div>
                        <LineChart series={allocatedSeries} color="var(--text-primary)" gapAware={true} />
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Active Observed</div>
                        <div class="metric-value mono text-sm">{aggregate.observed_active_requests}</div>
                        <LineChart series={activeSeries} color="var(--text-primary)" gapAware={true} />
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Contention + Starvation</div>
                        <div class="metric-value mono text-sm {(aggregate.pool_contention_count + aggregate.consumer_starvation_count) > 0 ? 'status-warn' : ''}">{aggregate.pool_contention_count + aggregate.consumer_starvation_count}</div>
                        <LineChart series={contentionSeries} color="var(--warning)" gapAware={true} />
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Queued</div>
                        <div class="metric-value mono text-sm">{fmtBytes(aggregate.queued_bytes)}</div>
                        <LineChart series={queuedSeries} color="var(--text-primary)" gapAware={true} />
                    </div>
                </div>
            </div>
            {#if seriesError}
                <div class="error-message text-xs">Series: {seriesError}</div>
            {/if}
        {/if}

        <!-- Active / Recent tabs -->
        <div class="sub-nav" role="tablist" aria-label="Stream view">
            <button type="button" class="sub-nav-item {activeTab === 'active' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'active'} onclick={() => switchTab('active')}>Active Streams ({streams.length}{streamsHasMore ? '+' : ''})</button>
            <button type="button" class="sub-nav-item {activeTab === 'recent' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'recent'} onclick={() => switchTab('recent')}>Recent Completions ({recent.length})</button>
        </div>

        {#if activeTab === 'active'}
            {#if streamsError}
                <div class="error-message text-xs mb-4">Streams: {streamsError}</div>
            {/if}

            {#if deepLinkNotFound || deepLinkError}
                <div class="info-banner mb-4" role="status">
                    {#if deepLinkError}
                        {deepLinkError}
                    {:else}
                        Stream not found &mdash; may have completed outside the retention window.
                    {/if}
                    <button type="button" class="icon" onclick={dismissDeepLink} aria-label="Dismiss">&times;</button>
                </div>
            {/if}

            {#if streamsCompleteness === 'limited'}
                <div class="info-banner mb-4 text-xs" role="status">
                    Observation limited &mdash; {aggregate?.unobserved_active_requests ?? 0} active streams not observed. List may be incomplete.
                </div>
            {/if}

            <div class="flex items-center gap-2 mb-4">
                <input type="text" placeholder="Filter loaded rows by user, item, device..." bind:value={searchFilter} style="max-width: 320px;" aria-label="Filter loaded streams" />
                <span class="text-xs text-secondary">{filteredStreams.length} of {streams.length} loaded</span>
            </div>

            <div class="table-container panel" style="padding: 0;">
                <table class="streams-table">
                    <thead>
                        <tr>
                            <th>User</th>
                            <th>Item</th>
                            <th class="col-desktop">Device</th>
                            <th class="col-desktop">Mode</th>
                            <th class="col-tablet">Owned</th>
                            <th class="col-desktop">Target</th>
                            <th>Health</th>
                            <th class="col-tablet">Age</th>
                            <th class="col-action"></th>
                        </tr>
                    </thead>
                    <tbody>
                        {#if filteredStreams.length === 0}
                            <tr><td colspan="9" class="empty">{streams.length === 0 ? 'No active streams.' : 'No matching streams.'}</td></tr>
                        {/if}
                        {#each filteredStreams as stream (stream.stream_id)}
                            <tr class={expandedStream === stream.stream_id ? 'row-expanded' : ''}>
                                <td class="cell-truncate"><strong title={stream.username || stream.user_id || ''}>{stream.username || stream.user_id || '-'}</strong></td>
                                <td class="cell-truncate" title={stream.item_id || ''}>{stream.item_id || '-'}</td>
                                <td class="col-desktop">{stream.device || '-'}</td>
                                <td class="col-desktop mono">{modeLabel(stream.media_mode)}</td>
                                <td class="col-tablet mono">{fmtBytes(stream.owned_bytes)}</td>
                                <td class="col-desktop mono">{fmtBytes(stream.target_bytes)}</td>
                                <td>
                                    <span class={healthClass(stream.health)}>
                                        {#if stream.health === 'warning'}&bull;{/if}{#if stream.health === 'critical'}&times;{/if} {healthLabel(stream.health)}
                                    </span>
                                </td>
                                <td class="col-tablet mono">{fmtAge(stream.age_ms)}</td>
                                <td class="col-action">
                                    <button type="button" class="icon expand-btn" onclick={() => toggleExpand(stream.stream_id)} aria-expanded={expandedStream === stream.stream_id} aria-controls="detail-{stream.stream_id}" aria-label="Toggle detail for stream {stream.stream_id}">
                                        {expandedStream === stream.stream_id ? '\u25BC' : '\u25B6'}
                                    </button>
                                </td>
                            </tr>
                            {#if expandedStream === stream.stream_id}
                                <tr class="detail-row">
                                    <td colspan="9">
                                        <div class="stream-detail" id="detail-{stream.stream_id}" role="region" aria-label="Stream {stream.stream_id} detail">
                                            <div class="detail-section">
                                                <div class="detail-heading">Identity</div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Stream ID</span><span class="stat-value mono">{stream.stream_id}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Boot ID</span><span class="stat-value mono truncate" style="max-width: 140px;" title={stream.boot_id}>{stream.boot_id}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Transfer ID</span><span class="stat-value mono">{stream.transfer_id || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">User</span><span class="stat-value">{stream.username || '-'} <span class="text-secondary mono">({stream.user_id || '-'})</span></span></div>
                                                    <div class="stat-pair"><span class="stat-label">Device</span><span class="stat-value">{stream.device || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Item</span><span class="stat-value mono">{stream.item_id || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Mode</span><span class="stat-value">{modeLabel(stream.media_mode)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">Allocation <span class="panel-note">(controller-coherent word)</span></div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Target</span><span class="stat-value mono">{fmtBytes(stream.target_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Owned</span><span class="stat-value mono">{fmtBytes(stream.owned_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Debt</span><span class="stat-value mono">{fmtBytes(stream.debt_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Private base</span><span class="stat-value mono">{fmtBytes(stream.private_base_bytes)}</span></div>
                                                    <div class="stat-pair" title="Eventual gauge"><span class="stat-label">Queued*</span><span class="stat-value mono">{fmtBytes(stream.queued_bytes)}</span></div>
                                                    <div class="stat-pair" title="Eventual gauge"><span class="stat-label">Writing*</span><span class="stat-value mono">{fmtBytes(stream.writing_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Blocker</span><span class="stat-value">{blockerLabel(stream.allocation_blocker)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">State <span class="panel-note">(timed lifecycle words)</span></div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Lifecycle</span><span class="stat-value">{stream.state}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Producer</span><span class="stat-value">{stream.producer_state.replace(/_/g, ' ')}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Consumer</span><span class="stat-value">{stream.consumer_state.replace(/_/g, ' ')}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Wait condition</span><span class="stat-value">{waitLabel(stream.wait_condition)}{stream.wait_duration_ms > 0 ? ` (${fmtDuration(stream.wait_duration_ms)})` : ''}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">I/O <span class="panel-note">(eventual byte gauges)</span></div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Bytes read</span><span class="stat-value mono">{fmtBytes(stream.bytes_read)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Bytes written</span><span class="stat-value mono">{fmtBytes(stream.bytes_written)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Started</span><span class="stat-value mono">{fmtTimestamp(stream.started_at)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">Health</div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Status</span><span class="stat-value {healthClass(stream.health)}">{healthLabel(stream.health)}</span></div>
                                                    {#if stream.health_reasons.length > 0}
                                                        <div class="stat-pair"><span class="stat-label">Reasons</span><span class="stat-value">{stream.health_reasons.join(', ')}</span></div>
                                                    {/if}
                                                </div>
                                            </div>
                                            {#if stream.transfer_id}
                                                <div class="detail-links">
                                                    <a href={activityBufferLink(stream.boot_id, stream.stream_id)} title="View in Activity transfers">&rarr; Activity transfer</a>
                                                </div>
                                            {/if}
                                        </div>
                                    </td>
                                </tr>
                            {/if}
                        {/each}
                    </tbody>
                </table>
            </div>
            {#if streamsHasMore}
                <div class="flex items-center gap-2 mt-2">
                    <button type="button" class="secondary text-xs" onclick={loadMoreStreams} disabled={streamsLoading}>{streamsLoading ? 'Loading\u2026' : 'Load more streams'}</button>
                    <span class="text-xs text-secondary">{streams.length} loaded</span>
                </div>
            {/if}

        {:else}
            <!-- Recent completions -->
            {#if recentError}
                <div class="error-message text-xs mb-4">Recent completions: {recentError}</div>
            {/if}

            <div class="table-container panel" style="padding: 0;">
                <table class="recent-table">
                    <thead>
                        <tr>
                            <th>User</th>
                            <th class="col-desktop">Item</th>
                            <th class="col-desktop">Mode</th>
                            <th>Outcome</th>
                            <th class="col-tablet">Peak</th>
                            <th class="col-tablet">Written</th>
                            <th class="col-desktop">Duration</th>
                            <th class="col-desktop">Completed</th>
                            <th class="col-action"></th>
                        </tr>
                    </thead>
                    <tbody>
                        {#if recent.length === 0}
                            <tr><td colspan="9" class="empty">No recent completions.</td></tr>
                        {/if}
                        {#each recent as comp (comp.stream_id)}
                            <tr class={expandedCompletion === comp.stream_id ? 'row-expanded' : ''}>
                                <td class="cell-truncate"><strong title={comp.username || comp.user_id || ''}>{comp.username || comp.user_id || '-'}</strong></td>
                                <td class="col-desktop cell-truncate" title={comp.item_id || ''}>{comp.item_id || '-'}</td>
                                <td class="col-desktop mono">{modeLabel(comp.media_mode)}</td>
                                <td><span class={outcomeClass(comp.outcome)}>{comp.outcome.replace(/_/g, ' ')}</span></td>
                                <td class="col-tablet mono">{fmtBytes(comp.peak_owned_bytes)}</td>
                                <td class="col-tablet mono">{fmtBytes(comp.bytes_written)}</td>
                                <td class="col-desktop mono">{fmtDuration(comp.duration_ms)}</td>
                                <td class="col-desktop mono text-xs">{fmtTimestamp(comp.completed_at)}</td>
                                <td class="col-action">
                                    <button type="button" class="icon expand-btn" onclick={() => toggleCompletionExpand(comp.stream_id)} aria-expanded={expandedCompletion === comp.stream_id} aria-controls="comp-detail-{comp.stream_id}" aria-label="Toggle completion detail">
                                        {expandedCompletion === comp.stream_id ? '\u25BC' : '\u25B6'}
                                    </button>
                                </td>
                            </tr>
                            {#if expandedCompletion === comp.stream_id}
                                <tr class="detail-row">
                                    <td colspan="9">
                                        <div class="stream-detail" id="comp-detail-{comp.stream_id}" role="region" aria-label="Completion {comp.stream_id} detail">
                                            <div class="detail-section">
                                                <div class="detail-heading">Identity</div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Stream ID</span><span class="stat-value mono">{comp.stream_id}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Boot ID</span><span class="stat-value mono truncate" style="max-width: 140px;" title={comp.boot_id}>{comp.boot_id}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Transfer ID</span><span class="stat-value mono">{comp.transfer_id || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">User</span><span class="stat-value">{comp.username || '-'} <span class="text-secondary mono">({comp.user_id || '-'})</span></span></div>
                                                    <div class="stat-pair"><span class="stat-label">Device</span><span class="stat-value">{comp.device || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Item</span><span class="stat-value mono">{comp.item_id || '-'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Mode</span><span class="stat-value">{modeLabel(comp.media_mode)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">Outcome</div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Result</span><span class="stat-value {outcomeClass(comp.outcome)}">{comp.outcome.replace(/_/g, ' ')}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Invariant observed</span><span class="stat-value">{comp.invariant_observed ? 'Yes' : 'No'}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Final lifecycle</span><span class="stat-value">{comp.final_state}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Final producer</span><span class="stat-value">{comp.final_producer_state.replace(/_/g, ' ')}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Final consumer</span><span class="stat-value">{comp.final_consumer_state.replace(/_/g, ' ')}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Final blocker</span><span class="stat-value">{blockerLabel(comp.final_allocation_blocker)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">I/O and Peaks <span class="panel-note">(exact completion counters)</span></div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Bytes read</span><span class="stat-value mono">{fmtBytes(comp.bytes_read)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Bytes written</span><span class="stat-value mono">{fmtBytes(comp.bytes_written)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Peak owned</span><span class="stat-value mono">{fmtBytes(comp.peak_owned_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Peak debt</span><span class="stat-value mono">{fmtBytes(comp.peak_debt_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Peak queued</span><span class="stat-value mono">{fmtBytes(comp.peak_queued_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Peak writing</span><span class="stat-value mono">{fmtBytes(comp.peak_writing_bytes)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Duration</span><span class="stat-value mono">{fmtDuration(comp.duration_ms)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Started</span><span class="stat-value mono">{fmtTimestamp(comp.started_at)}</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Completed</span><span class="stat-value mono">{fmtTimestamp(comp.completed_at)}</span></div>
                                                </div>
                                            </div>
                                            <div class="detail-section">
                                                <div class="detail-heading">Wait Totals <span class="panel-note">(exact accumulated durations)</span></div>
                                                <div class="detail-grid">
                                                    <div class="stat-pair"><span class="stat-label">Buffer acquire</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.buffer_acquire.total)} (max {fmtDuration(comp.waits_ms.buffer_acquire.max)})</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Pool contention</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.pool_contention.total)} (max {fmtDuration(comp.waits_ms.pool_contention.max)})</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Consumer starvation</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.consumer_starvation.total)} (max {fmtDuration(comp.waits_ms.consumer_starvation.max)})</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Upstream stall</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.upstream_stall.total)} (max {fmtDuration(comp.waits_ms.upstream_stall.max)})</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Downstream stall</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.downstream_stall.total)} (max {fmtDuration(comp.waits_ms.downstream_stall.max)})</span></div>
                                                    <div class="stat-pair"><span class="stat-label">Close/join stall</span><span class="stat-value mono">{fmtDuration(comp.waits_ms.close_join_stall.total)} (max {fmtDuration(comp.waits_ms.close_join_stall.max)})</span></div>
                                                </div>
                                            </div>
                                            {#if comp.transfer_id}
                                                <div class="detail-links">
                                                    <span class="text-xs text-secondary">Transfer: {comp.transfer_id}</span>
                                                </div>
                                            {/if}
                                        </div>
                                    </td>
                                </tr>
                            {/if}
                        {/each}
                    </tbody>
                </table>
            </div>
            {#if recent.length > 0}
                <div class="text-xs text-secondary mt-2">Showing {recent.length} completions (bounded 15m retention).</div>
            {/if}
        {/if}
    {/if}
</div>

<style>
    .empty { text-align: center; padding: 2rem !important; color: var(--text-secondary); }
    .disabled-notice { text-align: center; padding: 3rem 1rem; }
    .stale-banner {
        border: 1px solid var(--warning);
        color: var(--warning);
        padding: 6px 12px;
        border-radius: 2px;
        margin-bottom: 1rem;
        font-size: 12px;
    }
    .info-banner {
        border: 1px solid var(--border-color);
        background-color: var(--panel-bg);
        color: var(--text-secondary);
        padding: 6px 12px;
        border-radius: 2px;
        font-size: 12px;
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.5rem;
    }
    .panel-note {
        font-size: 10px;
        font-weight: 400;
        color: var(--text-secondary);
        text-transform: none;
        letter-spacing: 0;
    }
    .stat-pair {
        display: flex;
        flex-direction: column;
        gap: 2px;
    }
    .stat-label {
        font-size: 11px;
        color: var(--text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.03em;
    }
    .stat-value {
        font-size: 13px;
        color: var(--text-primary);
    }
    .row-expanded {
        background-color: var(--panel-bg);
    }
    .detail-row td {
        padding: 0 !important;
        border-bottom: 1px solid var(--border-color);
    }
    .stream-detail {
        padding: 1rem 1.5rem;
        border-left: 2px solid var(--border-color);
        margin-left: 12px;
        display: flex;
        flex-direction: column;
        gap: 1rem;
    }
    .detail-section {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
    }
    .detail-heading {
        font-size: 11px;
        color: var(--text-secondary);
        text-transform: uppercase;
        letter-spacing: 0.05em;
        font-weight: 500;
    }
    .detail-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
        gap: 0.5rem 1rem;
    }
    .detail-links {
        padding-top: 0.5rem;
    }
    .detail-links a {
        font-size: 12px;
        color: var(--text-secondary);
        text-decoration: none;
    }
    .detail-links a:hover {
        color: var(--text-primary);
        text-decoration: underline;
    }
    .expand-btn {
        font-size: 10px;
        padding: 2px 6px;
    }
    .truncate {
        display: block;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
    }
    /* Override sub-nav to use buttons */
    .sub-nav .sub-nav-item {
        background: none;
        border: none;
        border-bottom: 2px solid transparent;
        cursor: pointer;
        font-family: var(--font-family);
    }

    /* Active metric: flex-wrap for observed + unobserved */
    .active-metric {
        display: flex;
        flex-wrap: wrap;
        align-items: baseline;
        gap: 0 0.4em;
    }
    .active-observed { font-size: 20px; font-weight: 500; }
    .active-label { white-space: nowrap; }
    .active-sep { margin: 0 0.1em; }
    .active-unobs { font-size: 14px; font-weight: 500; }

    /* Table responsive column classes */
    .cell-truncate {
        min-width: 0;
        max-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
    }
    .col-action {
        width: 40px;
        min-width: 40px;
        text-align: center;
    }

    /* Tables: fluid by default, constrained only above 768px */
    .streams-table,
    .recent-table {
        min-width: 0;
        table-layout: fixed;
    }
    @media (min-width: 769px) {
        .streams-table,
        .recent-table {
            table-layout: auto;
        }
        .cell-truncate {
            max-width: none;
        }
    }

    /* col-desktop: hidden below 769px (narrow tablets and phones) */
    /* col-tablet: hidden below 481px (phones only) */
    @media (max-width: 768px) {
        .col-desktop { display: none; }
        .stream-detail {
            margin-left: 0;
            padding: 0.75rem;
        }
        .detail-grid {
            grid-template-columns: 1fr 1fr;
        }
        .streams-table th,
        .streams-table td,
        .recent-table th,
        .recent-table td {
            padding: 6px 6px;
            font-size: 12px;
        }
        .expand-btn {
            font-size: 14px;
            padding: 4px 8px;
            min-width: 28px;
            min-height: 28px;
        }
    }
    @media (max-width: 480px) {
        .col-tablet { display: none; }
    }
</style>
