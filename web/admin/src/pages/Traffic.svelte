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

<div class="page-header">
    <h1 class="page-title">Traffic & Audit</h1>
    <button class="secondary" onclick={loadData} disabled={loading}>Refresh</button>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    <div class="table-container panel" style="padding: 0;">
        <table>
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
                        <td class="mono">{fmtTime(item.created)}</td>
                        <td>{item.event || '-'}</td>
                        <td class="mono">{item.method || '-'}</td>
                        <td class="path mono">{item.path || '-'}</td>
                        <td>
                            <span class={(item.status ?? 0) >= 500 ? 'status-err' : (item.status ?? 0) >= 400 ? 'status-warn' : 'status-ok'}>
                                {item.status || '-'}
                            </span>
                        </td>
                        <td class="mono">{item.duration_ms != null ? `${item.duration_ms}ms` : '-'}</td>
                        <td class="mono truncate" title={item.gateway_user_id || item.synthetic_user_id || ''}>{item.gateway_user_id || item.synthetic_user_id || '-'}</td>
                        <td class="mono">{item.remote_ip || '-'}</td>
                    </tr>
                {/each}
            </tbody>
        </table>
    </div>
</div>

<style>
    .empty { text-align: center; padding: 1.5rem; color: var(--text-secondary); }
    .path { max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
