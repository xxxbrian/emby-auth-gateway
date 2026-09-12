<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api';
    import type { AuditPage, AuditRecord } from '../lib/audit-types';

    let data = $state<AuditRecord[]>([]);
    let loading = $state(false);
    let error = $state<string | null>(null);
    let view = $state<'audit' | 'errors'>('audit');
    let timeWindow = $state('24h');
    let from = $state('');
    let to = $state('');
    let direction = $state('');
    let event = $state('');
    let errorKind = $state('');
    let userId = $state('');
    let cursor = $state('');
    let hasMore = $state(false);
    let selected = $state<AuditRecord | null>(null);
    let detailError = $state<string | null>(null);
    let detailLoading = $state(false);
    let appliedQuery = '';
    let request: AbortController | null = null;
    let detailRequest: AbortController | null = null;
    let selectionButton: HTMLButtonElement | null = null;

    const durations: Record<string, number> = { '15m': 15, '1h': 60, '6h': 360, '24h': 1440 };
    function localInput(date: Date): string {
        return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
    }
    function setRange(w: string) {
        timeWindow = w;
        const now = new Date();
        to = localInput(now);
        from = localInput(new Date(now.getTime() - (durations[w] || 1440) * 60000));
    }
    function writeURL(record?: string) {
        const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
        for (const key of ['view', 'window', 'from', 'to', 'direction', 'event', 'error_kind', 'user_id', 'record']) q.delete(key);
        q.set('view', view); q.set('window', timeWindow);
        if (from && to) { q.set('from', new Date(from).toISOString()); q.set('to', new Date(to).toISOString()); }
        if (direction) q.set('direction', direction);
        if (event.trim()) q.set('event', event.trim());
        if (errorKind.trim()) q.set('error_kind', errorKind.trim());
        if (userId.trim()) q.set('user_id', userId.trim());
        if (record) q.set('record', record);
        window.history.replaceState(null, '', `#/traffic?${q}`);
    }
    function makeQuery(): string | null {
        const start = new Date(from), end = new Date(to);
        if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || end <= start || end.getTime() - start.getTime() > 86400000) {
            error = 'Choose a positive time range of up to 24 hours.'; return null;
        }
        const q = new URLSearchParams({ from: start.toISOString(), to: end.toISOString(), limit: '50', view });
        if (direction) q.set('direction', direction);
        if (event.trim()) q.set('event', event.trim());
        if (errorKind.trim()) q.set('error_kind', errorKind.trim());
        if (userId.trim()) q.set('user_id', userId.trim());
        return q.toString();
    }
    async function loadData(append = false) {
        const query = append ? appliedQuery : makeQuery();
        if (!query) return;
        request?.abort(); const ctrl = new AbortController(); request = ctrl;
        if (!append) { appliedQuery = query; cursor = ''; hasMore = false; }
        loading = true;
        try {
            const res = await apiRequest<AuditPage>(`/audit?${query}${append && cursor ? `&cursor=${encodeURIComponent(cursor)}` : ''}`, { signal: ctrl.signal });
            if (ctrl.signal.aborted) return;
            data = append ? [...new Map([...data, ...(res.items || [])].map(row => [row.id, row])).values()] : res.items || [];
            cursor = res.next_cursor || ''; hasMore = !!res.has_more;
            error = null; writeURL(selected?.id);
        } catch (err) { if (!ctrl.signal.aborted) error = err instanceof Error ? err.message : String(err); }
        finally { if (!ctrl.signal.aborted) loading = false; }
    }
    function switchView(next: 'audit' | 'errors') { view = next; data = []; selected = null; loadData(); }
    function selectWindow(next: string) { setRange(next); loadData(); }
    function shiftWindow(amount: number) {
        const start = new Date(from), end = new Date(to), span = end.getTime() - start.getTime();
        if (!(span > 0 && span <= 86400000)) return;
        from = localInput(new Date(start.getTime() + amount * span));
        to = localInput(new Date(end.getTime() + amount * span));
        timeWindow = 'custom'; loadData();
    }
    function refresh() { if (timeWindow !== 'custom') setRange(timeWindow); loadData(); }
    async function showDetail(id: string, button?: HTMLButtonElement) {
        detailRequest?.abort(); const ctrl = new AbortController(); detailRequest = ctrl;
        selectionButton = button || null; selected = data.find(row => row.id === id) || null;
        detailLoading = true; detailError = null; writeURL(id);
        try {
            const row = await apiRequest<AuditRecord>(`/audit/${encodeURIComponent(id)}`, { signal: ctrl.signal });
            if (!ctrl.signal.aborted) selected = row;
        } catch (err) { if (!ctrl.signal.aborted) detailError = err instanceof Error ? err.message : String(err); }
        finally { if (!ctrl.signal.aborted) detailLoading = false; }
    }
    function closeDetail() {
        detailRequest?.abort(); selected = null; detailError = null; detailLoading = false;
        writeURL(); selectionButton?.focus();
    }
    function fmtTime(v?: string) { return v ? new Date(v).toLocaleString('en') : '—'; }
    function codeLabel(v?: string) { return v ? v.replaceAll('_', ' ') : 'Unknown event'; }
    function fmtBytes(v = 0) {
        if (v < 1024) return `${v} B`;
        if (v < 1048576) return `${(v / 1024).toFixed(1)} KiB`;
        return `${(v / 1048576).toFixed(1)} MiB`;
    }
    function severityClass(row: AuditRecord) { return row.severity === 'error' ? 'status-err' : row.severity === 'warning' ? 'status-warn' : 'text-secondary'; }
    onMount(() => {
        const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
        view = q.get('view') === 'errors' ? 'errors' : 'audit';
        setRange(q.get('window') && durations[q.get('window')!] ? q.get('window')! : '24h');
        if (q.get('from') && q.get('to')) {
            const start = new Date(q.get('from')!), end = new Date(q.get('to')!);
            if (Number.isFinite(start.getTime()) && Number.isFinite(end.getTime())) {
                from = localInput(start); to = localInput(end);
                if (q.get('window') === 'custom') timeWindow = 'custom';
            }
        }
        direction = q.get('direction') || ''; event = q.get('event') || ''; errorKind = q.get('error_kind') || ''; userId = q.get('user_id') || '';
        loadData(); if (q.get('record')) showDetail(q.get('record')!);
    });
    onDestroy(() => { request?.abort(); detailRequest?.abort(); });
</script>

<svelte:window onkeydown={(e) => { if (e.key === 'Escape' && (selected || detailError)) closeDetail(); }} />
<div class="page-header"><h1 class="page-title">Traffic & Audit</h1><button type="button" class="secondary" onclick={refresh} disabled={loading}>Refresh</button></div>
<div class="page-body">
    <div class="audit-toolbar">
        <div class="segmented-control" aria-label="Log view">
            <button type="button" class="tab {view === 'errors' ? 'active' : ''}" aria-pressed={view === 'errors'} onclick={() => switchView('errors')}>Errors</button>
            <button type="button" class="tab {view === 'audit' ? 'active' : ''}" aria-pressed={view === 'audit'} onclick={() => switchView('audit')}>Audit</button>
        </div>
        <div class="segmented-control" aria-label="Log time range">{#each ['15m', '1h', '6h', '24h'] as w}<button type="button" class="tab {timeWindow === w ? 'active' : ''}" aria-pressed={timeWindow === w} onclick={() => selectWindow(w)}>{w}</button>{/each}</div>
    </div>
    {#if view === 'errors'}<p class="coverage-note">Recorded failures and rejections. Telemetry counts can include events without a saved audit record. Canceled requests are excluded.</p>{/if}
    <p class="coverage-note">{fmtTime(from)} – {fmtTime(to)}</p>
    <details class="panel audit-filter-panel">
    <summary>Filter records{#if [direction, event, errorKind, userId].filter(Boolean).length > 0} · {[direction, event, errorKind, userId].filter(Boolean).length} active{/if}</summary>
    <form class="audit-filters" onsubmit={(e) => { e.preventDefault(); loadData(); }}>
        <label>From<input type="datetime-local" step="1" bind:value={from} onchange={() => timeWindow = 'custom'} /></label>
        <label>To<input type="datetime-local" step="1" bind:value={to} onchange={() => timeWindow = 'custom'} /></label>
        <label>Direction<select bind:value={direction}><option value="">All directions</option><option value="upstream">Upstream</option><option value="downstream">Downstream</option></select></label>
        <label>Error code<input type="text" bind:value={errorKind} placeholder="All error codes" maxlength="80" /></label>
        <label>Event<input type="text" bind:value={event} placeholder="All events" maxlength="255" /></label>
        <label>User ID<input type="text" bind:value={userId} placeholder="All users" maxlength="80" /></label>
        <div class="filter-actions"><button type="submit" disabled={loading}>Apply filters</button><button type="button" class="secondary" onclick={() => shiftWindow(-1)} disabled={loading}>Previous window</button><button type="button" class="secondary" onclick={() => shiftWindow(1)} disabled={loading}>Next window</button></div>
    </form>
    </details>
    {#if error}<div class="error-message" role="alert">{error}</div>{/if}
    <div class="table-container panel" style="padding:0;">
        <table class="audit-table"><thead><tr><th>Time</th><th>{view === 'errors' ? 'Failure' : 'Event'}</th><th class="col-secondary">Direction</th><th>HTTP</th><th class="col-secondary">Duration</th><th class="col-secondary">User</th></tr></thead><tbody>
            {#if data.length === 0}<tr><td colspan="6" class="empty">{loading ? 'Loading records…' : view === 'errors' ? 'No recorded failures in this time range.' : 'No audit records in this time range.'}</td></tr>{/if}
            {#each data as row (row.id)}
                <tr class:record-selected={selected?.id === row.id}>
                    <td class="mono time-cell">{fmtTime(row.created)}</td>
                    <td><button type="button" class="record-button" aria-expanded={selected?.id === row.id} aria-controls="audit-record-detail" onclick={(e) => showDetail(row.id, e.currentTarget)}><span class={severityClass(row)}>{codeLabel(row.error_kind || row.event)}</span><span class="record-subtitle">{row.message?.slice(0, 130) || row.event}</span></button></td>
                    <td class="col-secondary">{row.direction || '—'}</td><td class="mono"><span class={severityClass(row)}>{row.status || '—'}</span>{#if row.response_committed && row.is_error}<span class="record-subtitle">Interrupted</span>{/if}</td>
                    <td class="mono col-secondary">{row.duration_ms != null ? `${row.duration_ms} ms` : '—'}</td><td class="mono id-cell col-secondary">{row.gateway_user_id || row.synthetic_user_id || '—'}</td>
                </tr>
            {/each}
        </tbody></table>
    </div>
    <div class="audit-toolbar"><span class="coverage-note">{data.length} records loaded</span>{#if hasMore}<button type="button" class="secondary" onclick={() => loadData(true)} disabled={loading}>{loading ? 'Loading…' : 'Load more records'}</button>{/if}</div>
    {#if selected || detailError || detailLoading}
        <section class="panel record-detail" id="audit-record-detail" aria-label="Audit record details">
            <div class="audit-toolbar"><h2>Record details</h2><button type="button" class="secondary" onclick={closeDetail}>Close details</button></div>
            {#if detailError}<div class="error-message" role="alert">{detailError}</div>{/if}
            {#if detailLoading}<p class="coverage-note">Updating record…</p>{/if}
            {#if selected}
                <p class={severityClass(selected)}>{codeLabel(selected.error_kind || selected.event)}</p>
                {#if selected.message}<p class="record-message">{selected.message}</p>{/if}
                {#if selected.response_committed && selected.is_error}<p class="commit-note">The HTTP response had already started. Its initial status does not mean the media transfer completed successfully.</p>{/if}
                <dl class="record-grid">
                    <div><dt>Time</dt><dd>{fmtTime(selected.created)}</dd></div><div><dt>Event</dt><dd>{selected.event}</dd></div>
                    <div><dt>Error code</dt><dd>{selected.error_kind || '—'}</dd></div><div><dt>Direction</dt><dd>{selected.direction || '—'}</dd></div>
                    <div><dt>HTTP / upstream status</dt><dd>{selected.status || '—'} / {selected.upstream_status || '—'}</dd></div><div><dt>Response started</dt><dd>{selected.response_committed ? 'Yes' : 'No'}</dd></div>
                    <div><dt>Bytes transferred</dt><dd>{fmtBytes(selected.bytes_transferred)}</dd></div><div><dt>Duration</dt><dd>{selected.duration_ms ?? 0} ms</dd></div>
                    <div><dt>Request</dt><dd>{selected.method || '—'} {selected.path || '—'}</dd></div><div><dt>User</dt><dd>{selected.gateway_user_id || selected.synthetic_user_id || '—'}</dd></div>
                    <div><dt>Remote IP</dt><dd>{selected.remote_ip || '—'}</dd></div><div><dt>Record ID</dt><dd>{selected.id}</dd></div>
                </dl>
            {/if}
        </section>
    {/if}
</div>

<style>
    .audit-toolbar { display:flex; flex-wrap:wrap; align-items:center; justify-content:space-between; gap:12px; margin-bottom:16px; }
    .coverage-note { color:var(--text-secondary); font-size:12px; line-height:1.6; }
    .audit-filters { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:12px; }
    .audit-filter-panel summary { cursor:pointer; font-size:13px; }
    .audit-filter-panel[open] .audit-filters { margin-top:16px; }
    .audit-filters label { color:var(--text-secondary); font-size:12px; display:flex; flex-direction:column; gap:6px; min-width:0; }
    .filter-actions { grid-column:1/-1; display:flex; gap:8px; flex-wrap:wrap; }
    .audit-table { min-width:690px; }
    .audit-table td { vertical-align:top; }
    .time-cell { width:145px; white-space:normal; }
    .id-cell { max-width:140px; overflow-wrap:anywhere; }
    .record-button { background:none; border:0; padding:0; color:var(--text-primary); text-align:left; width:100%; }
    .record-button:hover { background:none; text-decoration:underline; }
    .record-subtitle { display:block; color:var(--text-secondary); font-size:12px; margin-top:4px; max-width:400px; overflow-wrap:anywhere; }
    .record-selected { background:rgba(37,99,235,.08); }
    .empty { text-align:center; padding:24px; color:var(--text-secondary); }
    .record-detail h2 { margin:0; font-size:16px; font-weight:500; }
    .record-grid { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:20px; margin:20px 0 0; }
    .record-grid dt { color:var(--text-secondary); font-size:12px; margin-bottom:4px; }
    .record-grid dd { margin:0; font-size:13px; overflow-wrap:anywhere; }
    .record-message { white-space:pre-wrap; overflow-wrap:anywhere; }
    .commit-note { padding-left:12px; border-left:2px solid var(--warning); font-size:13px; }
    @media(max-width:760px) { .audit-filters,.record-grid { grid-template-columns:repeat(2,minmax(0,1fr)); } }
    @media(max-width:600px) { .audit-table { min-width:0; table-layout:fixed; } .col-secondary { display:none; } .audit-table th:first-child { width:88px; } .audit-table th:nth-child(4) { width:74px; } .time-cell { font-size:11px; width:auto; } .audit-table th,.audit-table td { padding:10px 8px; } .record-subtitle { font-size:11px; } }
    @media(max-width:480px) { .audit-filters,.record-grid { grid-template-columns:1fr; } .segmented-control { flex-wrap:wrap; } }
</style>
