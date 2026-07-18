<script>
    import { login } from '../lib/api.js';

    let identity = '';
    let password = '';
    let error = '';
    let loading = false;

    async function handleSubmit(e) {
        e.preventDefault();
        error = '';
        loading = true;
        try {
            await login(identity, password);
        } catch (err) {
            error = err.message;
        } finally {
            loading = false;
        }
    }
</script>

<div class="login-container">
    <form class="login-box panel" on:submit={handleSubmit}>
        <h2 class="mb-4" style="margin-top: 0;">Emby Auth Gateway</h2>
        
        {#if error}
            <div class="error-message">{error}</div>
        {/if}

        <div class="mb-4">
            <label class="text-sm text-secondary mb-2" style="display: block" for="identity">Admin Email</label>
            <input type="text" id="identity" bind:value={identity} required />
        </div>
        
        <div class="mb-4">
            <label class="text-sm text-secondary mb-2" style="display: block" for="password">Password</label>
            <input type="password" id="password" bind:value={password} required />
        </div>

        <button type="submit" style="width: 100%" disabled={loading}>
            {loading ? 'Authenticating...' : 'Sign In'}
        </button>
    </form>
</div>

<style>
    .login-container {
        display: flex;
        justify-content: center;
        align-items: center;
        min-height: 100vh;
        background-color: var(--bg-color);
    }
    .login-box {
        width: 100%;
        max-width: 400px;
    }
</style>
