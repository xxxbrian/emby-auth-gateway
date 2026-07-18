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

    onMount(() => {
        fetchData();
        timer = setInterval(fetchData, 2000);
    });

    onDestroy(() => {
        clearInterval(timer);
    });
</script>

<h1 class="page-title">Overview</h1>

{#if error}
    <div class="error-message">Error fetching overview: {error}</div>
{/if}

{#if data}
    <div class="panel">
        <h3 style="margin-top:0">Gateway Status</h3>
        <div class="flex gap-4 mt-4" style="flex-wrap: wrap">
            <div class="stat-box">
                <div class="text-sm text-secondary">Active Playbacks</div>
                <div class="stat-value">{data.active_playbacks || 0}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Active Transfers</div>
                <div class="stat-value">{data.active_transfers || 0}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">RPS (1m)</div>
                <div class="stat-value">{data.requests_per_second_1m ? data.requests_per_second_1m.toFixed(2) : '0.00'}</div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Bandwidth (1m)</div>
                <div class="stat-value">
                    {#if data.bytes_out_per_second_1m}
                        {(data.bytes_out_per_second_1m * 8 / 1000000).toFixed(2)} Mbps
                    {:else}
                        0 Mbps
                    {/if}
                </div>
            </div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Reliability</h3>
        <div class="flex gap-4 mt-4" style="flex-wrap: wrap">
            <div class="stat-box">
                <div class="text-sm text-secondary">Error Rate (1m)</div>
                <div class="stat-value {data.error_rate_1m > 0.05 ? 'status-err' : data.error_rate_1m > 0.01 ? 'status-warn' : 'status-ok'}">
                    {data.error_rate_1m ? (data.error_rate_1m * 100).toFixed(2) : '0.00'}%
                </div>
            </div>
            <div class="stat-box">
                <div class="text-sm text-secondary">Reject Rate (1m)</div>
                <div class="stat-value {data.reject_rate_1m > 0.05 ? 'status-err' : data.reject_rate_1m > 0.01 ? 'status-warn' : 'status-ok'}">
                    {data.reject_rate_1m ? (data.reject_rate_1m * 100).toFixed(2) : '0.00'}%
                </div>
            </div>
        </div>
    </div>
{:else if !error}
    <div>Loading...</div>
{/if}

<style>
    .stat-box {
        background: rgba(255,255,255,0.02);
        border: 1px solid var(--border-color);
        padding: 1rem;
        border-radius: 6px;
        min-width: 150px;
    }
    .stat-value {
        font-size: 1.5rem;
        font-weight: 600;
        margin-top: 0.5rem;
    }
</style>
