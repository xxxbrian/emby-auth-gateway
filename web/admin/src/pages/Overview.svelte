<script>
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api.js';

    let data = null;
    let error = null;
    let timer;

    async function fetchData() {
        try {
            data = await apiRequest('/overview');
            error = null;
        } catch (err) {
            error = err.message;
        }
    }

    function fmtMbps(v) {
        if (v == null || Number.isNaN(v)) return '0.00';
        return Number(v).toFixed(2);
    }

    function fmtPct(v) {
        if (v == null || Number.isNaN(v)) return '0.00';
        return (Number(v) * 100).toFixed(2);
    }

    function upstreamLabel(u) {
        if (!u) return 'Unknown';
        if (u.last_error_at && (!u.last_ok_at || new Date(u.last_error_at) > new Date(u.last_ok_at))) {
            return `Error · ${u.last_error_kind || u.last_status_class || 'failed'}`;
        }
        if (u.last_ok_at) {
            const ms = u.last_latency_ms != null ? `${u.last_latency_ms}ms` : 'ok';
            return `OK · ${ms}`;
        }
        return 'Unknown';
    }

    function upstreamClass(u) {
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

    onDestroy(() => clearInterval(timer));
</script>

<h1 class="page-title">Overview</h1>

{#if error}
    <div class="error-message">Error fetching overview: {error}</div>
{/if}

{#if data}
    <div class="panel status-bar">
        <div>
            <div class="text-sm text-secondary">Upstream</div>
            <div class={upstreamClass(data.upstream)}>{upstreamLabel(data.upstream)}</div>
        </div>
        <div>
            <div class="text-sm text-secondary">Auth</div>
            <div class={data.upstream?.auth_ok ? 'status-ok' : 'status-warn'}>
                {data.upstream?.auth_ok ? 'OK' : (data.upstream?.last_auth_error || 'Unknown')}
            </div>
        </div>
        <div>
            <div class="text-sm text-secondary">Uptime</div>
            <div>{data.uptime_sec ?? 0}s</div>
        </div>
        <div>
            <div class="text-sm text-secondary">Boot</div>
            <div class="mono text-sm">{data.boot_id || '-'}</div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Capacity</h3>
        <div class="stat-grid">
            <div class="stat-box">
                <div class="text-sm text-secondary">Active Playbacks</div>
                <div class="stat-value">{data.capacity?.active_playbacks ?? 0}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Media Transfers</div>
                <div class="stat-value">{data.capacity?.active_media_transfers ?? 0}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Active Sessions</div>
                <div class="stat-value">{data.capacity?.active_sessions ?? 0}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Reject Rate (5m)</div>
                <div class="stat-value {(data.capacity?.reject_rate_5m || 0) > 0.05 ? 'status-err' : 'status-ok'}">
                    {fmtPct(data.capacity?.reject_rate_5m)}%
                </div>
            </div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Traffic</h3>
        <div class="stat-grid">
            <div class="stat-box">
                <div class="text-sm text-secondary">RPS</div>
                <div class="stat-value">{fmtMbps(data.traffic?.rps)}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Out (Mbps)</div>
                <div class="stat-value">{fmtMbps(data.traffic?.mbps_out)}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">In (Mbps)</div>
                <div class="stat-value">{fmtMbps(data.traffic?.mbps_in)}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Error Rate (15m)</div>
                <div class="stat-value {(data.traffic?.error_rate_15m || 0) > 0.05 ? 'status-err' : 'status-ok'}">
                    {fmtPct(data.traffic?.error_rate_15m)}%
                </div>
            </div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Reliability & Runtime</h3>
        <div class="stat-grid">
            <div class="stat-box">
                <div class="text-sm text-secondary">Userdata Write Fail (5m)</div>
                <div class="stat-value {(data.reliability?.userdata_write_fail_5m || 0) > 0 ? 'status-err' : 'status-ok'}">
                    {data.reliability?.userdata_write_fail_5m ?? 0}
                </div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Overlay Fail (5m)</div>
                <div class="stat-value {(data.reliability?.overlay_fail_5m || 0) > 0 ? 'status-err' : 'status-ok'}">
                    {data.reliability?.overlay_fail_5m ?? 0}
                </div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Telemetry Drops</div>
                <div class="stat-value {(data.reliability?.telemetry_drops || 0) > 0 ? 'status-warn' : 'status-ok'}">
                    {data.reliability?.telemetry_drops ?? 0}
                </div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Goroutines / Heap</div>
                <div class="stat-value text-base">
                    {data.runtime?.goroutines ?? 0}
                    <span class="text-secondary text-sm"> / {((data.runtime?.heap_bytes || 0) / 1024 / 1024).toFixed(1)} MB</span>
                </div>
            </div>
        </div>
    </div>
{:else if !error}
    <div class="text-secondary">Loading…</div>
{/if}

<style>
    .status-bar {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
        gap: 1rem;
    }
    .stat-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
        gap: 1rem;
        margin-top: 1rem;
    }
    .stat-box {
        background: rgba(255,255,255,0.02);
        border: 1px solid var(--border-color);
        padding: 1rem;
        border-radius: 6px;
    }
    .stat-value {
        font-size: 1.5rem;
        font-weight: 600;
        margin-top: 0.5rem;
    }
    .text-base { font-size: 1.1rem; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
</style>
