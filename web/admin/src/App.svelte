<script lang="ts">
    import { onMount } from 'svelte';
    import Router from 'svelte-spa-router';
    import { session, initialized, checkSession, logout } from './lib/api';
    import Login from './pages/Login.svelte';
    import Overview from './pages/Overview.svelte';
    import Users from './pages/Users.svelte';
    import Activity from './pages/Activity.svelte';
    import Traffic from './pages/Traffic.svelte';
    import System from './pages/System.svelte';

    import './app.css';

    const routes: Record<string, typeof Overview> = {
        '/': Overview,
        '/users': Users,
        '/activity': Activity,
        '/traffic': Traffic,
        '/system': System,
        '*': Overview
    };

    let currentPath = $state('/');

    function updatePath() {
        let hash = window.location.hash;
        if (hash.startsWith('#')) hash = hash.substring(1);
        if (hash.includes('?')) hash = hash.split('?')[0];
        currentPath = hash || '/';
    }

    onMount(() => {
        checkSession();
        updatePath();
    });
</script>

<svelte:window onhashchange={updatePath} />

{#if !$initialized}
    <div style="display: flex; justify-content: center; align-items: center; height: 100vh;">
        Loading...
    </div>
{:else if !$session}
    <Login />
{:else}
    <div class="app-container">
        <aside class="sidebar">
            <div class="sidebar-title">Admin</div>
            <nav class="sidebar-nav">
                <a href="#/" class="nav-link {currentPath === '/' ? 'active' : ''}">Overview</a>
                <a href="#/users" class="nav-link {currentPath === '/users' ? 'active' : ''}">Users</a>
                <a href="#/activity" class="nav-link {currentPath === '/activity' ? 'active' : ''}">Activity</a>
                <a href="#/traffic" class="nav-link {currentPath === '/traffic' ? 'active' : ''}">Traffic</a>
                <a href="#/system" class="nav-link {currentPath === '/system' ? 'active' : ''}">System</a>
            </nav>
            <div class="sidebar-footer">
                <div class="text-sm text-secondary" style="word-break: break-all;">{$session.email || $session.superuser_id || 'superuser'}</div>
                <button class="secondary sidebar-logout" onclick={logout}>Logout</button>
            </div>
        </aside>

        <main class="main-content">
            <Router {routes} />
        </main>
    </div>
{/if}
