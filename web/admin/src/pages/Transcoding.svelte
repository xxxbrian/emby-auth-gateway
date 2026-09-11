<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest, ApiError } from '../lib/api';
    import type { TranscodingPage, TranscodingJob } from '../lib/types';
    import TranscodingSummary from '../lib/TranscodingSummary.svelte';

    let page = $state<TranscodingPage | null>(null);
    let recent = $state<TranscodingJob[]>([]);
    let activeTab = $state<'active' | 'recent'>('active');
    let selected = $state<TranscodingJob | null>(null);
    let selectedId = $state<string | null>(null);
    let error = $state<string | null>(null);
    let detailError = $state<string | null>(null);
    let loading = $state(true);
    let pageCursor = $state('');
    let rows = $derived(activeTab === 'active' ? page?.items ?? [] : recent);
    let controller: AbortController | null = null;
    let timer: ReturnType<typeof setInterval> | undefined;
    let expectedBoot: string | null = null;

    const reasons: Record<string, string> = {
        audio_not_supported: 'Audio format or channel layout is not supported by this client',
        remux_required: 'The selected tracks are repackaged for this client',
        index_unavailable: 'A reliable seek index is unavailable',
        source_unavailable: 'The upstream media is unavailable or has changed',
        capacity_exhausted: 'The configured resource budget is exhausted',
        work_timeout: 'Media preparation exceeded its time limit',
        worker_failed: 'The conversion process could not produce valid media',
    };
    const stateNames: Record<string, string> = {
        preparing: 'Preparing', queued: 'Queued', producing: 'Converting', ready: 'Idle', stopped: 'Stopped', failed: 'Failed',
    };
    function size(n: number | null): string {
        if (n == null) return '—';
        if (n >= 1024 ** 3) return `${(n / 1024 ** 3).toFixed(2)} GiB`;
        return `${(n / 1024 ** 2).toFixed(1)} MiB`;
    }
    function clock(ticks: number | null): string {
        if (ticks == null) return 'Not reported';
        const seconds = Math.max(0, Math.floor(ticks / 10_000_000));
        return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
    }
    function channels(n: number): string { return n === 6 ? '5.1' : n === 2 ? 'Stereo' : n === 1 ? 'Mono' : `${n} ch`; }
    function percent(n: number, duration: number): number { return Math.min(100, Math.max(0, duration > 0 ? n / duration * 100 : 0)); }
    function timestamp(value: string | null): string { return value ? new Date(value).toLocaleTimeString() : '—'; }

    async function load(append = false) {
        if (document.hidden) return;
        controller?.abort();
        const ctrl = new AbortController(); controller = ctrl;
        try {
            if (append && page?.next_cursor) pageCursor = page.next_cursor;
            const cursor = pageCursor ? `&cursor=${encodeURIComponent(pageCursor)}` : '';
            const result = await apiRequest<TranscodingPage>(`/transcoding/jobs?limit=50${cursor}`, { signal: ctrl.signal });
            if (ctrl.signal.aborted) return;
            if (page && page.boot_id !== result.boot_id) { selectedId = null; selected = null; detailError = 'The gateway restarted. Select a current task.'; }
            page = result;
            error = null; loading = false;
            if (activeTab === 'recent') {
                const result = await apiRequest<{ items: TranscodingJob[] }>('/transcoding/recent?limit=50', { signal: ctrl.signal });
                if (ctrl.signal.aborted) return; recent = result.items;
            }
            if (selectedId) {
                const boot = expectedBoot || page.boot_id;
                try {
                    const detail = await apiRequest<{ job: TranscodingJob }>(`/transcoding/jobs/${encodeURIComponent(selectedId)}?boot_id=${encodeURIComponent(boot)}`, { signal: ctrl.signal });
                    if (ctrl.signal.aborted) return; selected = detail.job; detailError = null; expectedBoot = null;
                } catch (err) {
                    if (ctrl.signal.aborted) return;
                    selected = null; detailError = err instanceof Error ? err.message : 'Task details unavailable';
                }
            }
        } catch (err) {
            if (ctrl.signal.aborted) return;
            if (err instanceof ApiError && err.code === 'stale_cursor') { pageCursor = ''; await load(); return; }
            error = err instanceof Error ? err.message : 'Unable to load conversion status'; loading = false;
        }
    }
    function choose(job: TranscodingJob) {
        selectedId = selectedId === job.id ? null : job.id;
        selected = selectedId ? job : null; detailError = null;
        if (selectedId) load();
    }
    function selectTab(tab: 'active' | 'recent') { activeTab = tab; load(); }
    function visible() { if (!document.hidden) load(); else controller?.abort(); }

    onMount(() => {
        const params = new URLSearchParams(window.location.hash.split('?')[1] || '');
        selectedId = params.get('job'); expectedBoot = params.get('boot');
        load(); timer = setInterval(() => load(), 5000);
        document.addEventListener('visibilitychange', visible);
    });
    onDestroy(() => { controller?.abort(); if (timer) clearInterval(timer); document.removeEventListener('visibilitychange', visible); });
</script>

<div class="page-header">
    <div><h1 class="page-title">Transcoding</h1><p class="text-secondary text-sm subtitle">Audio compatibility, conversion tasks, and reusable media segments.</p></div>
    {#if page}<span class="text-xs text-secondary">Updated {timestamp(page.at)}</span>{/if}
</div>

<div class="page-body">
    {#if error}<div class="error-message" role="alert">{error}{page ? ` · Showing the last available snapshot from ${timestamp(page.at)}.` : ''}</div>{/if}
    {#if loading}<p class="text-secondary">Loading conversion status…</p>{/if}
    {#if page}
        <TranscodingSummary value={page.aggregate} />
        {#if !page.aggregate.enabled}
            <div class="panel"><h2>Audio conversion is disabled</h2><p class="text-secondary">Enable audio conversion in the gateway deployment to prepare compatible audio for clients that need it.</p></div>
        {:else}
            <div class="panel resources">
                <div><span class="metric-label">Idle tasks</span><strong>{page.aggregate.ready} registered plans</strong><span class="text-secondary text-xs">These tasks have no active conversion process.</span></div>
                <div><span class="metric-label">Cache in use</span><strong>{size(page.aggregate.pinned_bytes)} pinned</strong><span class="text-secondary text-xs">{size(page.aggregate.reserved_bytes)} being written</span></div>
                <div><span class="metric-label">Cache activity</span><strong>{page.aggregate.cache_hits} hits · {page.aggregate.cache_evictions} evictions</strong><span class="text-secondary text-xs">Counts since gateway startup</span></div>
                <div class="version"><span class="metric-label">Media runtime</span><span class="text-sm">{page.aggregate.ffmpeg_version}</span><span class="text-secondary text-xs">Video is copied without re-encoding.</span></div>
            </div>

            <div class="panel tasks-panel">
                <div class="task-heading">
                    <div class="segmented-control" aria-label="Conversion task view">
                        <button class:active={activeTab === 'active'} class="tab" onclick={() => selectTab('active')}>Active</button>
                        <button class:active={activeTab === 'recent'} class="tab" onclick={() => selectTab('recent')}>Recent</button>
                    </div>
                    <span class="text-secondary text-sm">{activeTab === 'active' ? 'Registered playback tasks' : 'Latest stopped and failed tasks'}</span>
                </div>
                {#if rows.length === 0}
                    <p class="empty-state">{activeTab === 'active' ? 'No audio conversion tasks. Compatible media continues to play directly.' : 'No recent conversion outcomes.'}</p>
                {:else}
                    <div class="table-scroll"><table>
                        <thead><tr><th>Playback</th><th>User / device</th><th>Audio</th><th>State</th><th>Cached</th><th></th></tr></thead>
                        <tbody>{#each rows as job (job.id)}
                            <tr class:selected-row={selectedId === job.id}>
                                <td><strong>{job.item_name || job.item_id}</strong><div class="text-xs text-secondary source-name">{job.source_name}</div></td>
                                <td>{job.username || job.user_id}<div class="text-xs text-secondary">{job.device}</div></td>
                                <td><span class="mono">{job.audio_source.toUpperCase()} {channels(job.audio_source_channels)}</span><div class="text-xs">→ {job.audio_output.toUpperCase()} {channels(job.audio_output_channels)}</div></td>
                                <td><span class:status-err={job.state === 'failed'} class:status-ok={job.state === 'producing'}>{stateNames[job.state] || job.state}</span>{#if job.paused}<div class="text-xs text-secondary">Playback paused</div>{/if}</td>
                                <td class="mono">{size(job.cache_bytes)}<div class="text-xs text-secondary">{job.cached_segments} segments</div></td>
                                <td><button class="secondary" aria-expanded={selectedId === job.id} aria-label={`Details for ${job.item_name || job.item_id}`} onclick={() => choose(job)}>Details</button></td>
                            </tr>
                        {/each}</tbody>
                    </table></div>
                    {#if activeTab === 'active' && pageCursor}<button class="secondary load-more" onclick={() => { pageCursor = ''; load(); }}>First page</button>{/if}
                    {#if activeTab === 'active' && page.next_cursor}<button class="secondary load-more" onclick={() => load(true)}>Next page</button>{/if}
                {/if}
            </div>

            {#if detailError}<div class="error-message" role="alert">{detailError}</div>{/if}
            {#if selected}
                <section class="panel" aria-label="Conversion task details">
                    <div class="detail-heading"><div><h2>{selected.item_name || selected.item_id}</h2><p class="text-secondary text-sm">{reasons[selected.reason] || selected.reason}</p></div><button class="secondary" onclick={() => { selectedId = null; selected = null; }}>Close details</button></div>
                    {#if selected.failure}<div class="error-message">{reasons[selected.failure] || selected.failure}</div>{/if}
                    <div class="details-grid">
                        <div><span class="metric-label">Reported playback position</span><strong class="mono">{clock(selected.position_ticks)}</strong><small>Reported at {timestamp(selected.position_at)}</small></div>
                        <div><span class="metric-label">Requested media position</span><strong class="mono">{clock(selected.requested_ticks)}</strong><small>Current generation request</small></div>
                        <div><span class="metric-label">Produced through</span><strong class="mono">{clock(selected.produced_ticks)}</strong><small>Generation progress, not watched progress</small></div>
                        <div><span class="metric-label">Generation speed</span><strong class="mono">{selected.speed == null ? '—' : `${selected.speed.toFixed(1)}×`}</strong><small>{selected.runs} process runs</small></div>
                        <div><span class="metric-label">Worker CPU</span><strong class="mono">{selected.cpu_percent == null ? '—' : `${selected.cpu_percent.toFixed(1)}%`}</strong><small>100% is one CPU core</small></div>
                        <div><span class="metric-label">Worker resident memory</span><strong class="mono">{size(selected.rss_bytes)}</strong><small>Separate from the gateway Go heap</small></div>
                    </div>
                    <div class="range-heading"><span class="metric-label">Available media in the segment cache</span><span class="mono text-xs">0:00 — {clock(selected.duration_ticks)}</span></div>
                    <div class="range-track" role="img" aria-label={`${selected.cached_segments} cached segments; playback position ${clock(selected.position_ticks)}`}>
                        {#each selected.ranges as range}<span class="range-fill" style:left={`${percent(range.start_ticks, selected.duration_ticks)}%`} style:width={`${percent(range.end_ticks - range.start_ticks, selected.duration_ticks)}%`}></span>{/each}
                        {#if selected.position_ticks != null}<span class="playhead" style:left={`${percent(selected.position_ticks, selected.duration_ticks)}%`}></span>{/if}
                    </div>
                    <p class="text-secondary text-xs">Highlighted intervals are reusable server segments. The marker is the last reported playback position.{selected.ranges_truncated ? ' Only the first 64 retained intervals are shown.' : ''}</p>
                    <p class="text-secondary text-xs">Task <span class="mono">{selected.id}</span> · Started {timestamp(selected.created_at)} · Last activity {timestamp(selected.last_seen)}</p>
                </section>
            {/if}
        {/if}
    {/if}
</div>

<style>
    .subtitle { margin: .4rem 0 0; }
    .resources, .details-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(185px, 1fr)); gap: 1.4rem; }
    .resources > div, .details-grid > div { display: flex; flex-direction: column; gap: .45rem; min-width: 0; }
    .resources strong, .details-grid strong { font-weight: 500; }
    .resources .version { overflow-wrap: anywhere; }
    .task-heading, .detail-heading, .range-heading { display: flex; justify-content: space-between; gap: 1rem; align-items: center; }
    .task-heading, .detail-heading { margin-bottom: 1.2rem; flex-wrap: wrap; }
    .detail-heading h2 { margin: 0; font-size: 1rem; }
    .detail-heading p { margin-bottom: 0; }
    .table-scroll { overflow-x: auto; }
    table { width: 100%; border-collapse: collapse; font-size: .82rem; }
    th, td { text-align: left; padding: .85rem .65rem; border-bottom: 1px solid var(--border-color); vertical-align: top; }
    th { color: var(--text-secondary); font-size: .75rem; font-weight: 500; }
    td strong { font-weight: 500; }
    td > div { margin-top: .35rem; }
    .source-name { max-width: 280px; overflow-wrap: anywhere; }
    .selected-row { background: var(--bg-hover, #242424); }
    .empty-state { padding: 2rem 0; text-align: center; color: var(--text-secondary); font-size: .875rem; }
    .load-more { margin-top: 1rem; }
    .details-grid { margin: 1.5rem 0; }
    small { color: var(--text-secondary); font-size: .72rem; }
    .range-heading { margin-bottom: .6rem; }
    .range-track { height: 14px; border-radius: 4px; background: var(--bg-hover, #292929); position: relative; overflow: hidden; }
    .range-fill { position: absolute; height: 100%; background: var(--success, #56aa7e); }
    .playhead { position: absolute; height: 100%; width: 2px; background: var(--text-primary); }
    @media (max-width: 700px) { .page-header { flex-wrap: wrap; } .range-heading { align-items: flex-start; flex-direction: column; } }
</style>
