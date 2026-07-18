<script lang="ts">
    import { onMount } from 'svelte';
    import { apiRequest } from '../lib/api';
    import type { AuditDTO, ItemsResponse } from '../lib/types';

    let data = $state<AuditDTO[]>([]);
    let loading = $state(true);
    let error = $state<string | null>(null);

    async function loadData() {
        loading = true;
        try {
            const res = await apiRequest<ItemsResponse<AuditDTO>>('/audit?limit=50');
            data = res.items || [];
            error = null;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    function fmtTime(v: string | undefined): string {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }

    onMount(loadData);
</script>

<div class="flex justify-between items-center mb-4">
    <h1 class="page-title" style="margin: 0">Traffic & Audit</h1>
    <button class="secondary" onclick={loadData} disabled={loading}>Refresh</button>
</div>

{#if error}
    <div class="error-message">{error}</div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    <table class="mobile-cards">
        <thead>
            <tr>
                <th>Time</th>
                <th>Event</th>
                <th>Method</th>
                <th>Path</th>
                <th>Status</th>
                <th>Duration</th>
                <th>User</th>
                <th>IP</th>
            </tr>
        </thead>
        <tbody>
            {#if loading && data.length === 0}
                <tr><td colspan="8" class="empty">Loading…</td></tr>
            {:else if data.length === 0}
                <tr><td colspan="8" class="empty">No recent audit events.</td></tr>
            {/if}
            {#each data as item}
                <tr>
                    <td data-label="Time">{fmtTime(item.created)}</td>
                    <td data-label="Event">{item.event || '-'}</td>
                    <td data-label="Method"><strong>{item.method || '-'}</strong></td>
                    <td data-label="Path" class="path">{item.path || '-'}</td>
                    <td data-label="Status">
                        <span class={(item.status ?? 0) >= 500 ? 'status-err' : (item.status ?? 0) >= 400 ? 'status-warn' : 'status-ok'}>
                            {item.status || '-'}
                        </span>
                    </td>
                    <td data-label="Duration">{item.duration_ms != null ? `${item.duration_ms}ms` : '-'}</td>
                    <td data-label="User">{item.gateway_user_id || item.synthetic_user_id || '-'}</td>
                    <td data-label="IP">{item.remote_ip || '-'}</td>
                </tr>
            {/each}
        </tbody>
    </table>
</div>

<style>
    .empty { text-align: center; padding: 1.5rem; color: var(--text-secondary); }
    .path { word-break: break-all; max-width: 280px; font-size: 0.85rem; }
</style>
