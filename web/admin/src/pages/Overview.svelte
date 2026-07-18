<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api';
    import type { Snapshot, UpstreamStatus } from '../lib/types';
    import LineChart from '../lib/LineChart.svelte';

    let data = $state<Snapshot | null>(null);
    let error = $state<string | null>(null);
    let timer: ReturnType<typeof setInterval> | undefined;
    let timeWindow = $state('15m');

    async function fetchData() {
        try {
            data = await apiRequest<Snapshot>(`/overview?window=${timeWindow}`);
            error = null;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        }
    }

    function setWindow(w: string) {
        timeWindow = w;
        fetchData();
    }

    function fmtMbps(v: number | null | undefined): string {
        if (v == null || Number.isNaN(v)) return '0.00';
        return Number(v).toFixed(2);
    }

    function fmtPct(v: number | null | undefined): string {
        if (v == null || Number.isNaN(v)) return '0.00';
        return (Number(v) * 100).toFixed(2);
    }

    function upstreamLabel(u: UpstreamStatus | null | undefined): string {
        if (!u) return 'Unknown';
        if (u.last_error_at && (!u.last_ok_at || new Date(u.last_error_at) > new Date(u.last_ok_at))) {
            return `ERR · ${u.last_error_kind || u.last_status_class || 'failed'}`;
        }
        if (u.last_ok_at) {
            const ms = u.last_latency_ms != null ? `${u.last_latency_ms}ms` : 'ok';
            return `OK · ${ms}`;
        }
        return 'Unknown';
    }

    function upstreamClass(u: UpstreamStatus | null | undefined): string {
        if (!u) return 'status-warn';
        if (u.last_error_at && (!u.last_ok_at || new Date(u.last_error_at) > new Date(u.last_ok_at))) {
            return 'status-err';
        }
        if (u.last_ok_at) return 'status-ok';
        return 'status-warn';
    }

    onMount(() => {
        fetchData();
        timer = setInterval(fetchData, 2000);
    });

    onDestroy(() => {
        if (timer) clearInterval(timer);
    });
</script>

<div class="page-header">
    <h1 class="page-title">Overview</h1>
    <div class="segmented-control">
        <div class="tab {timeWindow === '15m' ? 'active' : ''}" onclick={() => setWindow('15m')}>15m</div>
        <div class="tab {timeWindow === '1h' ? 'active' : ''}" onclick={() => setWindow('1h')}>1h</div>
        <div class="tab {timeWindow === '6h' ? 'active' : ''}" onclick={() => setWindow('6h')}>6h</div>
        <div class="tab {timeWindow === '24h' ? 'active' : ''}" onclick={() => setWindow('24h')}>24h</div>
    </div>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">Error fetching overview: {error}</div>
    {/if}

    {#if data}
        <div class="panel">
            <div class="data-grid" style="grid-template-columns: repeat(4, 1fr);">
                <div class="metric-box">
                    <div class="metric-label">Upstream</div>
                    <div class="metric-value {upstreamClass(data.upstream)}">{upstreamLabel(data.upstream)}</div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Auth</div>
                    <div class="metric-value {data.upstream?.auth_ok ? 'status-ok' : 'status-warn'}">
                        {data.upstream?.auth_ok ? 'OK' : (data.upstream?.last_auth_error || 'Unknown')}
                    </div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Uptime</div>
                    <div class="metric-value mono">{data.uptime_sec ?? 0}s</div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Boot ID</div>
                    <div class="metric-value mono" style="font-size: 14px;">{data.boot_id || '-'}</div>
                </div>
            </div>
        </div>

        <div class="data-grid mb-4">
            <div class="metric-box">
                <div class="metric-label">RPS</div>
                <div class="metric-value mono">{fmtMbps(data.traffic?.rps)}</div>
                <LineChart series={data.series?.rps ?? []} color="var(--text-primary)" />
            </div>
            
            <div class="metric-box">
                <div class="metric-label">Bandwidth Out</div>
                <div class="metric-value mono">{fmtMbps(data.traffic?.mbps_out)} <span class="text-xs text-secondary">Mbps</span></div>
                <LineChart series={data.series?.mbps_out ?? []} color="var(--text-primary)" />
            </div>

            <div class="metric-box">
                <div class="metric-label">Errors</div>
                <div class="metric-value mono {(data.traffic?.error_rate_15m || 0) > 0.05 ? 'status-err' : 'status-ok'}">
                    {fmtPct(data.traffic?.error_rate_15m)}%
                </div>
                <LineChart series={data.series?.errors ?? []} color="var(--danger)" />
            </div>

            <div class="metric-box">
                <div class="metric-label">Playbacks</div>
                <div class="metric-value mono">{data.capacity?.active_playbacks ?? 0}</div>
                <LineChart series={data.series?.playbacks ?? []} color="var(--text-primary)" />
            </div>
        </div>

        <div class="panel">
            <div class="metric-label mb-4">System Health</div>
            <div class="data-grid">
                <div class="metric-box">
                    <div class="metric-label">Userdata Write Fail</div>
                    <div class="metric-value mono {(data.reliability?.userdata_write_fail_5m || 0) > 0 ? 'status-err' : 'status-ok'}">
                        {data.reliability?.userdata_write_fail_5m ?? 0}
                    </div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Overlay Fail</div>
                    <div class="metric-value mono {(data.reliability?.overlay_fail_5m || 0) > 0 ? 'status-err' : 'status-ok'}">
                        {data.reliability?.overlay_fail_5m ?? 0}
                    </div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Telemetry Drops</div>
                    <div class="metric-value mono {(data.reliability?.telemetry_drops || 0) > 0 ? 'status-warn' : 'status-ok'}">
                        {data.reliability?.telemetry_drops ?? 0}
                    </div>
                </div>
                <div class="metric-box">
                    <div class="metric-label">Goroutines / Heap</div>
                    <div class="metric-value mono">
                        {data.runtime?.goroutines ?? 0}
                        <span class="text-secondary text-sm"> / {((data.runtime?.heap_bytes || 0) / 1024 / 1024).toFixed(1)} MB</span>
                    </div>
                </div>
            </div>
        </div>
    {:else if !error}
        <div class="text-secondary">Loading…</div>
    {/if}
</div>
