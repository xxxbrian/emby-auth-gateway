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

<div class="page-header">
    <h1 class="page-title">Activity</h1>
    <div class="segmented-control">
        <div class="tab {activeTab === 'playbacks' ? 'active' : ''}" onclick={() => switchTab('playbacks')} role="tab" aria-selected={activeTab === 'playbacks'} tabindex="0" onkeydown={(e) => e.key === 'Enter' && switchTab('playbacks')}>Playbacks</div>
        <div class="tab {activeTab === 'transfers' ? 'active' : ''}" onclick={() => switchTab('transfers')} role="tab" aria-selected={activeTab === 'transfers'} tabindex="0" onkeydown={(e) => e.key === 'Enter' && switchTab('transfers')}>Transfers</div>
        <div class="tab {activeTab === 'sessions' ? 'active' : ''}" onclick={() => switchTab('sessions')} role="tab" aria-selected={activeTab === 'sessions'} tabindex="0" onkeydown={(e) => e.key === 'Enter' && switchTab('sessions')}>Sessions</div>
    </div>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    <div class="table-container panel" style="padding: 0;">
        <table style="min-width: 800px;">
            {#if activeTab === 'playbacks'}
                <thead>
                    <tr>
                        <th style="width: 20%">User</th>
                        <th style="width: 25%">Item</th>
                        <th style="width: 15%">Device</th>
                        <th style="width: 10%">Status</th>
                        <th style="width: 15%">Started</th>
                        <th style="width: 15%">Last Seen</th>
                    </tr>
                </thead>
                <tbody>
                    {#if data.length === 0}
                        <tr><td colspan="6" class="empty">No active playbacks.</td></tr>
                    {/if}
                    {#each data as item}
                        {@const p = asPlayback(item)}
                        <tr>
                            <td><strong>{p.username || p.user_id || '-'}</strong></td>
                            <td class="truncate" style="max-width: 200px;" title={p.item_name || p.item_id}>{p.item_name || p.item_id || '-'}</td>
                            <td>{p.device || '-'}</td>
                            <td><span class={p.is_paused ? 'status-warn' : 'status-ok'}>{p.is_paused ? 'Paused' : 'Playing'}</span></td>
                            <td>{fmtTime(p.started_at)}</td>
                            <td>{fmtTime(p.last_seen)}</td>
                        </tr>
                    {/each}
                </tbody>
            {:else if activeTab === 'transfers'}
                <thead>
                    <tr>
                        <th style="width: 20%">User</th>
                        <th style="width: 25%">Item ID</th>
                        <th style="width: 15%">Mode</th>
                        <th style="width: 10%">Bytes Out</th>
                        <th style="width: 15%">Started</th>
                        <th style="width: 15%">Last Seen</th>
                    </tr>
                </thead>
                <tbody>
                    {#if data.length === 0}
                        <tr><td colspan="6" class="empty">No active transfers.</td></tr>
                    {/if}
                    {#each data as item}
                        {@const t = asTransfer(item)}
                        <tr>
                            <td><strong>{t.username || t.user_id || '-'}</strong></td>
                            <td class="mono truncate" style="max-width: 200px;" title={t.item_id}>{t.item_id || '-'}</td>
                            <td>{t.media_mode || '-'}</td>
                            <td class="mono">{t.bytes_out ?? 0}</td>
                            <td>{fmtTime(t.started_at)}</td>
                            <td>{fmtTime(t.last_seen)}</td>
                        </tr>
                    {/each}
                </tbody>
            {:else}
                <thead>
                    <tr>
                        <th style="width: 15%">User</th>
                        <th style="width: 15%">Client</th>
                        <th style="width: 15%">Device</th>
                        <th style="width: 15%">IP</th>
                        <th style="width: 10%">Active</th>
                        <th style="width: 15%">Expires</th>
                        <th style="width: 10%; text-align: right;">Actions</th>
                    </tr>
                </thead>
                <tbody>
                    {#if data.length === 0}
                        <tr><td colspan="7" class="empty">No sessions.</td></tr>
                    {/if}
                    {#each data as item}
                        {@const s = asSession(item)}
                        <tr>
                            <td><strong>{s.gateway_username || s.gateway_user_id || '-'}</strong></td>
                            <td>{s.client || '-'}</td>
                            <td>{s.device || '-'}</td>
                            <td class="mono">{s.remote_ip || '-'}</td>
                            <td>
                                <span class={s.active ? 'status-ok' : 'status-err'}>{s.active ? 'Active' : 'Inactive'}</span>
                            </td>
                            <td>{fmtTime(s.expires_at)}</td>
                            <td>
                                <div class="flex justify-end">
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
                                </div>
                            </td>
                        </tr>
                    {/each}
                </tbody>
            {/if}
        </table>
    </div>
</div>

<style>
    .empty { text-align: center; padding: 2rem !important; color: var(--text-secondary); }
    .justify-end { justify-content: flex-end; }
</style>
