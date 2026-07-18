<script>
    import { onMount } from 'svelte';
    import { apiRequest } from '../lib/api.js';

    let data = [];
    let loading = true;
    let error = null;

    async function loadData() {
        loading = true;
        try {
            const res = await apiRequest('/audit');
            data = res.items || [];
            error = null;
        } catch (err) {
            error = err.message;
        } finally {
            loading = false;
        }
    }

    onMount(() => {
        loadData();
    });
</script>

<div class="flex justify-between items-center mb-4">
    <h1 class="page-title" style="margin: 0">Traffic Audit</h1>
    <button class="secondary" on:click={loadData} disabled={loading}>Refresh</button>
</div>

{#if error}
    <div class="error-message">{error}</div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    <table class="mobile-cards">
        <thead>
            <tr>
                <th>Time</th>
                <th>Method</th>
                <th>Path</th>
                <th>Status</th>
                <th>Duration (ms)</th>
                <th>User ID</th>
                <th>IP</th>
            </tr>
        </thead>
        <tbody>
            {#if loading && data.length === 0}
                <tr><td colspan="7" style="text-align: center; padding: 1.5rem">Loading...</td></tr>
            {:else if data.length === 0}
                <tr><td colspan="7" style="text-align: center; padding: 1.5rem">No recent audit events.</td></tr>
            {/if}
            {#each data as item}
                <tr>
                    <td data-label="Time">{new Date(item.timestamp || item.created).toLocaleString()}</td>
                    <td data-label="Method"><strong>{item.method}</strong></td>
                    <td data-label="Path" class="text-sm" style="word-break: break-all; max-width: 250px;">{item.path}</td>
                    <td data-label="Status">
                        <span class={item.status >= 500 ? 'status-err' : item.status >= 400 ? 'status-warn' : 'status-ok'}>
                            {item.status}
                        </span>
                    </td>
                    <td data-label="Duration">{item.duration_ms}</td>
                    <td data-label="User ID">{item.user_id || '-'}</td>
                    <td data-label="IP">{item.ip || '-'}</td>
                </tr>
            {/each}
        </tbody>
    </table>
</div>
