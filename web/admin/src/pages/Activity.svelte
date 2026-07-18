<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api';
    import type { ItemsResponse, Playback, SessionDTO, Transfer } from '../lib/types';

    type ActivityTab = 'playbacks' | 'transfers' | 'sessions';
    type ActivityItem = Playback | Transfer | SessionDTO;

    let activeTab = $state<ActivityTab>('playbacks');
    let data = $state<ActivityItem[]>([]);
    let error = $state<string | null>(null);
    let timer: ReturnType<typeof setInterval> | undefined;

    const endpoints: Record<ActivityTab, string> = {
        playbacks: '/activity/playbacks',
        transfers: '/activity/transfers',
        sessions: '/sessions',
    };

    async function loadData() {
        try {
            const res = await apiRequest<ItemsResponse<ActivityItem>>(endpoints[activeTab]);
            data = res.items || [];
            error = null;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        }
    }

    function switchTab(tab: ActivityTab) {
        activeTab = tab;
        data = [];
        loadData();
    }

    function fmtTime(v: string | undefined): string {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }

    function asPlayback(item: ActivityItem): Playback {
        return item as Playback;
    }

    function asTransfer(item: ActivityItem): Transfer {
        return item as Transfer;
    }

    function asSession(item: ActivityItem): SessionDTO {
        return item as SessionDTO;
    }

    onMount(() => {
        loadData();
        timer = setInterval(loadData, 3000);
    });

    onDestroy(() => {
        if (timer) clearInterval(timer);
    });
</script>

<h1 class="page-title">Activity</h1>

<div class="tabs" role="tablist">
    <button type="button" class="tab {activeTab === 'playbacks' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'playbacks'} onclick={() => switchTab('playbacks')}>Playbacks</button>
    <button type="button" class="tab {activeTab === 'transfers' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'transfers'} onclick={() => switchTab('transfers')}>Transfers</button>
    <button type="button" class="tab {activeTab === 'sessions' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'sessions'} onclick={() => switchTab('sessions')}>Sessions</button>
</div>

{#if error}
    <div class="error-message">{error}</div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    <table class="mobile-cards">
        {#if activeTab === 'playbacks'}
            <thead>
                <tr>
                    <th>User</th>
                    <th>Item</th>
                    <th>Device</th>
                    <th>Paused</th>
                    <th>Started</th>
                    <th>Last Seen</th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="6" class="empty">No active playbacks.</td></tr>
                {/if}
                {#each data as item}
                    {@const p = asPlayback(item)}
                    <tr>
                        <td data-label="User">{p.username || p.user_id || '-'}</td>
                        <td data-label="Item">{p.item_name || p.item_id || '-'}</td>
                        <td data-label="Device">{p.device || '-'}</td>
                        <td data-label="Paused">{p.is_paused ? 'Yes' : 'No'}</td>
                        <td data-label="Started">{fmtTime(p.started_at)}</td>
                        <td data-label="Last Seen">{fmtTime(p.last_seen)}</td>
                    </tr>
                {/each}
            </tbody>
        {:else if activeTab === 'transfers'}
            <thead>
                <tr>
                    <th>User</th>
                    <th>Item</th>
                    <th>Mode</th>
                    <th>Bytes Out</th>
                    <th>Started</th>
                    <th>Last Seen</th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="6" class="empty">No active transfers.</td></tr>
                {/if}
                {#each data as item}
                    {@const t = asTransfer(item)}
                    <tr>
                        <td data-label="User">{t.username || t.user_id || '-'}</td>
                        <td data-label="Item">{t.item_id || '-'}</td>
                        <td data-label="Mode">{t.media_mode || '-'}</td>
                        <td data-label="Bytes Out">{t.bytes_out ?? 0}</td>
                        <td data-label="Started">{fmtTime(t.started_at)}</td>
                        <td data-label="Last Seen">{fmtTime(t.last_seen)}</td>
                    </tr>
                {/each}
            </tbody>
        {:else}
            <thead>
                <tr>
                    <th>User</th>
                    <th>Client</th>
                    <th>Device</th>
                    <th>IP</th>
                    <th>Active</th>
                    <th>Expires</th>
                    <th></th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="7" class="empty">No sessions.</td></tr>
                {/if}
                {#each data as item}
                    {@const s = asSession(item)}
                    <tr>
                        <td data-label="User">{s.gateway_username || s.gateway_user_id || '-'}</td>
                        <td data-label="Client">{s.client || '-'}</td>
                        <td data-label="Device">{s.device || '-'}</td>
                        <td data-label="IP">{s.remote_ip || '-'}</td>
                        <td data-label="Active">
                            <span class={s.active ? 'status-ok' : 'status-err'}>{s.active ? 'Active' : 'Inactive'}</span>
                        </td>
                        <td data-label="Expires">{fmtTime(s.expires_at)}</td>
                        <td data-label="Actions">
                            {#if s.active}
                                <button class="secondary text-xs" onclick={async () => {
                                    if (!confirm('Revoke this session?')) return;
                                    try {
                                        await apiRequest(`/sessions/${s.id}/revoke`, { method: 'POST' });
                                        await loadData();
                                    } catch (err) {
                                        alert(err instanceof Error ? err.message : String(err));
                                    }
                                }}>Revoke</button>
                            {/if}
                        </td>
                    </tr>
                {/each}
            </tbody>
        {/if}
    </table>
</div>

<style>
    .empty { text-align: center; padding: 1.5rem; color: var(--text-secondary); }
    .tabs { display: flex; gap: 0.5rem; margin-bottom: 1rem; }
    .tab {
        background: transparent;
        border: 1px solid var(--border-color);
        color: var(--text-secondary);
        padding: 0.4rem 0.9rem;
        border-radius: 999px;
    }
    .tab.active {
        background: var(--primary);
        border-color: var(--primary);
        color: #fff;
    }
    .text-xs { font-size: 0.75rem; padding: 0.25rem 0.5rem; }
</style>
