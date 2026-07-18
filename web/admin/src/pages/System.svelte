<script lang="ts">
    import { onMount } from 'svelte';
    import { apiRequest, session, reauth } from '../lib/api';
    import type {
        ItemsResponse,
        Policy,
        PolicyBody,
        PolicyForm,
        SystemInfo,
        UpstreamBody,
        UpstreamDTO,
        UpstreamProbeResult,
        InstallDefaultsResponse,
    } from '../lib/types';

    let sysInfo = $state<SystemInfo | null>(null);
    let upstream = $state<UpstreamDTO | null>(null);
    let policies = $state<Policy[]>([]);

    let loading = $state(true);
    let error = $state<string | null>(null);

    let probeResult = $state<UpstreamProbeResult | null>(null);
    let probing = $state(false);
    let probeError = $state<string | null>(null);
    let showProbeModal = $state(false);

    let showPolicyModal = $state(false);
    let policyError = $state<string | null>(null);
    let policySaving = $state(false);
    
    // Using explicit object structure instead of Partial<PolicyForm> 
    // because Svelte 5 state needs to know all property keys upfront sometimes.
    let currentPolicy = $state<PolicyForm>({
        id: '',
        method: '',
        path: '',
        action: 'deny',
        reason: '',
        priority: 100,
        enabled: true
    });
    let isEditingPolicy = $state(false);

    let activeTab = $state<'runtime' | 'upstream' | 'policies'>('runtime');

    let probeForm = $state({
        emby_base_url: '',
        backend_username: '',
        backend_password: '',
        backend_user_agent: 'SenPlayer/6.1.3',
        backend_authorization_client: 'SenPlayer',
        backend_authorization_device: 'Mac',
        backend_authorization_version: '6.1.3',
        force: false,
    });

    let showReauthModal = $state(false);
    let reauthPassword = $state('');
    let reauthError = $state<string | null>(null);
    let reauthLoading = $state(false);
    let reauthTicket = $state<string | null>(null);
    let pendingAction: (() => Promise<void>) | null = null;

    async function loadData() {
        loading = true;
        try {
            const [si, up, pol] = await Promise.all([
                apiRequest<SystemInfo>('/system'),
                apiRequest<UpstreamDTO>('/upstream'),
                apiRequest<ItemsResponse<Policy>>('/path-policies'),
            ]);
            sysInfo = si;
            upstream = up;
            policies = pol.items || [];
            
            if (up) {
                probeForm.emby_base_url = up.base_url || '';
                probeForm.backend_username = up.backend_username || '';
                probeForm.backend_user_agent = up.backend_user_agent || 'SenPlayer/6.1.3';
                probeForm.backend_authorization_client = up.backend_authorization_client || 'SenPlayer';
                probeForm.backend_authorization_device = up.backend_authorization_device || 'Mac';
                probeForm.backend_authorization_version = up.backend_authorization_version || '6.1.3';
            }
            error = null;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    onMount(loadData);

    function fmtTime(v: string | undefined): string {
        if (!v) return '-';
        try { return new Date(v).toLocaleString(); } catch { return String(v); }
    }

    function yesNo(v: boolean | undefined): string {
        return v ? 'Yes' : 'No';
    }

    async function handleProbe(e: Event) {
        e.preventDefault();
        probeError = null;
        probeResult = null;
        probing = true;
        showProbeModal = true;
        try {
            const body: UpstreamBody = { ...probeForm, force: false };
            const res = await apiRequest<UpstreamProbeResult>('/upstream/probe', {
                method: 'POST',
                body: JSON.stringify(body),
            });
            probeResult = res;
        } catch (err) {
            probeError = err instanceof Error ? err.message : String(err);
        } finally {
            probing = false;
        }
    }

    function askForReauth(action: () => Promise<void>) {
        pendingAction = action;
        reauthError = null;
        reauthPassword = '';
        showReauthModal = true;
    }

    async function performApply() {
        try {
            if (!reauthTicket) throw new Error('Re-authentication required');
            const body: UpstreamBody = { ...probeForm, force: probeForm.force === true };
            await apiRequest('/upstream/reconfigure', {
                method: 'POST',
                headers: { 'X-Admin-Reauth': reauthTicket },
                body: JSON.stringify(body),
            });
            reauthTicket = null;
            showProbeModal = false;
            await loadData();
        } catch (err) {
            probeError = err instanceof Error ? err.message : String(err);
            throw err;
        }
    }

    function requestApply() {
        askForReauth(performApply);
    }

    function openNewPolicy() {
        currentPolicy = {
            id: '',
            method: '*',
            path: '',
            action: 'deny',
            reason: '',
            priority: 100,
            enabled: true,
        };
        isEditingPolicy = false;
        policyError = null;
        showPolicyModal = true;
    }

    function openEditPolicy(p: Policy) {
        currentPolicy = {
            id: p.ID || p.id || '',
            method: p.Method || p.method || '*',
            path: p.Path || p.path || '',
            action: p.Action || p.action || 'deny',
            reason: p.Reason || p.reason || '',
            priority: p.Priority ?? p.priority ?? 100,
            enabled: p.Enabled ?? p.enabled ?? true,
        };
        isEditingPolicy = true;
        policyError = null;
        showPolicyModal = true;
    }

    async function handleSavePolicy(e: Event) {
        e.preventDefault();
        policyError = null;
        policySaving = true;
        try {
            const body: PolicyBody = {
                method: currentPolicy.method,
                path: currentPolicy.path,
                action: currentPolicy.action,
                reason: currentPolicy.reason,
                priority: currentPolicy.priority,
                enabled: currentPolicy.enabled,
            };
            
            if (isEditingPolicy && currentPolicy.id) {
                await apiRequest(`/path-policies/${currentPolicy.id}`, {
                    method: 'PUT',
                    body: JSON.stringify(body),
                });
            } else {
                await apiRequest('/path-policies', {
                    method: 'POST',
                    body: JSON.stringify(body),
                });
            }
            showPolicyModal = false;
            await loadData();
        } catch (err) {
            policyError = err instanceof Error ? err.message : String(err);
        } finally {
            policySaving = false;
        }
    }

    async function handleDeletePolicy(id: string) {
        if (!confirm('Delete this policy?')) return;
        try {
            await apiRequest(`/path-policies/${id}`, { method: 'DELETE' });
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function handleInstallDefaults() {
        if (!confirm('Install default path policies? Existing default policies will be updated or preserved.')) return;
        try {
            const res = await apiRequest<InstallDefaultsResponse>('/path-policies/install-defaults', { method: 'POST' });
            alert(`Installed defaults. Created: ${res.created}, Preserved: ${res.preserved}`);
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function handleReauthSubmit(e: Event) {
        e.preventDefault();
        if (!pendingAction) {
            showReauthModal = false;
            return;
        }
        reauthError = null;
        reauthLoading = true;
        try {
            const identity = $session?.email || $session?.superuser_id;
            if (!identity) throw new Error('No active session identity found');
            
            reauthTicket = await reauth(identity, reauthPassword);
            showReauthModal = false;
            await pendingAction();
            pendingAction = null;
            reauthTicket = null;
        } catch (err) {
            reauthError = err instanceof Error ? err.message : String(err);
        } finally {
            reauthLoading = false;
        }
    }
</script>

<div class="page-header">
    <h1 class="page-title">System</h1>
</div>

<div class="page-body">
    {#if error}
        <div class="error-message">{error}</div>
    {/if}

    <div class="sub-nav">
        <div class="sub-nav-item {activeTab === 'runtime' ? 'active' : ''}" onclick={() => activeTab = 'runtime'} role="tab" tabindex="0" onkeydown={(e) => e.key === 'Enter' && (activeTab = 'runtime')}>Runtime Info</div>
        <div class="sub-nav-item {activeTab === 'upstream' ? 'active' : ''}" onclick={() => activeTab = 'upstream'} role="tab" tabindex="0" onkeydown={(e) => e.key === 'Enter' && (activeTab = 'upstream')}>Upstream</div>
        <div class="sub-nav-item {activeTab === 'policies' ? 'active' : ''}" onclick={() => activeTab = 'policies'} role="tab" tabindex="0" onkeydown={(e) => e.key === 'Enter' && (activeTab = 'policies')}>Path Policies</div>
    </div>

    {#if loading}
        <div class="text-secondary">Loading...</div>
    {:else}
        {#if activeTab === 'runtime'}
            <div class="panel">
                <div class="metric-label mb-4">System Information</div>
                <div class="data-grid">
                    <div class="metric-box">
                        <div class="metric-label">Version</div>
                        <div class="metric-value mono" style="font-size: 16px;">{sysInfo?.version || '-'}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Go Version</div>
                        <div class="metric-value mono" style="font-size: 16px;">{sysInfo?.go_version || '-'}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Boot ID</div>
                        <div class="metric-value mono truncate" style="font-size: 16px;">{sysInfo?.boot_id || '-'}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Started</div>
                        <div class="metric-value mono" style="font-size: 16px;">{fmtTime(sysInfo?.started_at)}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Uptime</div>
                        <div class="metric-value mono" style="font-size: 16px;">{sysInfo?.uptime_sec || 0}s</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Goroutines</div>
                        <div class="metric-value mono" style="font-size: 16px;">{sysInfo?.goroutines || 0}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Heap memory</div>
                        <div class="metric-value mono" style="font-size: 16px;">{((sysInfo?.heap_bytes || 0) / 1024 / 1024).toFixed(2)} MB</div>
                    </div>
                </div>
            </div>
        {/if}

        {#if activeTab === 'upstream'}
            <div class="panel">
                <div class="flex justify-between items-center mb-4">
                    <div class="metric-label" style="margin:0">Upstream Status</div>
                </div>
                <div class="data-grid mb-4">
                    <div class="metric-box">
                        <div class="metric-label">Configured</div>
                        <div class="metric-value {upstream?.configured ? 'status-ok' : 'status-warn'}">
                            {yesNo(upstream?.configured)}
                        </div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Endpoint Active</div>
                        <div class="metric-value {upstream?.endpoint_active ? 'status-ok' : 'status-warn'}">
                            {yesNo(upstream?.endpoint_active)}
                        </div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Password Set</div>
                        <div class="metric-value {upstream?.password_set ? 'status-ok' : 'status-err'}">
                            {yesNo(upstream?.password_set)}
                        </div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Token Set</div>
                        <div class="metric-value {upstream?.token_set ? 'status-ok' : 'status-err'}">
                            {yesNo(upstream?.token_set)}
                        </div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Version Checked</div>
                        <div class="metric-value mono" style="font-size: 14px;">{fmtTime(upstream?.version_checked_at)}</div>
                    </div>
                    <div class="metric-box">
                        <div class="metric-label">Last Login</div>
                        <div class="metric-value mono" style="font-size: 14px;">{fmtTime(upstream?.last_login_at)}</div>
                    </div>
                </div>

                {#if upstream?.last_login_error}
                    <div class="error-message">
                        Last login error: {upstream.last_login_error}
                    </div>
                {/if}
            </div>

            <div class="panel">
                <div class="metric-label mb-4">Upstream Configuration</div>
                <form onsubmit={handleProbe}>
                    <div class="form-grid">
                        <div style="grid-column: 1 / -1;">
                            <label class="text-sm text-secondary block mb-1" for="emby_base_url">Emby Base URL</label>
                            <input type="url" id="emby_base_url" bind:value={probeForm.emby_base_url} required placeholder="https://emby.example.com/emby" />
                        </div>
                        <div>
                            <label class="text-sm text-secondary block mb-1" for="backend_username">Backend Username</label>
                            <input type="text" id="backend_username" bind:value={probeForm.backend_username} required />
                        </div>
                        <div>
                            <label class="text-sm text-secondary block mb-1" for="backend_password">Backend Password</label>
                            <input type="password" id="backend_password" bind:value={probeForm.backend_password} placeholder={upstream?.password_set ? '(unchanged)' : ''} />
                        </div>
                        <div style="grid-column: 1 / -1;">
                            <label class="text-sm text-secondary block mb-1" for="backend_user_agent">User-Agent</label>
                            <input type="text" id="backend_user_agent" bind:value={probeForm.backend_user_agent} />
                        </div>
                        <div>
                            <label class="text-sm text-secondary block mb-1" for="backend_authorization_client">Auth Client</label>
                            <input type="text" id="backend_authorization_client" bind:value={probeForm.backend_authorization_client} />
                        </div>
                        <div>
                            <label class="text-sm text-secondary block mb-1" for="backend_authorization_device">Auth Device</label>
                            <input type="text" id="backend_authorization_device" bind:value={probeForm.backend_authorization_device} />
                        </div>
                        <div>
                            <label class="text-sm text-secondary block mb-1" for="backend_authorization_version">Auth Version</label>
                            <input type="text" id="backend_authorization_version" bind:value={probeForm.backend_authorization_version} />
                        </div>
                    </div>
                    <div class="mt-4 flex justify-end">
                        <button type="submit" disabled={probing}>{probing ? 'Probing...' : 'Probe & Setup'}</button>
                    </div>
                </form>
            </div>
        {/if}

        {#if activeTab === 'policies'}
            <div class="panel" style="padding: 0;">
                <div class="flex justify-between items-center" style="padding: 1rem 1.5rem; border-bottom: 1px solid var(--border-color);">
                    <div class="metric-label" style="margin:0">Path Policies</div>
                    <div class="flex gap-2">
                        <button type="button" class="secondary" onclick={handleInstallDefaults}>Install Defaults</button>
                        <button type="button" onclick={openNewPolicy}>Add Policy</button>
                    </div>
                </div>
                
                <div class="table-container" style="max-height: calc(100vh - 250px);">
                    <table style="min-width: 800px;">
                        <thead>
                            <tr>
                                <th style="width: 5%">Pri</th>
                                <th style="width: 10%">Method</th>
                                <th style="width: 35%">Path</th>
                                <th style="width: 10%">Action</th>
                                <th style="width: 20%">Reason</th>
                                <th style="width: 5%">Enabled</th>
                                <th style="width: 15%; text-align: right;">Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            {#if policies.length === 0}
                                <tr>
                                    <td colspan="7" class="text-secondary text-center" style="padding: 2rem;">No path policies configured.</td>
                                </tr>
                            {/if}
                            {#each policies as p}
                                <tr>
                                    <td>{p.Priority ?? p.priority}</td>
                                    <td><strong class="mono">{p.Method || p.method}</strong></td>
                                    <td><span class="mono">{p.Path || p.path}</span></td>
                                    <td>
                                        <span class={(p.Action || p.action) === 'allow' ? 'status-ok' : 'status-err'}>
                                            {(p.Action || p.action)?.toUpperCase()}
                                        </span>
                                    </td>
                                    <td class="text-secondary">{p.Reason || p.reason || '-'}</td>
                                    <td>{(p.Enabled ?? p.enabled) ? 'Yes' : 'No'}</td>
                                    <td>
                                        <div class="flex gap-2 justify-end">
                                            <button class="secondary text-xs" onclick={() => openEditPolicy(p)}>Edit</button>
                                            <button class="danger text-xs" onclick={() => handleDeletePolicy(p.ID || p.id || '')}>Del</button>
                                        </div>
                                    </td>
                                </tr>
                            {/each}
                        </tbody>
                    </table>
                </div>
            </div>
        {/if}
    {/if}
</div>

{#if showProbeModal}
    <div class="overlay" onclick={() => showProbeModal = false}>
        <div class="drawer" style="width: 400px;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">Upstream Probe Result</h3>
                <button class="icon" onclick={() => showProbeModal = false}>✕</button>
            </div>
            
            <div class="drawer-body">
                {#if probing}
                    <div class="text-secondary">Probing upstream server...</div>
                {:else if probeError}
                    <div class="error-message">{probeError}</div>
                {:else if probeResult}
                    <div class="mb-4">
                        <div class="metric-label mb-2">Probe Successful</div>
                        <div class="data-grid" style="grid-template-columns: 1fr;">
                            <div class="metric-box">
                                <div class="metric-label">Server Name</div>
                                <div class="metric-value" style="font-size: 16px;">{probeResult.server_name}</div>
                            </div>
                            <div class="metric-box">
                                <div class="metric-label">Server Version</div>
                                <div class="metric-value mono" style="font-size: 16px;">{probeResult.server_version}</div>
                            </div>
                            <div class="metric-box">
                                <div class="metric-label">Latency</div>
                                <div class="metric-value mono" style="font-size: 16px;">{probeResult.latency_ms} ms</div>
                            </div>
                        </div>
                    </div>
                    <div class="text-sm text-secondary">
                        <span class="status-warn font-bold">WARNING:</span> Applying this configuration will forcefully replace backend tokens and immediately disconnect all currently active gateway sessions.
                    </div>
                {/if}
            </div>

            <div class="drawer-footer">
                <button class="secondary" onclick={() => showProbeModal = false}>Cancel</button>
                {#if !probing && probeResult}
                    <button class="danger" onclick={requestApply}>Apply & Disconnect Sessions</button>
                {/if}
            </div>
        </div>
    </div>
{/if}

{#if showPolicyModal}
    <div class="overlay" onclick={() => showPolicyModal = false}>
        <div class="drawer" style="width: 450px;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">{isEditingPolicy ? 'Edit Policy' : 'Add Policy'}</h3>
                <button class="icon" onclick={() => showPolicyModal = false}>✕</button>
            </div>

            <div class="drawer-body">
                {#if policyError}
                    <div class="error-message">{policyError}</div>
                {/if}
                <form id="policy-form" onsubmit={handleSavePolicy}>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="p_method">Method</label>
                        <select id="p_method" bind:value={currentPolicy.method}>
                            <option value="*">* (Any)</option>
                            <option value="GET">GET</option>
                            <option value="POST">POST</option>
                            <option value="PUT">PUT</option>
                            <option value="DELETE">DELETE</option>
                        </select>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="p_path">Path (Regex)</label>
                        <input type="text" id="p_path" bind:value={currentPolicy.path} required placeholder="^/emby/users" class="mono" />
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="p_action">Action</label>
                        <select id="p_action" bind:value={currentPolicy.action}>
                            <option value="allow">Allow</option>
                            <option value="deny">Deny</option>
                        </select>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="p_reason">Reason</label>
                        <input type="text" id="p_reason" bind:value={currentPolicy.reason} />
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="p_priority">Priority (lower runs first)</label>
                        <input type="number" id="p_priority" bind:value={currentPolicy.priority} />
                    </div>
                    <div class="mb-4 flex items-center gap-2">
                        <input type="checkbox" id="p_enabled" bind:checked={currentPolicy.enabled} style="width:auto" />
                        <label class="text-sm text-secondary" for="p_enabled">Enabled</label>
                    </div>
                </form>
            </div>

            <div class="drawer-footer">
                <button class="secondary" onclick={() => showPolicyModal = false}>Cancel</button>
                <button type="submit" form="policy-form" disabled={policySaving}>{policySaving ? 'Saving...' : 'Save Policy'}</button>
            </div>
        </div>
    </div>
{/if}

{#if showReauthModal}
    <div class="overlay" style="z-index: 100;" onclick={() => showReauthModal = false}>
        <div class="drawer" style="width: 350px; justify-content: center; max-height: 350px; border-radius: 4px; margin: auto; height: auto;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">Confirm Change</h3>
                <button class="icon" onclick={() => showReauthModal = false}>✕</button>
            </div>
            
            <div class="drawer-body">
                <p class="text-sm text-secondary mb-4">
                    Re-enter your admin password to apply this change.
                    {#if $session?.email}
                        <br />Identity: <span class="mono">{$session.email}</span>
                    {/if}
                </p>
                {#if reauthError}
                    <div class="error-message">{reauthError}</div>
                {/if}
                <form id="reauth-form" onsubmit={handleReauthSubmit}>
                    <input type="password" placeholder="Admin Password" bind:value={reauthPassword} required autofocus />
                </form>
            </div>

            <div class="drawer-footer">
                <button class="secondary" onclick={() => showReauthModal = false}>Cancel</button>
                <button type="submit" form="reauth-form" disabled={reauthLoading} class="danger">
                    {reauthLoading ? 'Verifying...' : 'Confirm'}
                </button>
            </div>
        </div>
    </div>
{/if}

<style>
    .form-grid {
        display: grid;
        grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
        gap: 1rem;
    }
    .block { display: block; }
    .justify-end { justify-content: flex-end; }
    .text-xs { font-size: 11px; padding: 4px 8px; }
</style>