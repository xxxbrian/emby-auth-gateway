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
} from '../lib/types';
import TimeSeriesChart from '../lib/TimeSeriesChart.svelte';
import StreamPipeline from '../lib/StreamPipeline.svelte';
import MediaItemCell from '../lib/MediaItemCell.svelte';
import { streamStatus, conditionNames } from '../lib/buffer-display';
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
let timeWindow = $state('24h');
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

let recentPage = $state<BufferRecentResponse | null>(null);
let seriesMeta = $state<BufferSeriesResponse | null>(null);
let aggregateTime = $state('');
let bootStartedAt = $state('');
let streamRequestCursor = $state('');
let recentRequestCursor = $state('');
let outcomeFilter = $state('');
let recentBounds: { from: string; to: string } | null = null;
let selectedStream: BufferStream | null = null;
let selectedCompletion: BufferCompletion | null = null;
let detailAbort: AbortController | null = null;
let completionLinkId = $state<string | null>(null);
let completionLinkBoot = $state<string | null>(null);
const windowMS: Record<string,number> = { '15m':900_000,'1h':3_600_000,'6h':21_600_000,'24h':86_400_000 };
function updateQuery(changes: Record<string,string|null>) {
    const [route,query] = window.location.hash.split('?'); const params = new URLSearchParams(query);
    for (const [key,value] of Object.entries(changes)) { if(value) params.set(key,value); else params.delete(key); }
    window.history.replaceState(null,'',`${route || '#/buffer'}${params.size ? `?${params}` : ''}`);
}
function chartSeries(key: keyof BufferAggregate, label: string, color: string, peaks = false) {
    return { label, color, points: series.map(p => ({t:p.t, v:Number((peaks ? p.peaks?.[key] ?? p.aggregate?.[key] : p.aggregate?.[key]) || 0), gap: !p.present || !p.aggregate})) };
}
function hashChanged() {
    const params=new URLSearchParams(window.location.hash.split('?')[1]);
    const stream=params.get('stream'); const completion=params.get('completion');
    const currentStream=deepLinkStreamId ? `${deepLinkBootId ? `${deepLinkBootId}:` : ''}${deepLinkStreamId}` : null;
    const currentCompletion=completionLinkId ? `${completionLinkBoot ? `${completionLinkBoot}:` : ''}${completionLinkId}` : null;
    if(stream!==currentStream || completion!==currentCompletion) {
        detailAbort?.abort(); detailAbort=null;
        deepLinkStreamId=null; deepLinkBootId=null; completionLinkId=null; completionLinkBoot=null;
        expandedStream=null; expandedCompletion=null; selectedStream=null; selectedCompletion=null;
        deepLinkError=null; deepLinkNotFound=false; parseDeepLink();
        if(deepLinkStreamId) resolveDeepLink(); if(completionLinkId) resolveCompletion();
    }
}
function visibilityChange() {
    if (document.hidden) { abortAll(); return; }
    fetchAggregate(); fetchSeries();
    if(activeTab === 'active') { fetchStreams(); resolveDeepLink(); } else { fetchRecent(); resolveCompletion(); }
}
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
    const params = new URLSearchParams(window.location.hash.split('?')[1]);
    const w = params.get('window'); if(w && windowMS[w]) timeWindow=w;
    const raw = params.get('stream');
    if(raw) { const split = raw.indexOf(':'); deepLinkBootId = split > 0 ? raw.slice(0,split) : null; deepLinkStreamId=split > 0 ? raw.slice(split+1) : raw; expandedStream=deepLinkStreamId; }
    const completion = params.get('completion');
    if(completion) { const split = completion.indexOf(':'); completionLinkBoot=split > 0 ? completion.slice(0,split) : null; completionLinkId=split > 0 ? completion.slice(split+1) : completion; expandedCompletion=completionLinkId; activeTab='recent'; }
    if(params.get('tab') === 'recent') activeTab='recent';
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
    if (h === 'healthy') return 'Healthy';
    if (h === 'idle') return 'Idle';
    if (h === 'disabled') return 'Disabled';
    return 'Unknown';
}

function outcomeClass(o: string | null | undefined): string {
    if (o === 'success') return 'status-ok';
    if (o === 'canceled') return 'text-secondary';
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
        if (err.code === 'stale_cursor' || err.code === 'expired_cursor') return 'This page expired. Return to the first page.';
        if (err.code === 'completion_expired') return 'This completion expired from retained history.';
        if (err.code === 'completion_not_found') return 'Completion not found in retained history.';
        if (err.code === 'stream_not_found') return 'This stream is no longer active. Check recent completions.';
        return err.message;
    }
    if (err instanceof Error) return err.message;
    return String(err);
}

// --- Data fetching with AbortController ---
async function fetchAggregate() {
    if(document.hidden || aggAbort) return;
    const ctrl = new AbortController();
    aggAbort = ctrl;
    try {
        const data = await apiRequest<{ boot_id: string; now: string; started_at: string; media_buffer?: BufferAggregate }>('/media-buffer', { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        if (data.media_buffer) {
            aggregate = data.media_buffer;
            aggregateTime=data.now; bootStartedAt=data.started_at;
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
    } finally { if(aggAbort === ctrl) aggAbort = null; }
}

async function fetchStreams(append = false) {
    if(document.hidden || streamsAbort) return;
    if(append && streamsCursor) streamRequestCursor=streamsCursor;
    const ctrl = new AbortController();
    streamsAbort = ctrl;
    streamsLoading = true;
    const cursor = streamRequestCursor ? `&cursor=${encodeURIComponent(streamRequestCursor)}` : '';
    try {
        const res = await apiRequest<BufferStreamsResponse>(`/media-buffer/streams?limit=50${cursor}`, { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        streams = res.items || [];
        if (selectedStream && selectedStream.boot_id === res.boot_id && !streams.some(s => s.stream_id === selectedStream!.stream_id)) streams = [selectedStream, ...streams];
        if (streamsBootId && streamsBootId !== res.boot_id) { selectedStream=null; expandedStream=null; deepLinkError='Gateway restarted; previous stream references may be unavailable.'; }
        streamsBootId = res.boot_id;
        streamsCursor = res.next_cursor;
        streamsHasMore = res.has_more;
        streamsCompleteness = res.observation_completeness;
        streamsError = null;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        streamsError = apiErrorMsg(err);
    } finally {
        streamsLoading = false;
        if(streamsAbort === ctrl) streamsAbort=null;
    }
}

async function fetchSeries() {
    if(document.hidden || seriesAbort) return;
    const ctrl = new AbortController();
    seriesAbort = ctrl;
    try {
        const res = await apiRequest<BufferSeriesResponse>(`/media-buffer/series?window=${timeWindow}`, { signal: ctrl.signal });
        if (ctrl.signal.aborted) return;
        series = res.points || [];
        seriesMeta=res;
        seriesError = null;
    } catch (err) {
        if ((err as Error).name === 'AbortError') return;
        seriesError = apiErrorMsg(err);
    } finally { if(seriesAbort === ctrl) seriesAbort=null; }
}

async function fetchRecent(next = false) {
    if(document.hidden || recentAbort) return;
    if(next) recentRequestCursor=recentPage?.next_cursor || '';
    if(!recentRequestCursor || !recentBounds) { const snapshotTime=Date.now(); recentBounds={from:new Date(snapshotTime-windowMS[timeWindow]).toISOString(),to:new Date(snapshotTime).toISOString()}; }
    const params = new URLSearchParams({limit:'50',...recentBounds});
    if(recentRequestCursor) params.set('cursor',recentRequestCursor);
    if(outcomeFilter) params.set('outcome',outcomeFilter);
    const ctrl=new AbortController(); recentAbort=ctrl;
    try {
        const res=await apiRequest<BufferRecentResponse>(`/media-buffer/recent?${params}`,{signal:ctrl.signal});
        if(ctrl.signal.aborted) return;
        recent=res.items || []; recentPage=res; recentError=null;
        if(selectedCompletion && selectedCompletion.boot_id === res.boot_id && !recent.some(c=>c.completion_id===selectedCompletion!.completion_id)) recent=[selectedCompletion,...recent];
    } catch(err) { if(!ctrl.signal.aborted) recentError=apiErrorMsg(err); }
    finally { if(recentAbort===ctrl) recentAbort=null; }
}
function resetRecent() { recentAbort?.abort(); recentAbort=null; recentRequestCursor=''; recentBounds=null; selectedCompletion=null; expandedCompletion=null; completionLinkId=null; updateQuery({completion:null}); if(activeTab==='recent') fetchRecent(); }
function firstStreams() { streamsAbort?.abort(); streamsAbort=null; streamRequestCursor=''; fetchStreams(); }
async function resolveCompletion() {
    if(!completionLinkId || detailAbort || document.hidden) return;
    const ctrl=new AbortController(); detailAbort=ctrl;
    try {
        const result=await apiRequest<{boot_id:string;item:BufferCompletion}>(`/media-buffer/recent/${encodeURIComponent(completionLinkId)}${completionLinkBoot ? `?boot_id=${encodeURIComponent(completionLinkBoot)}` : ''}`,{signal:ctrl.signal});
        if(ctrl.signal.aborted) return;
        if(!result.item) { selectedCompletion=null; expandedCompletion=null; recentError='No completion details are available for this reference.'; return; }
        selectedCompletion=result.item; expandedCompletion=completionLinkId;
        recent=[result.item,...recent.filter(c=>c.completion_id!==result.item.completion_id)]; recentError=null;
    } catch(err) { if(!ctrl.signal.aborted) recentError=apiErrorMsg(err); }
    finally { if(detailAbort===ctrl) detailAbort=null; }
}

/** Resolve deep link via direct detail fetch. */
async function resolveDeepLink() {
    if (!deepLinkStreamId || detailAbort || document.hidden) return;
    const ctrl=new AbortController(); detailAbort=ctrl;
    const bootParam = deepLinkBootId ? `?boot_id=${encodeURIComponent(deepLinkBootId)}` : '';
    try {
        const res = await apiRequest<BufferStreamDetailResponse>(`/media-buffer/streams/${encodeURIComponent(deepLinkStreamId)}${bootParam}`, {signal:ctrl.signal});
        if(ctrl.signal.aborted) return;
        if (res.item) {
            selectedStream=res.item;
            streams=[res.item,...streams.filter(s=>s.stream_id!==res.item!.stream_id)];
            expandedStream = res.item.stream_id;
            deepLinkNotFound = false;
            deepLinkError = null;
        } else {
            deepLinkNotFound = true;
        }
    } catch (err) {
        if(ctrl.signal.aborted) return;
        if (err instanceof ApiError) {
            if (err.code === 'stream_not_found') {
                deepLinkNotFound = true;
                deepLinkError="The selected stream is no longer active. Showing its last available snapshot; check recent completions.";
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
    } finally { if(detailAbort===ctrl) detailAbort=null; }
}

function setWindow(w: string) {
    timeWindow=w; seriesAbort?.abort(); seriesAbort=null; series=[]; seriesMeta=null;
    try { localStorage.setItem('admin.buffer.window',w); } catch { /* optional preference */ }
    updateQuery({window:w}); fetchSeries(); resetRecent();
}
function switchTab(tab: BufferTab) { detailAbort?.abort(); detailAbort=null; activeTab=tab; updateQuery({tab}); if(tab==='recent') { fetchRecent(); resolveCompletion(); } else { fetchStreams(); resolveDeepLink(); } }
function toggleExpand(id: string) {
    detailAbort?.abort(); detailAbort=null;
    completionLinkId=null; completionLinkBoot=null; selectedCompletion=null; expandedCompletion=null;
    if(expandedStream===id) { expandedStream=null; deepLinkStreamId=null; selectedStream=null; updateQuery({stream:null}); return; }
    const stream=streams.find(s=>s.stream_id===id); if(!stream) return;
    selectedStream=stream; expandedStream=id; deepLinkStreamId=id; deepLinkBootId=stream.boot_id; deepLinkError=null;
    updateQuery({stream:`${stream.boot_id}:${id}`,completion:null}); resolveDeepLink();
}
function toggleCompletionExpand(id: string) {
    detailAbort?.abort(); detailAbort=null;
    deepLinkStreamId=null; deepLinkBootId=null; selectedStream=null; expandedStream=null;
    if(expandedCompletion===id) { expandedCompletion=null; completionLinkId=null; selectedCompletion=null; updateQuery({completion:null}); return; }
    const item=recent.find(c=>c.completion_id===id); if(!item) return;
    selectedCompletion=item; expandedCompletion=id; completionLinkId=id; completionLinkBoot=item.boot_id;
    updateQuery({completion:`${item.boot_id}:${id}`,stream:null}); resolveCompletion();
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
    deepLinkError = null; expandedStream=null; selectedStream=null; updateQuery({stream:null});
}

function activityBufferLink(bootId: string, streamId: string): string {
    return `#/activity?tab=transfers&buffer=${encodeURIComponent(bootId + ':' + streamId)}`;
}

function abortAll() {
    aggAbort?.abort();
    streamsAbort?.abort();
    seriesAbort?.abort();
    recentAbort?.abort();
    detailAbort?.abort();
    aggAbort=null; streamsAbort=null; seriesAbort=null; recentAbort=null; detailAbort=null;
}

let filteredStreams = $derived.by(() => {
    if (!searchFilter.trim()) return streams;
    const q = searchFilter.toLowerCase();
    return streams.filter(s => s.stream_id === expandedStream ||
        (s.username && s.username.toLowerCase().includes(q)) ||
        (s.item_id && s.item_id.toLowerCase().includes(q)) ||
        (s.device && s.device.toLowerCase().includes(q)) ||
        (s.stream_id && s.stream_id.includes(q))
    );
});

let staleAgo = $derived.by(() => {
    void staleTick; // subscribe to tick updates
    const last=Date.parse(aggregateTime);
    if (!last || (!staleAt && Date.now()-last < 10_000)) return null;
    const sec = Math.floor((Date.now() - last) / 1000);
    return `${sec}s ago`;
});

let isDisabled = $derived(aggregate?.enabled === false);
let isIdle = $derived(aggregate?.health === 'idle');

onMount(() => {
    try { const preferred=localStorage.getItem('admin.buffer.window'); if(preferred && windowMS[preferred]) timeWindow=preferred; } catch { /* use default */ }
    parseDeepLink();
    fetchAggregate();
    fetchStreams();
    fetchSeries();
    if(activeTab==='recent') fetchRecent();
    if(completionLinkId) resolveCompletion();
    document.addEventListener('visibilitychange',visibilityChange);
    window.addEventListener('hashchange',hashChanged);

    // Resolve deep link after initial streams load
    if (deepLinkStreamId) {
        resolveDeepLink();
    }

    aggTimer = setInterval(() => { fetchAggregate(); if(activeTab==='active') resolveDeepLink(); }, 2000);
    streamsTimer = setInterval(() => { if(activeTab==='active') fetchStreams(false); }, 5000);
    seriesTimer = setInterval(fetchSeries, 30000);
    recentTimer = setInterval(() => { if(activeTab==='recent') fetchRecent(); }, 10000);
    staleTimer = setInterval(() => { staleTick++; }, 1000);
});

onDestroy(() => {
    abortAll();
    document.removeEventListener('visibilitychange',visibilityChange);
    window.removeEventListener('hashchange',hashChanged);
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
    {:else if aggregate}
        {#if isDisabled}<div class="info-banner mb-4">Buffer management is not enabled. Retained history remains visible below.</div>{/if}
        <!-- Aggregate panel: controller-coherent composition -->
        <div class="panel">
            <div class="panel-note">Controller snapshot (coherent)</div>
            <div class="data-grid-agg">
                <div class="metric-box">
                    <div class="metric-label">Pool Health</div>
                    <div class="metric-value {healthClass(aggregate.health)}">{healthLabel(aggregate.health)}</div>
                    {#if aggregate.health_reasons.length > 0}
                        <div class="text-xs text-secondary mt-2">{aggregate.health_reasons.map(reason => conditionNames[reason] || reason.replaceAll('_', ' ')).join(' · ')}</div>
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

    {/if}
        <section class="panel history-panel">
            <div class="flex items-center justify-between gap-4 mb-4"><div><h2 class="history-heading">History · {timeWindow}</h2><p class="text-xs text-secondary">{seriesMeta?.interval === '1m' ? 'One-minute samples: last committed snapshot; condition charts preserve independent minute peaks.' : 'One-second committed snapshots.'} Gaps mean no observation.</p></div><a class="text-xs" href={`#/traffic?view=errors&window=${timeWindow}`}>Recorded errors ↗</a></div>
            {#if seriesError}<div class="error-message" role="alert">{seriesError}. {series.length ? 'Showing the last available history.' : ''}</div>{/if}
            <div class="history-grid">
                <div><h3>Optional pool memory</h3><TimeSeriesChart label="Optional pool memory" unit="bytes" series={[chartSeries('allocated_bytes','Allocated','#8cb2ff'),chartSeries('owned_bytes','Owned','#64c3b0'),chartSeries('free_bytes','Reusable','#b59beb')]} /></div>
                <div><h3>Queued and writing</h3><TimeSeriesChart label="Queued and writing bytes" unit="bytes" series={[chartSeries('queued_bytes','Queued','#8cb2ff'),chartSeries('writing_bytes','Writing','#64c3b0')]} /></div>
                <div><h3>Active requests</h3><TimeSeriesChart label="Active requests" series={[chartSeries('active_requests','Active','#8cb2ff'),chartSeries('observed_active_requests','Observed','#64c3b0')]} /></div>
                <div><h3>Sustained conditions {seriesMeta?.interval === '1m' ? '· minute peaks' : ''}</h3><TimeSeriesChart label="Sustained stream conditions" series={[chartSeries('upstream_stall_count','Upstream stall','#ed9876',true),chartSeries('downstream_stall_count','Client stall','#bf95ee',true),chartSeries('consumer_starvation_count','Waiting for data','#e1c069',true),chartSeries('pool_contention_count','Pool contention','#70b8e8',true),chartSeries('close_join_stall_count','Close stall','#ed7373',true)]} /></div>
            </div>
            <p class="text-xs text-secondary mt-4">Gateway started {fmtTimestamp(seriesMeta?.started_at || bootStartedAt)} · observed history begins {fmtTimestamp(seriesMeta?.available_from)} · {seriesMeta?.interval || '—'} resolution. Memory history clears on restart.</p>
        </section>

    {#if aggregate}
        <!-- Active / Recent tabs -->
        <div class="sub-nav" role="tablist" aria-label="Stream view">
            <button type="button" class="sub-nav-item {activeTab === 'active' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'active'} onclick={() => switchTab('active')}>Active Streams ({streams.length}{streamsHasMore ? '+' : ''})</button>
            <button type="button" class="sub-nav-item {activeTab === 'recent' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'recent'} onclick={() => switchTab('recent')}>Recent Completions ({recent.length})</button>
        </div>

        {#if activeTab === 'active'}
            {#if streamsError}
                <div class="error-message text-xs mb-4">Streams: {streamsError} <button class="secondary" onclick={firstStreams}>Refresh first page</button></div>
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
                <input type="text" placeholder="Filter this page by user, item ID, device..." bind:value={searchFilter} style="max-width: 320px;" aria-label="Filter loaded streams" />
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
                            <th class="col-tablet">Queue reserve</th>
                            <th class="col-desktop">Optional limit</th>
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
                                <td><MediaItemCell itemId={stream.item_id} sourceRef={stream.source_ref} at={stream.started_at} /></td>
                                <td class="col-desktop">{stream.device || '-'}</td>
                                <td class="col-desktop mono">{modeLabel(stream.media_mode)}</td>
                                <td class="col-tablet"><span class="mono">{fmtBytes(stream.queued_bytes)}</span><div class="queue-mini" role="img" aria-label={`${fmtBytes(stream.queued_bytes)} queued; ${fmtBytes(stream.private_base_bytes+stream.target_bytes)} allowance reference`}><span style:width={`${Math.min(100,stream.queued_bytes/Math.max(1,stream.private_base_bytes+stream.target_bytes)*100)}%`}></span></div><span class="text-xs text-secondary">{fmtBytes(stream.private_base_bytes+stream.target_bytes)} allowance</span></td>
                                <td class="col-desktop mono">{fmtBytes(stream.target_bytes)}</td>
                                <td>
                                    <span class={healthClass(stream.health)}>
                                        {healthLabel(stream.health)}<span class="flow-label">{streamStatus(stream)}</span>
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
                                            {#if deepLinkError}<p class="text-xs status-warn">Current stream state is unavailable. The visualization below is the last observed snapshot.</p>{/if}<StreamPipeline {stream} /><details><summary>Technical data</summary>
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
                                            </details>
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
                    <button type="button" class="secondary text-xs" onclick={loadMoreStreams} disabled={streamsLoading}>{streamsLoading ? 'Loading\u2026' : 'Next page'}</button>
                    <span class="text-xs text-secondary">{streams.length} loaded</span>
                </div>
            {/if}

            {#if streamRequestCursor}<button class="secondary mb-4" onclick={firstStreams}>First page</button>{/if}
            <p class="text-xs text-secondary">This page is retained during refresh. An expanded stream stays pinned while its detail is refreshed directly.</p>
        {:else}
            <!-- Recent completions -->
            <div class="flex items-center justify-between mb-4 gap-4"><label class="text-xs text-secondary">Outcome <select bind:value={outcomeFilter} onchange={resetRecent}><option value="">All outcomes</option><option value="errors">Errors only</option><option value="success">Success</option><option value="canceled">Canceled</option><option value="upstream_error">Upstream error</option><option value="downstream_error">Downstream error</option><option value="length_mismatch">Length mismatch</option></select></label><span class="text-xs text-secondary">Completed within {timeWindow}</span></div>
            {#if recentPage}<div class="info-banner mb-4 text-xs">Retained {recentPage.retained_count} / {recentPage.capacity} records · up to {Math.round(recentPage.retention_seconds/3600)}h · oldest retained {fmtTimestamp(recentPage.oldest_retained_at)}. {recentPage.evicted_count} evicted since startup; {aggregate.completion_drops} completion offers dropped. High traffic can shorten this window.</div>{/if}
            {#if recentError}
                <div class="error-message text-xs mb-4">Recent completions: {recentError} <button class="secondary" onclick={resetRecent}>Refresh first page</button></div>
            {/if}

            <div class="table-container panel" style="padding: 0;">
                <table class="recent-table">
                    <thead>
                        <tr>
                            <th>User</th>
                            <th>Media</th>
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
                        {#each recent as comp (comp.completion_id)}
                            <tr class={expandedCompletion === comp.completion_id ? 'row-expanded' : ''}>
                                <td class="cell-truncate"><strong title={comp.username || comp.user_id || ''}>{comp.username || comp.user_id || '-'}</strong></td>
                                <td><MediaItemCell itemId={comp.item_id} sourceRef={comp.source_ref} at={comp.completed_at} /></td>
                                <td class="col-desktop mono">{modeLabel(comp.media_mode)}</td>
                                <td><span class={outcomeClass(comp.outcome)}>{comp.outcome.replace(/_/g, ' ')}</span></td>
                                <td class="col-tablet mono">{fmtBytes(comp.peak_owned_bytes)}</td>
                                <td class="col-tablet mono">{fmtBytes(comp.bytes_written)}</td>
                                <td class="col-desktop mono">{fmtDuration(comp.duration_ms)}</td>
                                <td class="col-desktop mono text-xs">{fmtTimestamp(comp.completed_at)}</td>
                                <td class="col-action">
                                    <button type="button" class="icon expand-btn" onclick={() => toggleCompletionExpand(comp.completion_id)} aria-expanded={expandedCompletion === comp.completion_id} aria-controls="comp-detail-{comp.stream_id}" aria-label="Toggle completion detail">
                                        {expandedCompletion === comp.completion_id ? '\u25BC' : '\u25B6'}
                                    </button>
                                </td>
                            </tr>
                            {#if expandedCompletion === comp.completion_id}
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
            <div class="flex items-center gap-4 mt-4">{#if recentRequestCursor}<button class="secondary" onclick={resetRecent}>First page</button>{/if}{#if recentPage?.has_more}<button class="secondary" onclick={()=>fetchRecent(true)}>Next page</button>{/if}</div>
            {#if recent.length > 0}
                <div class="text-xs text-secondary mt-2">Showing {recent.length} completions. Later pages keep a fixed query window during refresh. An expanded completion remains pinned.</div>
            {/if}
        {/if}
        <p class="text-xs text-secondary mt-4">Current snapshot {fmtTimestamp(aggregateTime)} · Boot {streamsBootId || '—'}</p>
    {/if}
</div>

<style>
    .history-panel { padding:20px; }.history-heading { font-size:13px; font-weight:500; margin:0; }.history-grid { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:24px; }.history-grid > div { min-width:0; border-top:1px solid var(--border-color); padding-top:18px; }.history-grid h3 { font-size:11px; color:var(--text-secondary); font-weight:500; margin:0 0 16px; }.queue-mini { height:4px; background:#343439; overflow:hidden; border-radius:3px; margin:7px 0; min-width:85px; }.queue-mini span { display:block; height:100%; background:#779dea; }.flow-label { display:block; font-size:10px; max-width:200px; white-space:normal; line-height:1.6; margin-top:5px; color:var(--text-secondary); }summary { cursor:pointer; color:var(--text-secondary); font-size:12px; margin-bottom:18px; }details .detail-section { margin-bottom:20px; }
    @media(max-width:850px) { .history-grid { grid-template-columns:1fr; } }

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
        min-width: 570px;
        table-layout: auto;
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
    @media (max-width: 600px) {
        .streams-table, .recent-table { display:block; width:100%; min-width:0; max-width:100%; }
        .streams-table thead, .recent-table thead { display:none; }
        .streams-table tbody, .recent-table tbody { display:block; width:100%; }
        .streams-table tr:not(.detail-row), .recent-table tr:not(.detail-row) {
            display:grid; grid-template-columns:minmax(0,1fr) 32px; gap:8px 12px;
            padding:12px; border-bottom:1px solid var(--border-color);
        }
        .streams-table td, .recent-table td { min-width:0; padding:0 !important; border:0; }
        .streams-table .col-tablet, .recent-table .col-tablet { display:none; }
        .streams-table td:first-child, .recent-table td:first-child { grid-column:1; grid-row:1; }
        .streams-table td:nth-child(2), .recent-table td:nth-child(2) { grid-column:1; grid-row:2; }
        .streams-table td:nth-child(7), .recent-table td:nth-child(4) { grid-column:1; grid-row:3; }
        .streams-table td.col-action, .recent-table td.col-action {
            grid-column:2; grid-row:1 / span 3; width:32px; min-width:32px;
            display:flex; align-items:center; justify-content:center;
        }
        .cell-truncate { max-width:none; white-space:normal; overflow-wrap:anywhere; }
        .flow-label { max-width:none; font-size:11px; }
        .detail-row, .detail-row > td { display:block; width:100%; max-width:100%; }
        .detail-grid { grid-template-columns:repeat(2,minmax(0,1fr)); }
        .stat-value { min-width:0; overflow-wrap:anywhere; }
    }
</style>
