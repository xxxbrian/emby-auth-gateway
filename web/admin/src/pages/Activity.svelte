<script>
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api.js';

    let activeTab = 'playbacks'; // playbacks, transfers, sessions
    let data = [];
    let error = null;
    let timer;

    async function loadData() {
        try {
            const res = await apiRequest(`/${activeTab}`);
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

    onMount(() => {
        loadData();
        timer = setInterval(loadData, 3000);
    });

    onDestroy(() => {
        clearInterval(timer);
    });
</script>

<h1 class="page-title">Activity</h1>

<div class="tabs">
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="tab {activeTab === 'playbacks' ? 'active' : ''}" on:click={() => switchTab('playbacks')}>Playbacks</div>
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="tab {activeTab === 'transfers' ? 'active' : ''}" on:click={() => switchTab('transfers')}>Transfers</div>
    <!-- svelte-ignore a11y-click-events-have-key-events -->
    <div class="tab {activeTab === 'sessions' ? 'active' : ''}" on:click={() => switchTab('sessions')}>Sessions</div>
</div>

{#if error}
    <div class="error-message">{error}</div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    <table class="mobile-cards">
        {#if activeTab === 'playbacks'}
            <thead>
                <tr>
                    <th>User ID</th>
                    <th>Item ID</th>
                    <th>Client</th>
                    <th>Started At</th>
                    <th>Last Active</th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="5" style="text-align: center; padding: 1.5rem">No active playbacks.</td></tr>
                {/if}
                {#each data as item}
                    <tr>
                        <td data-label="User ID">{item.user_id}</td>
                        <td data-label="Item ID">{item.item_id}</td>
                        <td data-label="Client">{item.client}</td>
                        <td data-label="Started At">{new Date(item.started_at).toLocaleString()}</td>
                        <td data-label="Last Active">{new Date(item.last_active).toLocaleString()}</td>
                    </tr>
                {/each}
            </tbody>
        {:else if activeTab === 'transfers'}
            <thead>
                <tr>
                    <th>ID</th>
                    <th>User ID</th>
                    <th>Path</th>
                    <th>Bytes</th>
                    <th>Started At</th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="5" style="text-align: center; padding: 1.5rem">No active transfers.</td></tr>
                {/if}
                {#each data as item}
                    <tr>
                        <td data-label="ID">{item.id}</td>
                        <td data-label="User ID">{item.user_id}</td>
                        <td data-label="Path" class="text-sm">{item.path}</td>
                        <td data-label="Bytes">{item.bytes_transferred}</td>
                        <td data-label="Started At">{new Date(item.started_at).toLocaleString()}</td>
                    </tr>
                {/each}
            </tbody>
        {:else}
            <thead>
                <tr>
                    <th>ID</th>
                    <th>User ID</th>
                    <th>Device</th>
                    <th>Client</th>
                    <th>IP</th>
                    <th>Created</th>
                </tr>
            </thead>
            <tbody>
                {#if data.length === 0}
                    <tr><td colspan="6" style="text-align: center; padding: 1.5rem">No sessions found.</td></tr>
                {/if}
                {#each data as item}
                    <tr>
                        <td data-label="ID">{item.id}</td>
                        <td data-label="User ID">{item.user_id}</td>
                        <td data-label="Device">{item.device_name || '-'}</td>
                        <td data-label="Client">{item.client_name || '-'} {item.client_version || ''}</td>
                        <td data-label="IP">{item.ip || '-'}</td>
                        <td data-label="Created">{new Date(item.created).toLocaleString()}</td>
                    </tr>
                {/each}
            </tbody>
        {/if}
    </table>
</div>
