<script>
    import { onMount } from 'svelte';
    import { apiRequest, session, reauth } from '../lib/api.js';

    let sysInfo = null;
    let upstream = null;
    let policies = [];
    
    let loading = true;
    let error = null;

    let showPolicyForm = false;
    let policyForm = { id: '', method: '', path: '', action: 'allow', enabled: true };
    
    let upstreamForm = {
        emby_base_url: '',
        backend_username: '',
        backend_password: '',
        backend_user_agent: 'SenPlayer/6.1.3',
        backend_authorization_client: 'SenPlayer',
        backend_authorization_device: 'Mac',
        backend_authorization_version: '6.1.3',
        force: false
    };

    let showProbeModal = false;
    let probeResult = null;
    let probeError = null;
    let probing = false;

    // reauth modal state
    let showReauthModal = false;
    let reauthPassword = '';
    let reauthError = '';
    let reauthLoading = false;

    async function loadData() {
        try {
            sysInfo = await apiRequest('/system');
            upstream = await apiRequest('/upstream');
            const polRes = await apiRequest('/policies');
            policies = polRes.items || [];
            
            if (upstream) {
                upstreamForm.emby_base_url = upstream.emby_base_url || '';
                upstreamForm.backend_username = upstream.backend_username || '';
            }
        } catch (err) {
            error = err.message;
        } finally {
            loading = false;
        }
    }

    onMount(loadData);

    async function handleInstallDefaults() {
        if (!confirm('Install default policies? This will not override existing custom policies.')) return;
        try {
            const res = await apiRequest('/policies/defaults', { method: 'POST' });
            alert(`Installed ${res.created} defaults, preserved ${res.preserved}`);
            await loadData();
        } catch (err) {
            alert('Error: ' + err.message);
        }
    }

    async function savePolicy(e) {
        e.preventDefault();
        try {
            const payload = { ...policyForm };
            if (policyForm.id) {
                await apiRequest(`/policies/${policyForm.id}`, { method: 'PUT', body: JSON.stringify(payload) });
            } else {
                await apiRequest('/policies', { method: 'POST', body: JSON.stringify(payload) });
            }
            showPolicyForm = false;
            await loadData();
        } catch(err) {
            alert('Error: ' + err.message);
        }
    }

    async function deletePolicy(id) {
        if (!confirm('Delete this policy?')) return;
        try {
            await apiRequest(`/policies/${id}`, { method: 'DELETE' });
            await loadData();
        } catch(err) {
            alert('Error: ' + err.message);
        }
    }

    function editPolicy(p) {
        policyForm = { ...p };
        showPolicyForm = true;
    }

    async function handleProbe(e) {
        e.preventDefault();
        probing = true;
        probeError = null;
        probeResult = null;
        showProbeModal = true;
        try {
            probeResult = await apiRequest('/upstream/probe', {
                method: 'POST',
                body: JSON.stringify(upstreamForm)
            });
        } catch (err) {
            probeError = err.message;
        } finally {
            probing = false;
        }
    }

    function requestReconfigure() {
        showProbeModal = false;
        showReauthModal = true;
    }

    async function handleReauthAndReconfigure(e) {
        e.preventDefault();
        reauthError = '';
        reauthLoading = true;
        try {
            const ticket = await reauth($session.username || $session.id, reauthPassword);
            
            await apiRequest('/upstream', {
                method: 'POST',
                headers: {
                    'X-Admin-Reauth': ticket
                },
                body: JSON.stringify(upstreamForm)
            });
            
            showReauthModal = false;
            reauthPassword = '';
            alert('Upstream reconfigured successfully');
            await loadData();
        } catch (err) {
            reauthError = err.message;
            if (err.message.includes('media load')) {
                reauthError = 'Active media load detected. Check "Force" to proceed anyway.';
            }
        } finally {
            reauthLoading = false;
        }
    }
</script>

<h1 class="page-title">System & Upstream</h1>

{#if error}
    <div class="error-message">{error}</div>
{/if}

{#if loading}
    <div>Loading...</div>
{:else}
    <div class="panel">
        <h3 style="margin-top:0">System Info</h3>
        <div class="flex gap-4" style="flex-wrap: wrap">
            <div class="text-sm">
                <div class="text-secondary">Version</div>
                <div>{sysInfo?.version || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Boot ID</div>
                <div>{sysInfo?.boot_id || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Uptime</div>
                <div>{sysInfo?.uptime_seconds}s</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Memory</div>
                <div>{((sysInfo?.mem_alloc_bytes || 0) / 1024 / 1024).toFixed(1)} MB</div>
            </div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Upstream Configuration</h3>
        <form on:submit={handleProbe}>
            <div class="flex gap-4 mb-4" style="flex-wrap: wrap">
                <div style="flex: 1; min-width: 250px;">
                    <label class="text-sm text-secondary" for="base_url">Emby Base URL</label>
                    <input type="url" id="base_url" bind:value={upstreamForm.emby_base_url} required class="mt-2" />
                </div>
                <div style="flex: 1; min-width: 200px;">
                    <label class="text-sm text-secondary" for="b_user">Backend Username</label>
                    <input type="text" id="b_user" bind:value={upstreamForm.backend_username} required class="mt-2" />
                </div>
                <div style="flex: 1; min-width: 200px;">
                    <label class="text-sm text-secondary" for="b_pass">Backend Password (if changing)</label>
                    <input type="password" id="b_pass" bind:value={upstreamForm.backend_password} class="mt-2" />
                </div>
            </div>
            
            <details class="mb-4">
                <summary class="text-sm text-secondary cursor-pointer" style="cursor: pointer;">Advanced Device Info</summary>
                <div class="flex gap-4 mt-2" style="flex-wrap: wrap; padding: 1rem; background: rgba(255,255,255,0.02); border-radius: 4px;">
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary">Client</label>
                        <input type="text" bind:value={upstreamForm.backend_authorization_client} class="mt-2 text-sm" />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary">Device</label>
                        <input type="text" bind:value={upstreamForm.backend_authorization_device} class="mt-2 text-sm" />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary">Version</label>
                        <input type="text" bind:value={upstreamForm.backend_authorization_version} class="mt-2 text-sm" />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary">User Agent</label>
                        <input type="text" bind:value={upstreamForm.backend_user_agent} class="mt-2 text-sm" />
                    </div>
                </div>
            </details>
            
            <div class="flex items-center gap-4">
                <label class="flex items-center gap-2 text-sm">
                    <input type="checkbox" bind:checked={upstreamForm.force} style="width: auto;" />
                    Force reconfigure (ignore active playbacks)
                </label>
            </div>
            
            <div class="mt-4">
                <button type="submit">Probe & Configure</button>
            </div>
        </form>
    </div>

    <div class="panel" style="padding: 0;">
        <div class="flex justify-between items-center" style="padding: 1.5rem">
            <h3 style="margin: 0">Path Policies</h3>
            <div class="flex gap-2">
                <button class="secondary" on:click={handleInstallDefaults}>Install Defaults</button>
                <button on:click={() => { policyForm = { id: '', method: '', path: '', action: 'allow', enabled: true }; showPolicyForm = true; }}>Add Policy</button>
            </div>
        </div>
        
        {#if showPolicyForm}
            <div style="padding: 0 1.5rem 1.5rem; border-bottom: 1px solid var(--border-color);">
                <form on:submit={savePolicy} class="flex gap-4 items-center" style="flex-wrap: wrap">
                    <div>
                        <label class="text-xs text-secondary">Method (* or GET/POST)</label>
                        <input type="text" bind:value={policyForm.method} required class="mt-2 text-sm" />
                    </div>
                    <div>
                        <label class="text-xs text-secondary">Path (Regex)</label>
                        <input type="text" bind:value={policyForm.path} required class="mt-2 text-sm" />
                    </div>
                    <div>
                        <label class="text-xs text-secondary">Action</label>
                        <select bind:value={policyForm.action} required class="mt-2 text-sm">
                            <option value="allow">Allow</option>
                            <option value="deny">Deny</option>
                            <option value="rewrite">Rewrite</option>
                        </select>
                    </div>
                    <label class="flex items-center gap-2 mt-4 text-sm" style="align-self: flex-end; margin-bottom: 0.5rem;">
                        <input type="checkbox" bind:checked={policyForm.enabled} style="width: auto;" /> Enabled
                    </label>
                    <div style="align-self: flex-end; margin-bottom: 0.2rem;">
                        <button type="submit">Save</button>
                        <button type="button" class="secondary" on:click={() => showPolicyForm = false}>Cancel</button>
                    </div>
                </form>
            </div>
        {/if}

        <div style="overflow-x: auto;">
            <table class="mobile-cards">
                <thead>
                    <tr>
                        <th>Method</th>
                        <th>Path (Regex)</th>
                        <th>Action</th>
                        <th>Status</th>
                        <th>Actions</th>
                    </tr>
                </thead>
                <tbody>
                    {#each policies as p}
                        <tr>
                            <td data-label="Method"><strong>{p.method}</strong></td>
                            <td data-label="Path" class="text-sm"><code>{p.path}</code></td>
                            <td data-label="Action">
                                <span class={p.action === 'allow' ? 'status-ok' : p.action === 'deny' ? 'status-err' : 'status-warn'}>
                                    {p.action}
                                </span>
                            </td>
                            <td data-label="Status">{p.enabled ? 'Enabled' : 'Disabled'}</td>
                            <td data-label="Actions">
                                <div class="flex gap-2" style="justify-content: flex-end;">
                                    <button class="secondary text-xs" on:click={() => editPolicy(p)}>Edit</button>
                                    <button class="secondary text-xs" on:click={() => deletePolicy(p.id)}>Delete</button>
                                </div>
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    </div>
{/if}

<!-- Probe Modal -->
{#if showProbeModal}
    <div class="modal-backdrop">
        <div class="panel modal-content" style="max-width: 500px; width: 100%;">
            <h3 style="margin-top:0">Upstream Probe Result</h3>
            
            {#if probing}
                <div>Probing upstream server...</div>
            {:else if probeError}
                <div class="error-message">{probeError}</div>
                <div class="mt-4 flex gap-2">
                    <button class="secondary" on:click={() => showProbeModal = false}>Close</button>
                </div>
            {:else if probeResult}
                <div class="flex flex-col gap-2 mt-4 text-sm">
                    <div class="flex justify-between">
                        <span class="text-secondary">Server Name:</span>
                        <span>{probeResult.server_name || '-'}</span>
                    </div>
                    <div class="flex justify-between">
                        <span class="text-secondary">Server Version:</span>
                        <span>{probeResult.server_version || '-'}</span>
                    </div>
                    <div class="flex justify-between">
                        <span class="text-secondary">Server ID:</span>
                        <span>{probeResult.server_id || '-'}</span>
                    </div>
                    <div class="flex justify-between">
                        <span class="text-secondary">Latency:</span>
                        <span>{probeResult.latency_ms || 0} ms</span>
                    </div>
                </div>
                
                <div class="mt-4 pt-4 flex gap-2 justify-end" style="border-top: 1px solid var(--border-color)">
                    <button class="secondary" on:click={() => showProbeModal = false}>Cancel</button>
                    <button class="danger" on:click={requestReconfigure}>Apply Configuration</button>
                </div>
            {/if}
        </div>
    </div>
{/if}

<!-- Reauth Modal -->
{#if showReauthModal}
    <div class="modal-backdrop">
        <div class="panel modal-content" style="max-width: 400px; width: 100%;">
            <h3 style="margin-top:0">Confirm Configuration Change</h3>
            <p class="text-sm text-secondary mb-4">Re-enter your admin password to apply this dangerous change.</p>
            
            {#if reauthError}
                <div class="error-message">{reauthError}</div>
            {/if}
            
            <form on:submit={handleReauthAndReconfigure}>
                <input type="password" bind:value={reauthPassword} required placeholder="Admin Password" class="mb-4" />
                <div class="flex gap-2 justify-end">
                    <button type="button" class="secondary" on:click={() => showReauthModal = false} disabled={reauthLoading}>Cancel</button>
                    <button type="submit" class="danger" disabled={reauthLoading}>{reauthLoading ? 'Applying...' : 'Confirm & Apply'}</button>
                </div>
            </form>
        </div>
    </div>
{/if}

<style>
    .modal-backdrop {
        position: fixed;
        top: 0; left: 0; right: 0; bottom: 0;
        background: rgba(0,0,0,0.7);
        display: flex;
        justify-content: center;
        align-items: center;
        z-index: 100;
        padding: 1rem;
    }
    .modal-content {
        margin-bottom: 0;
        box-shadow: 0 10px 25px rgba(0,0,0,0.5);
    }
    .flex-col { flex-direction: column; }
    .justify-end { justify-content: flex-end; }
</style>
