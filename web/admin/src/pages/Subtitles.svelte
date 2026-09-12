<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest, ApiError } from '../lib/api';
    import type { SubtitleJob, SubtitlePage, SubtitleTrack } from '../lib/subtitle-types';

    let page = $state<SubtitlePage | null>(null);
    let selectedId = $state<string | null>(null);
    let selected = $derived(page?.jobs.find(job => job.id === selectedId) ?? null);
    let cursor = $state('');
    let error = $state<string | null>(null);
    let loading = $state(true);
    let receivedAt = $state<Date | null>(null);
    let controller: AbortController | null = null;
    let timer: ReturnType<typeof setInterval> | undefined;

    const states: Record<string, string> = {
        ready: 'Ready', unknown: 'Not prepared', probing: 'Checking upstream',
        extracting: 'Extracting', preparing: 'Preparing', queued: 'Queued',
        unavailable: 'Hidden', deferred: 'Deferred', idle: 'Idle', stopped: 'Stopped',
    };
    const reasons: Record<string, string> = {
        upstream_empty: 'The upstream subtitle response is empty.',
        upstream_unavailable: 'The upstream subtitle could not be retrieved.',
        source_unavailable: 'The original media source could not be read or validated.',
        source_validator_missing: 'The source has no strong ETag. Local extraction is unavailable without reliable source identity.',
        unsupported: 'This subtitle format cannot be prepared for Web playback.',
        invalid_subtitle: 'The upstream response is not a valid subtitle document.',
        resource_limit: 'Preparation reached its source-read or request budget.',
        missing_index: 'A usable subtitle index is unavailable. Full-file scanning is disabled.',
        empty_indexed_packets: 'The indexed source packets contain no usable subtitles.',
        canceled_or_timeout: 'Preparation stopped or exceeded its time limit.',
        cache_capacity_or_storage: 'The subtitle cache is full or storage is unavailable.',
        capacity_exhausted: 'Preparation is waiting for available resources.',
        no_web_viewers: 'The last Web viewer left.',
        source_changed: 'The source version changed; subtitles need to be prepared again.',
        configuration_invalid: 'The optional subtitle feature could not start with this configuration.',
        initialization_failed: 'The optional subtitle feature could not initialize its cache or resources.',
        startup_failed: 'The optional subtitle feature could not start.',
        invalid_configuration: 'The optional subtitle configuration is invalid.',
        context_unavailable: 'The gateway could not initialize Web context verification.',
        web_unavailable: 'The local Emby Web surface is unavailable.',
        cache_unavailable: 'The subtitle cache could not be initialized.',
    };

    function size(bytes: number): string {
        if (bytes >= 1024 ** 3) return `${(bytes / 1024 ** 3).toFixed(2)} GiB`;
        if (bytes >= 1024 ** 2) return `${(bytes / 1024 ** 2).toFixed(1)} MiB`;
        if (bytes >= 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
        return `${bytes} B`;
    }
    function time(value: string): string { return new Date(value).toLocaleString(); }
    function readyCount(job: SubtitleJob): number { return job.tracks.filter(track => track.state === 'ready').length; }
    function stateName(state: string): string { return states[state] ?? state.replaceAll('_', ' '); }
    function modeName(mode?: string): string {
        return mode === 'indexed' ? 'Indexed extraction' : mode === 'upstream' ? 'Upstream subtitle' : 'Not started';
    }
    function trackReason(track: SubtitleTrack): string {
        if (track.reason) return reasons[track.reason] ?? track.reason.replaceAll('_', ' ');
        if (track.state === 'ready') return 'Available to eligible Web playback.';
        if (track.state === 'unknown') return 'Hidden until preparation succeeds.';
        if (['probing', 'extracting', 'preparing', 'queued'].includes(track.state)) return 'Hidden while preparation is pending.';
        return 'Unavailable tracks are hidden from Web subtitle choices.';
    }

    async function load(nextCursor = cursor) {
        if (document.hidden) return;
        controller?.abort();
        const ctrl = new AbortController();
        controller = ctrl;
        loading = true;
        try {
            const params = new URLSearchParams({ limit: '8' });
            if (nextCursor) params.set('cursor', nextCursor);
            const result = await apiRequest<SubtitlePage>(`/subtitles?${params}`, { signal: ctrl.signal });
            if (ctrl.signal.aborted) return;
            if (page?.boot_id !== result.boot_id || cursor !== nextCursor) selectedId = null;
            page = result;
            cursor = nextCursor;
            receivedAt = new Date();
            error = null;
        } catch (err) {
            if (ctrl.signal.aborted) return;
            if (err instanceof ApiError && err.code === 'stale_cursor') {
                selectedId = null;
                await load('');
                return;
            }
            error = err instanceof Error ? err.message : 'Unable to load subtitle status';
        } finally {
            if (controller === ctrl) loading = false;
        }
    }
    function visible() {
        if (document.hidden) controller?.abort();
        else load();
    }
    onMount(() => {
        load();
        timer = setInterval(() => load(), 5000);
        document.addEventListener('visibilitychange', visible);
    });
    onDestroy(() => {
        controller?.abort();
        if (timer) clearInterval(timer);
        document.removeEventListener('visibilitychange', visible);
    });
</script>

<div class="page-header">
    <div>
        <h1 class="page-title">Web subtitles</h1>
        <p class="text-secondary text-sm intro">Subtitle preparation for verified Web playback. Native clients retain their original subtitle tracks.</p>
    </div>
    {#if page}<span class:status-ok={page.enabled} class="feature-state">{page.enabled ? 'Enabled' : 'Disabled'}</span>{/if}
</div>

<div class="page-body">
    {#if error}<div class="error-message" role="alert">{error}{page && receivedAt ? ` · Showing the last snapshot from ${receivedAt.toLocaleTimeString()}.` : ''}</div>{/if}
    {#if loading && !page}<p class="text-secondary">Loading subtitle status…</p>{/if}
    {#if page}
        {#if !page.enabled}
            <section class="panel" aria-label="Subtitle feature status">
                <h2>Web subtitle preparation is disabled</h2>
                <p class="text-secondary text-sm">This deployment does not start subtitle preparation or populate its source cache.</p>
                {#if page.reason}<p class="text-secondary text-sm">{reasons[page.reason] ?? page.reason.replaceAll('_', ' ')}</p>{/if}
            </section>
        {:else}
            <div class="resources">
                <div class="metric-box"><div class="metric-label">Preparation workers</div><div class="metric-value">{page.active} / {page.workers}</div><p class="text-secondary text-xs">Active / configured workers</p></div>
                <div class="metric-box"><div class="metric-label">Subtitle results</div><div class="metric-value">{size(page.cache_bytes)}</div><p class="text-secondary text-xs">of {size(page.cache_budget)} cache budget</p></div>
                <div class="metric-box"><div class="metric-label">Shared source ranges</div><div class="metric-value">{size(page.source_cache_bytes)}</div><p class="text-secondary text-xs">of {size(page.source_cache_budget)} cache budget</p></div>
                <div class="metric-box"><div class="metric-label">Source data read</div><div class="metric-value">{size(page.source_read_bytes)}</div><p class="text-secondary text-xs">{size(page.source_hit_bytes)} reused from cache · since startup</p></div>
            </div>
            <p class="text-secondary text-xs traffic-note">Source data includes subtitle preparation and shared Web playback reads. Cache reuse is counted separately.</p>

            <section class="panel" aria-label="Subtitle preparation tasks">
                <div class="section-heading"><h2>Prepared sources</h2><span class="text-secondary text-xs">{page.total_jobs} registered sources{receivedAt ? ` · Updated ${receivedAt.toLocaleTimeString()}` : ''}</span></div>
                <p class="text-secondary text-sm scope">Only ready tracks appear in Web subtitle choices. Temporary failures can be retried during a later Web request.</p>
                {#if page.jobs.length === 0}
                    <p class="empty-state">No Web subtitle preparation tasks. Native playback does not start this work.</p>
                {:else}
                    <div class="table-scroll"><table>
                        <thead><tr><th>Source</th><th>State</th><th>Web viewers</th><th>Ready tracks</th><th>Updated</th><th><span class="text-secondary">Details</span></th></tr></thead>
                        <tbody>{#each page.jobs as job (job.id)}
                            <tr class:selected-row={selectedId === job.id}>
                                <td class="source-name"><strong>{job.source_name || `Item ${job.item_id}`}</strong><div class="text-secondary text-xs">Item {job.item_id}{job.source_size ? ` · ${size(job.source_size)} source` : ''}</div></td>
                                <td><span class:status-ok={job.state === 'preparing'}>{stateName(job.state)}</span></td>
                                <td>{job.viewers}</td>
                                <td>{readyCount(job)} / {job.tracks.length}</td>
                                <td class="text-xs text-secondary">{time(job.updated_at)}</td>
                                <td><button class="secondary" aria-label={`Subtitle details for ${job.source_name || job.item_id}`} aria-expanded={selectedId === job.id} onclick={() => { selectedId = selectedId === job.id ? null : job.id; }}>Details</button></td>
                            </tr>
                        {/each}</tbody>
                    </table></div>
                    {#if cursor || page.next_cursor}
                        <div class="pagination">
                            {#if cursor}<button class="secondary" disabled={loading} onclick={() => load('')}>First page</button>{/if}
                            {#if page.next_cursor}<button class="secondary" disabled={loading} onclick={() => load(page?.next_cursor || '')}>Next page</button>{/if}
                        </div>
                    {/if}
                {/if}
            </section>

            {#if selected}
                <section class="panel" aria-label="Subtitle track details">
                    <div class="section-heading"><div><h2>{selected.source_name || `Item ${selected.item_id}`}</h2><p class="text-secondary text-xs">Source {selected.source_id}{selected.source_size ? ` · ${size(selected.source_size)}` : ''} · Registered {time(selected.created_at)}</p></div><button class="secondary" onclick={() => { selectedId = null; }}>Close details</button></div>
                    {#if selected.tracks.length === 0}<p class="empty-state">No subtitle tracks were reported for this source.</p>{:else}
                        <div class="table-scroll"><table class="tracks">
                            <thead><tr><th>Track</th><th>Web availability</th><th>Subtitle output</th><th>Preparation</th></tr></thead>
                            <tbody>{#each selected.tracks as track (track.index)}
                                <tr>
                                    <td class="track-name"><strong>{track.name || track.language || `Track ${track.index}`}</strong><div class="text-secondary text-xs">#{track.index}{track.language ? ` · ${track.language}` : ''}</div></td>
                                    <td class="availability"><span class:status-ok={track.state === 'ready'} class:status-warn={track.state === 'unavailable' || track.state === 'deferred'}>{stateName(track.state)}</span><p class="text-secondary text-xs">{trackReason(track)}</p></td>
                                    <td><span class="mono">{size(track.bytes)}</span><div class="text-secondary text-xs">{track.cues} cues{track.format ? ` · ${track.format.toUpperCase()}` : ''}</div>{#if track.state === 'ready'}<div class="text-secondary text-xs">{track.persistent ? 'Reusable across restarts' : 'Current process cache'}</div>{/if}</td>
                                    <td>{modeName(track.mode)}<div class="mono text-xs">{size(track.read_bytes)} indexed reads</div><div class="text-secondary text-xs">{track.requests} range requests</div></td>
                                </tr>
                            {/each}</tbody>
                        </table></div>
                    {/if}
                    <p class="text-secondary text-xs">Indexed reads include cache hits. Only reusable, source-validated subtitle results survive a gateway restart.</p>
                    <p class="text-secondary text-xs task-id">Task <span class="mono">{selected.id}</span></p>
                </section>
            {/if}
        {/if}
    {/if}
</div>

<style>
    .intro { margin: .4rem 0 0; max-width: 660px; }
    .feature-state { border: 1px solid var(--border-color); padding: .35rem .65rem; border-radius: 3px; font-size: .8rem; }
    .resources { display: grid; grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); gap: 1rem; margin-bottom: 1.5rem; }
    .resources p { margin: .6rem 0 0; }
    .traffic-note { margin: -.7rem 0 1.5rem; }
    h2 { font-size: 1rem; font-weight: 500; margin: 0; overflow-wrap: anywhere; }
    .section-heading { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 1rem; margin-bottom: 1rem; }
    .section-heading > div { min-width: 0; }
    .section-heading p { margin-bottom: 0; overflow-wrap: anywhere; }
    .scope { margin: 0 0 1rem; }
    .table-scroll { width: 100%; overflow-x: auto; }
    table { width: 100%; min-width: 680px; border-collapse: collapse; font-size: .82rem; }
    th, td { text-align: left; padding: .85rem .65rem; border-bottom: 1px solid var(--border-color); vertical-align: top; }
    th { color: var(--text-secondary); font-size: .75rem; font-weight: 500; }
    td strong { font-weight: 500; }
    td > div { margin-top: .35rem; }
    .source-name { max-width: 280px; overflow-wrap: anywhere; }
    .track-name { width: 26%; overflow-wrap: anywhere; }
    .availability { width: 40%; }
    .availability p { margin: .4rem 0 0; }
    .selected-row { background: var(--bg-hover, #242424); }
    .empty-state { padding: 2rem 0; text-align: center; color: var(--text-secondary); font-size: .875rem; }
    .pagination { display: flex; gap: .75rem; margin-top: 1rem; }
    .task-id { margin: 1rem 0 0; overflow-wrap: anywhere; }
    @media (max-width: 700px) { .page-header { flex-wrap: wrap; } .resources { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
    @media (max-width: 380px) { .resources { grid-template-columns: 1fr; } }
</style>
