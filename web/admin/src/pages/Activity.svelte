<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api';
    import type { ItemsResponse, Playback, SessionDTO, TransferWithBuffer } from '../lib/types';

    type ActivityTab = 'playbacks' | 'transfers' | 'sessions';
    type ActivityItem = Playback | TransferWithBuffer | SessionDTO;

    let activeTab = $state<ActivityTab>('playbacks');
    let data = $state<ActivityItem[]>([]);
    let error = $state<string | null>(null);
    let timer: ReturnType<typeof setInterval> | undefined;
    let currentAbort: AbortController | null = null;
    let generation = $state(0);

    /** Buffer pair to highlight: "boot_id:stream_id" */
    let highlightBuffer = $state<string | null>(null);
    /** Shown when highlight pair not found in loaded data */
    let highlightMissing = $state(false);

    const endpoints: Record<ActivityTab, string> = {
        playbacks: '/activity/playbacks',
        transfers: '/activity/transfers',
        sessions: '/sessions',
    };

    async function loadData() {
        currentAbort?.abort();
        const ctrl = new AbortController();
        currentAbort = ctrl;
        const gen = ++generation;
        try {
            const res = await apiRequest<ItemsResponse<ActivityItem>>(endpoints[activeTab], { signal: ctrl.signal });
            if (ctrl.signal.aborted || gen !== generation) return;
            data = res.items || [];
            error = null;
            // After data loads, check highlight match
            if (highlightBuffer && activeTab === 'transfers') {
                const found = (data as TransferWithBuffer[]).some(t => bufferPairKey(t) === highlightBuffer);
                highlightMissing = !found;
                if (found) {
                    scrollToRow(highlightBuffer);
                }
            }
        } catch (err) {
            if ((err as Error).name === 'AbortError') return;
            if (gen !== generation) return;
            error = err instanceof Error ? err.message : String(err);
        }
    }

    function switchTab(tab: ActivityTab) {
        if (tab === activeTab) return;
        activeTab = tab;
        data = [];
        highlightBuffer = null;
        highlightMissing = false;
        loadData();
    }

    function parseHashQuery() {
        const hash = window.location.hash;
        const tabMatch = hash.match(/[?&]tab=([^&]+)/);
        const bufferMatch = hash.match(/[?&]buffer=([^&]+)/);

        if (tabMatch) {
            const tab = decodeURIComponent(tabMatch[1]) as ActivityTab;
            if (tab === 'playbacks' || tab === 'transfers' || tab === 'sessions') {
                activeTab = tab;
            }
        }
        if (bufferMatch) {
            highlightBuffer = decodeURIComponent(bufferMatch[1]);
            activeTab = 'transfers';
            highlightMissing = false;
        }

        // Strip query from hash to keep URL clean
        const cleaned = hash.replace(/\?[^]*$/, '');
        if (cleaned !== hash) {
            window.history.replaceState(null, '', cleaned || '#/activity');
        }
    }

    /** Stable pair key from a transfer's media_buffer link. */
    function bufferPairKey(t: TransferWithBuffer): string | null {
        if (t.media_buffer && t.media_buffer.boot_id && t.media_buffer.stream_id) {
            return `${t.media_buffer.boot_id}:${t.media_buffer.stream_id}`;
        }
        return null;
    }

    /** CSS-safe row ID from buffer pair. */
    function transferRowId(t: TransferWithBuffer): string | undefined {
        const key = bufferPairKey(t);
        if (!key) return undefined;
        // Use a simple hash to make it CSS-safe
        return `transfer-row-${key.replace(/[^a-zA-Z0-9_-]/g, '_')}`;
    }

    function isHighlighted(t: TransferWithBuffer): boolean {
        if (!highlightBuffer) return false;
        return bufferPairKey(t) === highlightBuffer;
    }

    function scrollToRow(pairKey: string) {
        setTimeout(() => {
            const safeId = `transfer-row-${pairKey.replace(/[^a-zA-Z0-9_-]/g, '_')}`;
            const row = document.getElementById(safeId);
            if (row) {
                row.scrollIntoView({ block: 'center', behavior: 'smooth' });
                row.focus({ preventScroll: true });
            }
        }, 50);
    }

    function fmtTime(v: string | undefined): string {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }

    function bufferLink(t: TransferWithBuffer): string | null {
        if (t.media_buffer && t.media_buffer.boot_id && t.media_buffer.stream_id) {
            return `#/buffer?stream=${t.media_buffer.boot_id}:${t.media_buffer.stream_id}`;
        }
        return null;
    }

    function asPlayback(item: ActivityItem): Playback {
        return item as Playback;
    }

    function asTransfer(item: ActivityItem): TransferWithBuffer {
        return item as TransferWithBuffer;
    }

    function asSession(item: ActivityItem): SessionDTO {
        return item as SessionDTO;
    }

    onMount(() => {
        parseHashQuery();
        loadData();
        timer = setInterval(loadData, 3000);
    });

    onDestroy(() => {
        currentAbort?.abort();
        if (timer) clearInterval(timer);
    });
</script>

<div class="page-header">
    <h1 class="page-title">Activity</h1>
    <div class="segmented-control" role="tablist" aria-label="Activity view">
        <button type="button" class="tab {activeTab === 'playbacks' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'playbacks'} onclick={() => switchTab('playbacks')}>Playbacks</button>
        <button type="button" class="tab {activeTab === 'transfers' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'transfers'} onclick={() => switchTab('transfers')}>Transfers</button>
        <button type="button" class="tab {activeTab === 'sessions' ? 'active' : ''}" role="tab" aria-selected={activeTab === 'sessions'} onclick={() => switchTab('sessions')}>Sessions</button>
    </div>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    {#if highlightMissing && activeTab === 'transfers'}
        <div class="info-notice" role="status">
            Requested buffer stream not found in active transfers. It may have completed or been released.
        </div>
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
                        <th style="width: 17%">User</th>
                        <th style="width: 22%">Item ID</th>
                        <th style="width: 10%">Mode</th>
                        <th style="width: 10%">Bytes Out</th>
                        <th style="width: 10%">Buffer</th>
                        <th style="width: 15%">Started</th>
                        <th style="width: 16%">Last Seen</th>
                    </tr>
                </thead>
                <tbody>
                    {#if data.length === 0}
                        <tr><td colspan="7" class="empty">No active transfers.</td></tr>
                    {/if}
                    {#each data as item}
                        {@const t = asTransfer(item)}
                        <tr id={transferRowId(t)} class={isHighlighted(t) ? 'row-highlighted' : ''} tabindex={isHighlighted(t) ? 0 : -1} aria-selected={isHighlighted(t)}>
                            <td><strong>{t.username || t.user_id || '-'}</strong></td>
                            <td class="mono truncate" style="max-width: 200px;" title={t.item_id}>{t.item_id || '-'}</td>
                            <td>{t.media_mode || '-'}</td>
                            <td class="mono">{t.bytes_out ?? 0}</td>
                            <td class="mono">
                                {#if bufferLink(t)}
                                    <a href={bufferLink(t)} title="View buffer stream" class="buffer-link">&rarr; stream</a>
                                {:else}
                                    &mdash;
                                {/if}
                            </td>
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
                                        <button type="button" class="secondary text-xs" onclick={async () => {
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
    .buffer-link { font-size: 11px; text-decoration: none; }
    .buffer-link:hover { text-decoration: underline; }
    .row-highlighted {
        background-color: rgba(37, 99, 235, 0.08);
        box-shadow: inset 3px 0 0 var(--accent);
    }
    .row-highlighted:focus {
        outline: 2px solid var(--accent);
        outline-offset: -2px;
    }
    .info-notice {
        border: 1px solid var(--border-color);
        background-color: var(--panel-bg);
        color: var(--text-secondary);
        padding: 8px 12px;
        border-radius: 2px;
        margin-bottom: 1rem;
        font-size: 12px;
    }
</style>
