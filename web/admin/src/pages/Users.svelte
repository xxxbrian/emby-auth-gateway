<script lang="ts">
    import { onMount } from 'svelte';
    import { apiRequest } from '../lib/api';
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

    onMount(loadUsers);

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
                            <td><strong class="truncate" title={user.username}>{user.username}</strong></td>
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
