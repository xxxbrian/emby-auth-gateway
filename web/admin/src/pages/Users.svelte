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

<div class="flex justify-between items-center mb-4">
    <h1 class="page-title" style="margin: 0">Users</h1>
    <button onclick={() => showCreate = !showCreate}>{showCreate ? 'Cancel' : 'Create User'}</button>
</div>

{#if error}
    <div class="error-message">{error}</div>
{/if}

{#if showCreate}
    <div class="panel">
        <h3 style="margin-top:0">Create User</h3>
        {#if createError}
            <div class="error-message">{createError}</div>
        {/if}
        <form onsubmit={handleCreate} class="form-grid">
            <div>
                <label class="text-sm text-secondary" for="username">Username</label>
                <input type="text" id="username" bind:value={newUsername} required class="mt-2" />
            </div>
            <div>
                <label class="text-sm text-secondary" for="password">Password</label>
                <input type="password" id="password" bind:value={newPassword} required class="mt-2" />
            </div>
            <div>
                <label class="text-sm text-secondary" for="syn_id">Synthetic User ID</label>
                <input type="text" id="syn_id" bind:value={newSyntheticId} required class="mt-2" />
            </div>
            <div class="actions">
                <button type="submit" disabled={createLoading}>{createLoading ? 'Saving…' : 'Save'}</button>
            </div>
        </form>
    </div>
{/if}

{#if resetUserId}
    <div class="panel">
        <h3 style="margin-top:0">Reset Password</h3>
        <form onsubmit={handleResetPassword} class="flex gap-4 items-center">
            <input type="password" placeholder="New password" bind:value={resetPasswordValue} required />
            <button type="submit">Reset</button>
            <button type="button" class="secondary" onclick={() => resetUserId = null}>Cancel</button>
        </form>
    </div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    {#if loading}
        <div style="padding: 1.5rem" class="text-secondary">Loading…</div>
    {:else if users.length === 0}
        <div style="padding: 1.5rem" class="text-secondary">No users found.</div>
    {:else}
        <table class="mobile-cards">
            <thead>
                <tr>
                    <th>Username</th>
                    <th>Status</th>
                    <th>Synthetic ID</th>
                    <th>Created</th>
                    <th>Actions</th>
                </tr>
            </thead>
            <tbody>
                {#each users as user}
                    <tr>
                        <td data-label="Username"><strong>{user.username}</strong></td>
                        <td data-label="Status">
                            <span class={user.enabled ? 'status-ok' : 'status-err'}>
                                {user.enabled ? 'Enabled' : 'Disabled'}
                            </span>
                        </td>
                        <td data-label="Synthetic ID" class="text-secondary mono">{user.synthetic_user_id || '-'}</td>
                        <td data-label="Created">{fmtTime(user.created)}</td>
                        <td data-label="Actions">
                            <div class="flex gap-2 action-row">
                                <button class="secondary text-xs" onclick={() => toggleEnable(user)}>
                                    {user.enabled ? 'Disable' : 'Enable'}
                                </button>
                                <button class="secondary text-xs" onclick={() => resetUserId = user.id}>Password</button>
                                <button class="secondary text-xs" onclick={() => revokeSessions(user.id)}>Kick</button>
                            </div>
                        </td>
                    </tr>
                {/each}
            </tbody>
        </table>
    {/if}
</div>

<style>
    .form-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
        gap: 1rem;
        align-items: end;
    }
    .actions { padding-bottom: 0.1rem; }
    .text-xs { font-size: 0.75rem; padding: 0.35rem 0.65rem; }
    .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.85rem; }
    .action-row { justify-content: flex-end; flex-wrap: wrap; gap: 0.5rem; }
    @media (max-width: 768px) {
        .action-row {
            justify-content: stretch;
            width: 100%;
        }
        .action-row button {
            flex: 1 1 calc(50% - 0.25rem);
            min-width: 0;
        }
    }
</style>
