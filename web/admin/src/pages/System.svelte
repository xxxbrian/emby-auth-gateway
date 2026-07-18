<script lang="ts">
    import { onMount } from 'svelte';
    import { apiRequest, session, reauth } from '../lib/api';
    import type {
        InstallDefaultsResponse,
        ItemsResponse,
        Policy,
        PolicyBody,
        PolicyForm,
        SystemInfo,
        UpstreamBody,
        UpstreamDTO,
        UpstreamProbeResult,
    } from '../lib/types';

    let sysInfo = $state<SystemInfo | null>(null);
    let upstream = $state<UpstreamDTO | null>(null);
    let policies = $state<PolicyForm[]>([]);

    let loading = $state(true);
    let error = $state<string | null>(null);

    let showPolicyForm = $state(false);
    let policyForm = $state<PolicyForm>(emptyPolicyForm());

    let upstreamForm = $state<UpstreamBody>({
        emby_base_url: '',
        backend_username: '',
        backend_password: '',
        backend_user_agent: 'SenPlayer/6.1.3',
        backend_authorization_client: 'SenPlayer',
        backend_authorization_device: 'Mac',
        backend_authorization_version: '6.1.3',
        force: false,
    });

    let showProbeModal = $state(false);
    let probeResult = $state<UpstreamProbeResult | null>(null);
    let probeError = $state<string | null>(null);
    let probing = $state(false);

    let showReauthModal = $state(false);
    let reauthPassword = $state('');
    let reauthError = $state('');
    let reauthLoading = $state(false);

    function emptyPolicyForm(): PolicyForm {
        return {
            id: '',
            method: '*',
            path: '',
            action: 'allow',
            reason: '',
            priority: 0,
            enabled: true,
        };
    }

    function normalizePolicy(p: Policy | null | undefined): PolicyForm {
        if (!p) return emptyPolicyForm();
        return {
            id: p.id || p.ID || '',
            method: p.method || p.Method || '*',
            path: p.path || p.Path || '',
            action: (p.action || p.Action || 'allow').toLowerCase(),
            reason: p.reason || p.Reason || '',
            priority: p.priority ?? p.Priority ?? 0,
            enabled: p.enabled ?? p.Enabled ?? true,
        };
    }

    function applyUpstreamToForm(up: UpstreamDTO | null) {
        if (!up) return;
        upstreamForm.emby_base_url = up.base_url || '';
        upstreamForm.backend_username = up.backend_username || '';
        if (up.backend_user_agent) {
            upstreamForm.backend_user_agent = up.backend_user_agent;
        }
        if (up.backend_authorization_client) {
            upstreamForm.backend_authorization_client = up.backend_authorization_client;
        }
        if (up.backend_authorization_device) {
            upstreamForm.backend_authorization_device = up.backend_authorization_device;
        }
        if (up.backend_authorization_version) {
            upstreamForm.backend_authorization_version = up.backend_authorization_version;
        }
    }

    function yesNo(v: boolean | undefined): string {
        return v ? 'Yes' : 'No';
    }

    function formatBytes(n: number | undefined): string {
        const bytes = Number(n) || 0;
        if (bytes < 1024) return `${bytes} B`;
        if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
        return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
    }

    function formatUptime(sec: number | undefined): string {
        const s = Math.max(0, Number(sec) || 0);
        if (s < 60) return `${s}s`;
        const m = Math.floor(s / 60);
        const rs = s % 60;
        if (m < 60) return `${m}m ${rs}s`;
        const h = Math.floor(m / 60);
        const rm = m % 60;
        if (h < 48) return `${h}h ${rm}m`;
        const d = Math.floor(h / 24);
        const rh = h % 24;
        return `${d}d ${rh}h`;
    }

    async function loadData() {
        try {
            error = null;
            sysInfo = await apiRequest<SystemInfo>('/system');
            upstream = await apiRequest<UpstreamDTO>('/upstream');
            const polRes = await apiRequest<ItemsResponse<Policy>>('/path-policies');
            policies = (polRes.items || []).map(normalizePolicy);
            applyUpstreamToForm(upstream);
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    onMount(loadData);

    async function handleInstallDefaults() {
        if (!confirm('Install default policies? Existing matching policies are preserved.')) return;
        try {
            const res = await apiRequest<InstallDefaultsResponse>('/path-policies/install-defaults', { method: 'POST' });
            alert(`Installed ${res.created ?? 0} defaults, preserved ${res.preserved ?? 0}`);
            await loadData();
        } catch (err) {
            alert('Error: ' + (err instanceof Error ? err.message : String(err)));
        }
    }

    async function savePolicy(e: Event) {
        e.preventDefault();
        try {
            const payload: PolicyBody = {
                method: policyForm.method,
                path: policyForm.path,
                action: policyForm.action,
                reason: policyForm.reason,
                priority: Number(policyForm.priority) || 0,
                enabled: !!policyForm.enabled,
            };
            if (policyForm.id) {
                await apiRequest(`/path-policies/${policyForm.id}`, {
                    method: 'PUT',
                    body: JSON.stringify(payload),
                });
            } else {
                await apiRequest('/path-policies', {
                    method: 'POST',
                    body: JSON.stringify(payload),
                });
            }
            showPolicyForm = false;
            policyForm = emptyPolicyForm();
            await loadData();
        } catch (err) {
            alert('Error: ' + (err instanceof Error ? err.message : String(err)));
        }
    }

    async function deletePolicy(id: string) {
        if (!id) return;
        if (!confirm('Delete this policy?')) return;
        try {
            await apiRequest(`/path-policies/${id}`, { method: 'DELETE' });
            await loadData();
        } catch (err) {
            alert('Error: ' + (err instanceof Error ? err.message : String(err)));
        }
    }

    function editPolicy(p: PolicyForm) {
        policyForm = { ...p };
        showPolicyForm = true;
    }

    function openNewPolicy() {
        policyForm = emptyPolicyForm();
        showPolicyForm = true;
    }

    async function handleProbe(e: Event) {
        e.preventDefault();
        probing = true;
        probeError = null;
        probeResult = null;
        showProbeModal = true;
        try {
            const body: Omit<UpstreamBody, 'force'> = {
                emby_base_url: upstreamForm.emby_base_url,
                backend_username: upstreamForm.backend_username,
                backend_password: upstreamForm.backend_password,
                backend_user_agent: upstreamForm.backend_user_agent,
                backend_authorization_client: upstreamForm.backend_authorization_client,
                backend_authorization_device: upstreamForm.backend_authorization_device,
                backend_authorization_version: upstreamForm.backend_authorization_version,
            };
            probeResult = await apiRequest<UpstreamProbeResult>('/upstream/probe', {
                method: 'POST',
                body: JSON.stringify(body),
            });
        } catch (err) {
            probeError = err instanceof Error ? err.message : String(err);
        } finally {
            probing = false;
        }
    }

    function requestReconfigure() {
        showProbeModal = false;
        reauthError = '';
        reauthPassword = '';
        showReauthModal = true;
    }

    async function handleReauthAndReconfigure(e: Event) {
        e.preventDefault();
        reauthError = '';
        reauthLoading = true;
        try {
            const identity = $session?.email;
            if (!identity) {
                throw new Error('Admin email not available in session');
            }
            const ticket = await reauth(identity, reauthPassword);

            await apiRequest('/upstream/reconfigure', {
                method: 'POST',
                headers: {
                    'X-Admin-Reauth': ticket,
                },
                body: JSON.stringify(upstreamForm),
            });

            showReauthModal = false;
            reauthPassword = '';
            alert('Upstream reconfigured successfully');
            await loadData();
        } catch (err) {
            const msg = err instanceof Error ? err.message : String(err);
            reauthError = msg;
            if (/active.?media|force/i.test(msg || '')) {
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
                <div class="mono">{sysInfo?.boot_id || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Started</div>
                <div>{sysInfo?.started_at ? new Date(sysInfo.started_at).toLocaleString() : '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Uptime</div>
                <div title="{sysInfo?.uptime_sec ?? 0}s">{formatUptime(sysInfo?.uptime_sec)}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Goroutines</div>
                <div>{sysInfo?.goroutines ?? '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Heap</div>
                <div>{formatBytes(sysInfo?.heap_bytes)}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Go</div>
                <div class="mono">{sysInfo?.go_version || '-'}</div>
            </div>
        </div>
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Upstream Status</h3>
        <div class="flex gap-4 mb-4" style="flex-wrap: wrap">
            <div class="text-sm">
                <div class="text-secondary">Configured</div>
                <div class={upstream?.configured ? 'status-ok' : 'status-warn'}>
                    {yesNo(upstream?.configured)}
                </div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Base URL</div>
                <div class="mono text-sm">{upstream?.base_url || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Server</div>
                <div>
                    {upstream?.server_name || '-'}
                    {#if upstream?.server_version}
                        <span class="text-secondary">· {upstream.server_version}</span>
                    {/if}
                </div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Server ID</div>
                <div class="mono text-sm">{upstream?.server_id || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Backend User</div>
                <div>{upstream?.backend_username || '-'}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Password Set</div>
                <div>{yesNo(upstream?.password_set)}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Token Set</div>
                <div>{yesNo(upstream?.token_set)}</div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Endpoint Active</div>
                <div class={upstream?.endpoint_active ? 'status-ok' : 'status-warn'}>
                    {yesNo(upstream?.endpoint_active)}
                </div>
            </div>
            <div class="text-sm">
                <div class="text-secondary">Last Login</div>
                <div>
                    {upstream?.last_login_at ? new Date(upstream.last_login_at).toLocaleString() : '-'}
                </div>
            </div>
        </div>
        {#if upstream?.last_login_error}
            <div class="error-message" style="margin-bottom: 0">
                Last login error: {upstream.last_login_error}
            </div>
        {/if}
    </div>

    <div class="panel">
        <h3 style="margin-top:0">Upstream Configuration</h3>
        <form on:submit={handleProbe}>
            <div class="flex gap-4 mb-4" style="flex-wrap: wrap">
                <div style="flex: 1; min-width: 250px;">
                    <label class="text-sm text-secondary" for="emby_base_url">Emby Base URL</label>
                    <input
                        type="url"
                        id="emby_base_url"
                        bind:value={upstreamForm.emby_base_url}
                        required
                        class="mt-2"
                        autocomplete="off"
                    />
                </div>
                <div style="flex: 1; min-width: 200px;">
                    <label class="text-sm text-secondary" for="backend_username">Backend Username</label>
                    <input
                        type="text"
                        id="backend_username"
                        bind:value={upstreamForm.backend_username}
                        required
                        class="mt-2"
                        autocomplete="username"
                    />
                </div>
                <div style="flex: 1; min-width: 200px;">
                    <label class="text-sm text-secondary" for="backend_password">Backend Password (if changing)</label>
                    <input
                        type="password"
                        id="backend_password"
                        bind:value={upstreamForm.backend_password}
                        class="mt-2"
                        autocomplete="new-password"
                    />
                </div>
            </div>

            <details class="mb-4">
                <summary class="text-sm text-secondary" style="cursor: pointer;">Advanced Device Info</summary>
                <div class="flex gap-4 mt-2" style="flex-wrap: wrap; padding: 1rem; background: rgba(255,255,255,0.02); border-radius: 4px;">
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary" for="backend_authorization_client">Client</label>
                        <input
                            type="text"
                            id="backend_authorization_client"
                            bind:value={upstreamForm.backend_authorization_client}
                            class="mt-2 text-sm"
                        />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary" for="backend_authorization_device">Device</label>
                        <input
                            type="text"
                            id="backend_authorization_device"
                            bind:value={upstreamForm.backend_authorization_device}
                            class="mt-2 text-sm"
                        />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary" for="backend_authorization_version">Version</label>
                        <input
                            type="text"
                            id="backend_authorization_version"
                            bind:value={upstreamForm.backend_authorization_version}
                            class="mt-2 text-sm"
                        />
                    </div>
                    <div style="flex: 1; min-width: 150px;">
                        <label class="text-xs text-secondary" for="backend_user_agent">User Agent</label>
                        <input
                            type="text"
                            id="backend_user_agent"
                            bind:value={upstreamForm.backend_user_agent}
                            class="mt-2 text-sm"
                        />
                    </div>
                </div>
            </details>

            <div class="flex items-center gap-4">
                <label class="flex items-center gap-2 text-sm" for="upstream_force">
                    <input type="checkbox" id="upstream_force" bind:checked={upstreamForm.force} style="width: auto;" />
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
                <button type="button" class="secondary" on:click={handleInstallDefaults}>Install Defaults</button>
                <button type="button" on:click={openNewPolicy}>Add Policy</button>
            </div>
        </div>

        {#if showPolicyForm}
            <div style="padding: 0 1.5rem 1.5rem; border-bottom: 1px solid var(--border-color);">
                <form on:submit={savePolicy} class="flex gap-4 items-end" style="flex-wrap: wrap">
                    <div>
                        <label class="text-xs text-secondary" for="policy_method">Method (* or GET/POST)</label>
                        <input
                            type="text"
                            id="policy_method"
                            bind:value={policyForm.method}
                            required
                            class="mt-2 text-sm"
                        />
                    </div>
                    <div style="flex: 1; min-width: 180px;">
                        <label class="text-xs text-secondary" for="policy_path">Path pattern</label>
                        <input
                            type="text"
                            id="policy_path"
                            bind:value={policyForm.path}
                            required
                            class="mt-2 text-sm"
                            placeholder="/Items/*"
                        />
                    </div>
                    <div>
                        <label class="text-xs text-secondary" for="policy_action">Action</label>
                        <select id="policy_action" bind:value={policyForm.action} required class="mt-2 text-sm">
                            <option value="allow">Allow</option>
                            <option value="deny">Deny</option>
                        </select>
                    </div>
                    <div>
                        <label class="text-xs text-secondary" for="policy_priority">Priority</label>
                        <input
                            type="number"
                            id="policy_priority"
                            bind:value={policyForm.priority}
                            class="mt-2 text-sm"
                            style="width: 6rem;"
                        />
                    </div>
                    <div style="flex: 1; min-width: 140px;">
                        <label class="text-xs text-secondary" for="policy_reason">Reason</label>
                        <input
                            type="text"
                            id="policy_reason"
                            bind:value={policyForm.reason}
                            class="mt-2 text-sm"
                        />
                    </div>
                    <label class="flex items-center gap-2 text-sm" for="policy_enabled" style="margin-bottom: 0.35rem;">
                        <input type="checkbox" id="policy_enabled" bind:checked={policyForm.enabled} style="width: auto;" />
                        Enabled
                    </label>
                    <div class="flex gap-2" style="margin-bottom: 0.15rem;">
                        <button type="submit">Save</button>
                        <button type="button" class="secondary" on:click={() => (showPolicyForm = false)}>Cancel</button>
                    </div>
                </form>
            </div>
        {/if}

        <div style="overflow-x: auto;">
            <table class="mobile-cards">
                <thead>
                    <tr>
                        <th>Method</th>
                        <th>Path pattern</th>
                        <th>Action</th>
                        <th>Priority</th>
                        <th>Reason</th>
                        <th>Status</th>
                        <th>Actions</th>
                    </tr>
                </thead>
                <tbody>
                    {#each policies as p (p.id)}
                        <tr>
                            <td data-label="Method"><strong>{p.method}</strong></td>
                            <td data-label="Path pattern" class="text-sm"><code>{p.path}</code></td>
                            <td data-label="Action">
                                <span class={p.action === 'allow' ? 'status-ok' : 'status-err'}>
                                    {p.action}
                                </span>
                            </td>
                            <td data-label="Priority">{p.priority}</td>
                            <td data-label="Reason" class="text-sm text-secondary">{p.reason || '-'}</td>
                            <td data-label="Status">{p.enabled ? 'Enabled' : 'Disabled'}</td>
                            <td data-label="Actions">
                                <div class="flex gap-2" style="justify-content: flex-end;">
                                    <button type="button" class="secondary text-xs" on:click={() => editPolicy(p)}>Edit</button>
                                    <button type="button" class="secondary text-xs" on:click={() => deletePolicy(p.id)}>Delete</button>
                                </div>
                            </td>
                        </tr>
                    {:else}
                        <tr>
                            <td colspan="7" class="text-sm text-secondary" style="text-align: center; padding: 1.5rem;">
                                No path policies configured.
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    </div>
{/if}

{#if showProbeModal}
    <div class="modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="probe-modal-title">
        <div class="panel modal-content" style="max-width: 500px; width: 100%;">
            <h3 id="probe-modal-title" style="margin-top:0">Upstream Probe Result</h3>

            {#if probing}
                <div>Probing upstream server...</div>
            {:else if probeError}
                <div class="error-message">{probeError}</div>
                <div class="mt-4 flex gap-2">
                    <button type="button" class="secondary" on:click={() => (showProbeModal = false)}>Close</button>
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
                        <span class="mono">{probeResult.server_id || '-'}</span>
                    </div>
                    <div class="flex justify-between">
                        <span class="text-secondary">Latency:</span>
                        <span>{probeResult.latency_ms ?? 0} ms</span>
                    </div>
                </div>

                <div class="mt-4 pt-4 flex gap-2 justify-end" style="border-top: 1px solid var(--border-color)">
                    <button type="button" class="secondary" on:click={() => (showProbeModal = false)}>Cancel</button>
                    <button type="button" class="danger" on:click={requestReconfigure}>Apply Configuration</button>
                </div>
            {/if}
        </div>
    </div>
{/if}

{#if showReauthModal}
    <div class="modal-backdrop" role="dialog" aria-modal="true" aria-labelledby="reauth-modal-title">
        <div class="panel modal-content" style="max-width: 400px; width: 100%;">
            <h3 id="reauth-modal-title" style="margin-top:0">Confirm Configuration Change</h3>
            <p class="text-sm text-secondary mb-4">
                Re-enter your admin password to apply this dangerous change.
                {#if $session?.email}
                    <br />Identity: <span class="mono">{$session.email}</span>
                {/if}
            </p>

            {#if reauthError}
                <div class="error-message">{reauthError}</div>
            {/if}

            <form on:submit={handleReauthAndReconfigure}>
                <label class="text-sm text-secondary" for="reauth_password">Admin Password</label>
                <input
                    type="password"
                    id="reauth_password"
                    bind:value={reauthPassword}
                    required
                    class="mt-2 mb-4"
                    autocomplete="current-password"
                />
                <div class="flex gap-2 justify-end">
                    <button
                        type="button"
                        class="secondary"
                        on:click={() => (showReauthModal = false)}
                        disabled={reauthLoading}
                    >
                        Cancel
                    </button>
                    <button type="submit" class="danger" disabled={reauthLoading}>
                        {reauthLoading ? 'Applying...' : 'Confirm & Apply'}
                    </button>
                </div>
            </form>
        </div>
    </div>
{/if}

<style>
    .modal-backdrop {
        position: fixed;
        top: 0;
        left: 0;
        right: 0;
        bottom: 0;
        background: rgba(0, 0, 0, 0.7);
        display: flex;
        justify-content: center;
        align-items: center;
        z-index: 100;
        padding: 1rem;
    }
    .modal-content {
        margin-bottom: 0;
        box-shadow: 0 10px 25px rgba(0, 0, 0, 0.5);
    }
    .flex-col {
        flex-direction: column;
    }
    .justify-end {
        justify-content: flex-end;
    }
    .mono {
        font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
        word-break: break-all;
    }
</style>
