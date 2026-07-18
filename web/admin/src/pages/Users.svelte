<script>
    import { onMount } from 'svelte';
    import { apiRequest } from '../lib/api.js';

    let users = [];
    let loading = true;
    let error = null;

    let showCreate = false;
    let newUsername = '';
    let newPassword = '';
    let newSyntheticId = '';
    let createError = null;
    let createLoading = false;

    let resetUserId = null;
    let resetPasswordValue = '';

    async function loadUsers() {
        try {
            const data = await apiRequest('/users');
            users = data.items || [];
            error = null;
        } catch (err) {
            error = err.message;
        } finally {
            loading = false;
        }
    }

    onMount(loadUsers);

    async function handleCreate(e) {
        e.preventDefault();
        createError = null;
        createLoading = true;
        try {
            await apiRequest('/users', {
                method: 'POST',
                body: JSON.stringify({
                    username: newUsername,
                    password: newPassword,
                    synthetic_user_id: newSyntheticId || undefined
                })
            });
            showCreate = false;
            newUsername = '';
            newPassword = '';
            newSyntheticId = '';
            await loadUsers();
        } catch (err) {
            createError = err.message;
        } finally {
            createLoading = false;
        }
    }

    async function toggleEnable(user) {
        if (!confirm(`Are you sure you want to ${user.enabled ? 'disable' : 'enable'} ${user.username}?`)) return;
        try {
            await apiRequest(`/users/${user.id}/${user.enabled ? 'disable' : 'enable'}`, { method: 'POST' });
            await loadUsers();
        } catch (err) {
            alert('Error: ' + err.message);
        }
    }

    async function handleResetPassword(e) {
        e.preventDefault();
        if (!resetPasswordValue) return;
        try {
            await apiRequest(`/users/${resetUserId}/password`, {
                method: 'POST',
                body: JSON.stringify({ password: resetPasswordValue })
            });
            resetUserId = null;
            resetPasswordValue = '';
            alert('Password reset successful');
        } catch (err) {
            alert('Error: ' + err.message);
        }
    }

    async function revokeSessions(id) {
        if (!confirm('Revoke all sessions for this user?')) return;
        try {
            const res = await apiRequest(`/users/${id}/sessions/revoke`, { method: 'POST' });
            alert(`Revoked ${res.revoked || 0} sessions`);
        } catch (err) {
            alert('Error: ' + err.message);
        }
    }
</script>

<div class="flex justify-between items-center mb-4">
    <h1 class="page-title" style="margin: 0">Users</h1>
    <button on:click={() => showCreate = !showCreate}>
        {showCreate ? 'Cancel' : 'Create User'}
    </button>
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
        <form on:submit={handleCreate} class="flex gap-4 items-center" style="flex-wrap: wrap">
            <div>
                <label class="text-sm text-secondary" for="username">Username</label>
                <input type="text" id="username" bind:value={newUsername} required class="mt-2" />
            </div>
            <div>
                <label class="text-sm text-secondary" for="password">Password</label>
                <input type="password" id="password" bind:value={newPassword} required class="mt-2" />
            </div>
            <div>
                <label class="text-sm text-secondary" for="syn_id">Synthetic User ID (optional)</label>
                <input type="text" id="syn_id" bind:value={newSyntheticId} class="mt-2" />
            </div>
            <div style="margin-top: 1.5rem;">
                <button type="submit" disabled={createLoading}>
                    {createLoading ? 'Saving...' : 'Save'}
                </button>
            </div>
        </form>
    </div>
{/if}

{#if resetUserId}
    <div class="panel">
        <h3 style="margin-top:0">Reset Password</h3>
        <form on:submit={handleResetPassword} class="flex gap-4 items-center">
            <div>
                <input type="password" placeholder="New Password" bind:value={resetPasswordValue} required />
            </div>
            <button type="submit">Reset</button>
            <button type="button" class="secondary" on:click={() => resetUserId = null}>Cancel</button>
        </form>
    </div>
{/if}

<div class="panel" style="padding: 0; overflow-x: auto;">
    {#if loading}
        <div style="padding: 1.5rem">Loading...</div>
    {:else if users.length === 0}
        <div style="padding: 1.5rem">No users found.</div>
    {:else}
        <table class="mobile-cards">
            <thead>
                <tr>
                    <th>ID</th>
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
                        <td data-label="ID">{user.id}</td>
                        <td data-label="Username"><strong>{user.username}</strong></td>
                        <td data-label="Status">
                            <span class={user.enabled ? 'status-ok' : 'status-err'}>
                                {user.enabled ? 'Enabled' : 'Disabled'}
                            </span>
                        </td>
                        <td data-label="Synthetic ID" class="text-secondary">{user.synthetic_user_id || '-'}</td>
                        <td data-label="Created">{new Date(user.created).toLocaleString()}</td>
                        <td data-label="Actions">
                            <div class="flex gap-2" style="justify-content: flex-end;">
                                <button class="secondary text-xs" on:click={() => toggleEnable(user)}>
                                    {user.enabled ? 'Disable' : 'Enable'}
                                </button>
                                <button class="secondary text-xs" on:click={() => resetUserId = user.id}>
                                    Password
                                </button>
                                <button class="secondary text-xs" on:click={() => revokeSessions(user.id)}>
                                    Revoke Sessions
                                </button>
                            </div>
                        </td>
                    </tr>
                {/each}
            </tbody>
        </table>
    {/if}
</div>
