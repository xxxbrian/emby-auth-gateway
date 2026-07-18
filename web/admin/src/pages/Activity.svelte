<script>
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api.js';

    let activeTab = 'playbacks';
    let data = [];
    let error = null;
    let timer;

    const endpoints = {
        playbacks: '/activity/playbacks',
        transfers: '/activity/transfers',
        sessions: '/sessions'
    };

    async function loadData() {
        try {
            const res = await apiRequest(endpoints[activeTab]);
            data = res.items || [];
            error = null;
        } catch (err) {
            error = err.message;
        }
    }

    function switchTab(tab) {
        activeTab = tab;
        data = [];
        loadData();
    }

    function fmtTime(v) {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }

    onMount(() => {
        loadData();
        timer = setInterval(loadData, 3000);
    });

    onDestroy(() => clearInterval(timer));
</script>

<h1 class="page-title">Activity</h1>

<div class="tabs" role="tablist">
    <button type="button" class="tab {activeTab === 'playbacks' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'playbacks'} on:click={() => switchTab('playbacks')}>Playbacks</button>
    <button type="button" class="tab {activeTab === 'transfers' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'transfers'} on:click={() => switchTab('transfers')}>Transfers</button>
    <button type="button" class="tab {activeTab === 'sessions' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'sessions'} on:click={() => switchTab('sessions')}>Sessions</button>
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
                    <tr>
                        <td data-label="User">{item.username || item.user_id || '-'}</td>
                        <td data-label="Item">{item.item_name || item.item_id || '-'}</td>
                        <td data-label="Device">{item.device || '-'}</td>
                        <td data-label="Paused">{item.is_paused ? 'Yes' : 'No'}</td>
                        <td data-label="Started">{fmtTime(item.started_at)}</td>
                        <td data-label="Last Seen">{fmtTime(item.last_seen)}</td>
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
                    <tr>
                        <td data-label="User">{item.username || item.user_id || '-'}</td>
                        <td data-label="Item">{item.item_id || '-'}</td>
                        <td data-label="Mode">{item.media_mode || '-'}</td>
                        <td data-label="Bytes Out">{item.bytes_out ?? 0}</td>
                        <td data-label="Started">{fmtTime(item.started_at)}</td>
                        <td data-label="Last Seen">{fmtTime(item.last_seen)}</td>
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
                    <tr>
                        <td data-label="User">{item.gateway_username || item.gateway_user_id || '-'}</td>
                        <td data-label="Client">{item.client || '-'}</td>
                        <td data-label="Device">{item.device || '-'}</td>
                        <td data-label="IP">{item.remote_ip || '-'}</td>
                        <td data-label="Active">
                            <span class={item.active ? 'status-ok' : 'status-err'}>{item.active ? 'Active' : 'Inactive'}</span>
                        </td>
                        <td data-label="Expires">{fmtTime(item.expires_at)}</td>
                        <td data-label="Actions">
                            {#if item.active}
                                <button class="secondary text-xs" on:click={async () => {
                                    if (!confirm('Revoke this session?')) return;
                                    try {
                                        await apiRequest(`/sessions/${item.id}/revoke`, { method: 'POST' });
                                        await loadData();
                                    } catch (err) {
                                        alert(err.message);
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
