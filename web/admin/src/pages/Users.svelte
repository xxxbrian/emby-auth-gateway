<script lang="ts">
    import { onMount, onDestroy } from 'svelte';
    import { apiRequest } from '../lib/api';
    import MediaItemCell from '../lib/MediaItemCell.svelte';
    import type { UserMediaItem, UserMediaPage, UserMediaView } from '../lib/user-media-types';
    import type { CreateUserBody, ItemsResponse, PasswordBody, RevokeResponse, UserDTO } from '../lib/types';

    let users = $state<UserDTO[]>([]);
    let loading = $state(true);
    let error = $state<string | null>(null);

    let showCreate = $state(false);
    let newUsername = $state('');
    let newPassword = $state('');
    let newSyntheticId = $state('');
    let createError = $state<string | null>(null);
    let createLoading = $state(false);

    let resetUserId = $state<string | null>(null);
    let resetPasswordValue = $state('');

    let mediaUserId = $state<string | null>(null);
    let mediaView = $state<UserMediaView>('recent');
    let mediaRows = $state<UserMediaItem[]>([]);
    let mediaLoading = $state(false);
    let mediaError = $state<string | null>(null);
    let mediaCursor = $state('');
    let mediaHasMore = $state(false);
    let mediaAbort: AbortController | null = null;
    let mediaTrigger: HTMLButtonElement | null = null;
    let mediaUserName = $derived(users.find(user => user.id === mediaUserId)?.username || mediaUserId || '');

    function mediaURL() {
        const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
        q.delete('user_id'); q.delete('user_view');
        if (mediaUserId) { q.set('user_id', mediaUserId); q.set('user_view', mediaView); }
        window.history.replaceState(null, '', `#/users${q.size ? `?${q}` : ''}`);
    }
    async function loadUserMedia(append = false) {
        if (!mediaUserId) return;
        mediaAbort?.abort(); const ctrl = new AbortController(); mediaAbort = ctrl;
        const id = mediaUserId;
        const q = new URLSearchParams({ view: mediaView, limit: '50' });
        if (append && mediaCursor) q.set('cursor', mediaCursor);
        mediaLoading = true;
        try {
            const page = await apiRequest<UserMediaPage>(`/users/${encodeURIComponent(id)}/media?${q}`, { signal: ctrl.signal });
            if (ctrl.signal.aborted) return;
            mediaRows = append ? [...new Map([...mediaRows, ...(page.items || [])].map(row => [row.id, row])).values()] : page.items || [];
            mediaCursor = page.next_cursor || ''; mediaHasMore = !!page.has_more; mediaError = null;
        } catch (err) { if (!ctrl.signal.aborted) mediaError = err instanceof Error ? err.message : String(err); }
        finally { if (!ctrl.signal.aborted) mediaLoading = false; }
    }
    function selectMediaUser(id: string, trigger?: HTMLButtonElement) {
        mediaUserId = id; mediaView = 'recent'; mediaRows = []; mediaCursor = ''; mediaHasMore = false;
        mediaTrigger = trigger || null; mediaURL(); loadUserMedia();
    }
    function setMediaView(view: UserMediaView) {
        mediaView = view; mediaRows = []; mediaCursor = ''; mediaHasMore = false;
        mediaURL(); loadUserMedia();
    }
    function closeMedia() {
        mediaAbort?.abort(); mediaUserId = null; mediaRows = []; mediaLoading = false; mediaError = null;
        mediaURL(); mediaTrigger?.focus();
    }
    function fmtPosition(ticks: number): string {
        const seconds = Math.max(0, Math.floor(ticks / 10_000_000));
        const hours = Math.floor(seconds / 3600);
        return `${hours ? `${hours}:` : ''}${hours ? String(Math.floor(seconds % 3600 / 60)).padStart(2, '0') : Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
    }
    function mediaProgress(row: UserMediaItem) { return row.runtime_ticks > 0 ? Math.min(100, Math.max(0, row.position_ticks / row.runtime_ticks * 100)) : 0; }

    async function loadUsers() {
        try {
            const data = await apiRequest<ItemsResponse<UserDTO>>('/users');
            users = data.items || [];
            error = null;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    onMount(() => {
        loadUsers();
        const q = new URLSearchParams(window.location.hash.split('?')[1] || '');
        if (q.get('user_id')) {
            mediaUserId = q.get('user_id');
            const view = q.get('user_view');
            mediaView = view === 'resume' || view === 'favorites' ? view : 'recent';
            loadUserMedia();
        }
    });
    onDestroy(() => mediaAbort?.abort());

    async function handleCreate(e: Event) {
        e.preventDefault();
        createError = null;
        createLoading = true;
        try {
            const body: CreateUserBody = {
                username: newUsername,
                password: newPassword,
                synthetic_user_id: newSyntheticId,
            };
            await apiRequest<UserDTO>('/users', {
                method: 'POST',
                body: JSON.stringify(body),
            });
            showCreate = false;
            newUsername = '';
            newPassword = '';
            newSyntheticId = '';
            await loadUsers();
        } catch (err) {
            createError = err instanceof Error ? err.message : String(err);
        } finally {
            createLoading = false;
        }
    }

    async function toggleEnable(user: UserDTO) {
        if (!confirm(`${user.enabled ? 'Disable' : 'Enable'} user ${user.username}?`)) return;
        try {
            await apiRequest(`/users/${user.id}/${user.enabled ? 'disable' : 'enable'}`, { method: 'POST' });
            await loadUsers();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function handleResetPassword(e: Event) {
        e.preventDefault();
        if (!resetPasswordValue || !resetUserId) return;
        try {
            const body: PasswordBody = { password: resetPasswordValue };
            await apiRequest(`/users/${resetUserId}/password`, {
                method: 'POST',
                body: JSON.stringify(body),
            });
            resetUserId = null;
            resetPasswordValue = '';
            alert('Password reset.');
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function revokeSessions(id: string) {
        if (!confirm('Revoke all sessions for this user?')) return;
        try {
            const res = await apiRequest<RevokeResponse>(`/users/${id}/sessions/revoke-all`, { method: 'POST' });
            alert(`Revoked ${res.revoked || 0} sessions`);
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    function fmtTime(v: string | undefined): string {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }
</script>

<div class="page-header">
    <h1 class="page-title">Users</h1>
    <button onclick={() => showCreate = true}>Create User</button>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    {#if loading}
        <div class="text-secondary">Loading…</div>
    {:else if users.length === 0}
        <div class="text-secondary">No users found.</div>
    {:else}
        <div class="table-container panel users-table-wrap" style="padding: 0;">
            <table class="users-table">
                <thead>
                    <tr>
                        <th>Username</th>
                        <th>Status</th>
                        <th class="col-optional">ID</th>
                        <th class="col-optional">Synthetic ID</th>
                        <th class="col-optional">Created</th>
                        <th style="text-align: right;">Actions</th>
                    </tr>
                </thead>
                <tbody>
                    {#each users as user}
                        <tr>
                            <td><button type="button" class="user-media-link truncate" title={user.username} aria-label={`View media for ${user.username}`} onclick={(e) => selectMediaUser(user.id, e.currentTarget)}>{user.username}</button></td>
                            <td>
                                <span class={user.enabled ? 'status-ok' : 'status-err'}>
                                    {user.enabled ? 'Enabled' : 'Disabled'}
                                </span>
                            </td>
                            <td class="col-optional">
                                <div class="mono truncate" style="max-width: 120px;" title={user.id}>{user.id}</div>
                            </td>
                            <td class="col-optional">
                                <div class="mono text-secondary truncate" style="max-width: 160px;" title={user.synthetic_user_id || '-'}>{user.synthetic_user_id || '-'}</div>
                            </td>
                            <td class="col-optional">{fmtTime(user.created)}</td>
                            <td>
                                <div class="flex gap-2 justify-end user-actions">
                                    <button class="secondary text-xs" onclick={() => toggleEnable(user)}>
                                        {user.enabled ? 'Disable' : 'Enable'}
                                    </button>
                                    <button class="secondary text-xs" onclick={() => resetUserId = user.id}>Pwd</button>
                                    <button class="secondary text-xs" onclick={() => revokeSessions(user.id)}>Kick</button>
                                </div>
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    {/if}
    {#if mediaUserId}
        <section class="panel user-media-panel" aria-label={`Media for ${mediaUserName}`}>
            <div class="user-media-header"><div><h2>{mediaUserName} · Media</h2><p class="text-secondary text-sm">Playback progress and favorites saved for this Gateway user.</p></div><button type="button" class="secondary" onclick={closeMedia}>Close media</button></div>
            <div class="user-media-header">
                <div class="segmented-control" aria-label="User media view">
                    {#each [{ id: 'recent', label: 'Recently played' }, { id: 'resume', label: 'Continue watching' }, { id: 'favorites', label: 'Favorites' }] as tab}
                        <button type="button" class="tab {mediaView === tab.id ? 'active' : ''}" aria-pressed={mediaView === tab.id} onclick={() => setMediaView(tab.id as UserMediaView)}>{tab.label}</button>
                    {/each}
                </div>
                <button type="button" class="secondary" onclick={() => loadUserMedia()} disabled={mediaLoading}>Refresh media</button>
            </div>
            {#if mediaError}<div class="error-message" role="alert">{mediaError}</div>{/if}
            <div class="user-media-list">
                {#if mediaRows.length === 0}<p class="text-secondary">{mediaLoading ? 'Loading media…' : mediaView === 'favorites' ? 'No favorites saved for this user.' : mediaView === 'resume' ? 'Nothing to continue watching.' : 'No recently played media.'}</p>{/if}
                {#each mediaRows as row (row.id)}
                    <div class="user-media-row">
                        <div class="user-media-resource"><MediaItemCell itemId={row.item_id} fallbackName={row.item_name} sourceRef={row.source_ref} userId={mediaUserId} localState={{ position_ticks: row.position_ticks, runtime_ticks: row.runtime_ticks, played: row.played, is_favorite: row.is_favorite, last_played_at: row.last_played_at }} />{#if row.orphaned}<span class="text-xs status-warn">Resource no longer available</span>{:else if row.metadata_status === 'identity_mismatch'}<span class="text-xs status-warn">Resource no longer matches the saved playback</span>{/if}</div>
                        <div class="user-media-progress">
                            <div class="text-sm">{row.played ? 'Watched' : fmtPosition(row.position_ticks)}{#if row.runtime_ticks > 0 && !row.played}<span class="text-secondary"> / {fmtPosition(row.runtime_ticks)}</span>{/if}</div>
                            {#if row.runtime_ticks > 0}<progress max="100" value={row.played ? 100 : mediaProgress(row)} aria-label={`Playback progress for ${row.item_name || row.item_id}`}></progress>{:else if !row.played}<div class="text-secondary text-xs">Duration unavailable</div>{/if}
                        </div>
                        <div class="user-media-status">{#if row.is_favorite}<span class="status-ok text-sm">Favorite</span>{/if}<div class="text-secondary text-xs">{row.last_played_at ? fmtTime(row.last_played_at) : 'Not played yet'}</div></div>
                    </div>
                {/each}
            </div>
            <div class="user-media-header media-page-footer"><span class="text-secondary text-xs">{mediaRows.length} resources loaded</span>{#if mediaHasMore}<button type="button" class="secondary" onclick={() => loadUserMedia(true)} disabled={mediaLoading}>{mediaLoading ? 'Loading…' : 'Load more media'}</button>{/if}</div>
        </section>
    {/if}
</div>

{#if showCreate}
    <div class="overlay" onclick={() => showCreate = false}>
        <div class="drawer" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">Create User</h3>
                <button class="icon" onclick={() => showCreate = false}>✕</button>
            </div>
            <div class="drawer-body">
                {#if createError}
                    <div class="error-message">{createError}</div>
                {/if}
                <form id="create-user-form" onsubmit={handleCreate}>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-2" for="username">Username</label>
                        <input type="text" id="username" bind:value={newUsername} required />
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-2" for="password">Password</label>
                        <input type="password" id="password" bind:value={newPassword} required />
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-2" for="syn_id">Synthetic User ID</label>
                        <input type="text" id="syn_id" bind:value={newSyntheticId} required />
                    </div>
                </form>
            </div>
            <div class="drawer-footer">
                <button class="secondary" onclick={() => showCreate = false}>Cancel</button>
                <button type="submit" form="create-user-form" disabled={createLoading}>{createLoading ? 'Saving…' : 'Save User'}</button>
            </div>
        </div>
    </div>
{/if}

{#if resetUserId}
    <div class="overlay" onclick={() => resetUserId = null}>
        <div class="drawer" style="width: 350px;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">Reset Password</h3>
                <button class="icon" onclick={() => resetUserId = null}>✕</button>
            </div>
            <div class="drawer-body">
                <form id="reset-pwd-form" onsubmit={handleResetPassword}>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-2" for="new_pwd">New Password</label>
                        <input type="password" id="new_pwd" placeholder="New password" bind:value={resetPasswordValue} required />
                    </div>
                </form>
            </div>
            <div class="drawer-footer">
                <button class="secondary" onclick={() => resetUserId = null}>Cancel</button>
                <button type="submit" form="reset-pwd-form">Reset Password</button>
            </div>
        </div>
    </div>
{/if}

<style>
    .user-media-link { background:none; border:0; padding:0; color:var(--text-primary); text-align:left; font-weight:500; text-decoration:underline; text-underline-offset:3px; }
    .user-media-link:hover { background:none; color:var(--accent-hover); }
    .user-media-panel { margin-top:20px; }
    .user-media-header { display:flex; align-items:center; justify-content:space-between; flex-wrap:wrap; gap:12px; margin-bottom:16px; }
    .user-media-header h2 { margin:0; font-size:17px; font-weight:500; }
    .user-media-header p { margin:6px 0 0; }
    .user-media-row { display:grid; grid-template-columns:minmax(0,1.5fr) minmax(130px,.8fr) minmax(100px,.7fr); gap:20px; align-items:center; padding:14px 0; border-bottom:1px solid var(--border-color); }
    .user-media-resource { min-width:0; }
    .user-media-progress progress { display:block; width:100%; height:5px; margin-top:8px; border:0; border-radius:2px; background:var(--border-color); accent-color:var(--accent-hover); }
    .user-media-progress progress::-webkit-progress-bar { background:var(--border-color); border-radius:2px; }
    .user-media-progress progress::-webkit-progress-value { background:var(--accent-hover); border-radius:2px; }
    .media-page-footer { margin:16px 0 0; }
    .justify-end { justify-content: flex-end; }
    .block { display: block; }
    .users-table {
        width: 100%;
        min-width: 0;
    }
    .user-actions {
        flex-wrap: wrap;
    }
    .truncate {
        display: block;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 140px;
    }
    @media (max-width: 768px) {
        .user-media-row { grid-template-columns:minmax(0,1fr) 110px; gap:12px; }
        .user-media-status { grid-column:1/-1; display:flex; justify-content:space-between; flex-wrap:wrap; gap:8px; }
        .user-media-header .segmented-control { flex-wrap:wrap; }
        .users-table-wrap {
            overflow-x: hidden;
        }
        .users-table .col-optional {
            display: none;
        }
        .users-table th,
        .users-table td {
            padding: 8px 6px;
        }
        .user-actions {
            gap: 0.25rem;
        }
        .user-actions button {
            padding: 2px 8px;
            font-size: 11px;
        }
        .truncate {
            max-width: 110px;
        }
    }
</style>
