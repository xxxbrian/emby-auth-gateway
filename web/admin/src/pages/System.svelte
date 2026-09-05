<script lang="ts">
    import { onMount } from 'svelte';
    import { apiRequest, session, reauth, completeReauthMfa, requestMfaOtp } from '../lib/api';
    import type {
        ItemsResponse,
        Policy,
        PolicyBody,
        PolicyForm,
        PolicyPreviewResult,
        SystemInfo,
        UpstreamBody,
        UpstreamDTO,
        UpstreamEndpointDTO,
        UpstreamEndpointBody,
        RouteRuleDTO,
        RouteRuleBody,
        WebSocketProbeResult,
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

    let previewMethod = $state('GET');
    let previewPath = $state('');
    let previewResult = $state<PolicyPreviewResult | null>(null);
    let previewError = $state<string | null>(null);
    let previewing = $state(false);
    
    // Using explicit object structure instead of Partial<PolicyForm> 
    // because Svelte 5 state needs to know all property keys upfront sometimes.
    let currentPolicy = $state<PolicyForm>({
        id: '',
        method: '',
        path: '',
        action: 'deny',
        reason: '',
        priority: 100,
        enabled: true,
        updated: '',
    });
    let isEditingPolicy = $state(false);

    let activeTab = $state<'runtime' | 'upstream' | 'policies'>('runtime');

    // --- upstream endpoints ---
    let endpoints = $state<UpstreamEndpointDTO[]>([]);
    let showEndpointModal = $state(false);
    let endpointError = $state<string | null>(null);
    let endpointSaving = $state(false);
    let probingEndpointID = $state<string | null>(null);
    let endpointForm = $state<UpstreamEndpointBody & { id: string }>({
        id: '', key: '', base_url: '', enabled: true, is_default: false,
    });

    // --- route rules ---
    let routeRules = $state<RouteRuleDTO[]>([]);
    let showRouteRuleModal = $state(false);
    let routeRuleError = $state<string | null>(null);
    let routeRuleSaving = $state(false);
    let isEditingRouteRule = $state(false);
    let routeRuleForm = $state<RouteRuleBody & { id: string }>({
        id: '', method: '', path: '', transport: '', target: '', priority: 100, enabled: true, reason: '', updated: '',
    });
    let routePreviewMethod = $state('GET');
    let routePreviewPath = $state('');
    let routePreviewTransport = $state('');
    let routePreviewResult = $state<{ target: string; rule_id?: string; reason?: string } | null>(null);
    let routePreviewError = $state<string | null>(null);
    let routePreviewing = $state(false);

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
    let reauthStep = $state<'password' | 'otp'>('password');
    let reauthMfaId = $state('');
    let reauthOtpId = $state('');
    let reauthOtp = $state('');
    let reauthOtpHint = $state('');
    let pendingAction: (() => Promise<void>) | null = null;

    async function loadData() {
        loading = true;
        try {
            const [si, up, pol, eps, rr] = await Promise.all([
                apiRequest<SystemInfo>('/system'),
                apiRequest<UpstreamDTO>('/upstream'),
                apiRequest<ItemsResponse<Policy>>('/path-policies'),
                apiRequest<ItemsResponse<UpstreamEndpointDTO>>('/upstream/endpoints'),
                apiRequest<ItemsResponse<RouteRuleDTO>>('/route-rules'),
            ]);
            sysInfo = si;
            upstream = up;
            policies = pol.items || [];
            endpoints = eps.items || [];
            routeRules = rr.items || [];
            
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
        reauthStep = 'password';
        reauthMfaId = '';
        reauthOtpId = '';
        reauthOtp = '';
        reauthOtpHint = '';
        showReauthModal = true;
    }

    async function finishReauthWithTicket(ticket: string) {
        reauthTicket = ticket;
        showReauthModal = false;
        if (!pendingAction) return;
        try {
            await pendingAction();
            pendingAction = null;
            reauthTicket = null;
        } catch {
            // pendingAction surfaces its own error (e.g. probeError)
        }
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
            updated: '',
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
            updated: p.Updated || p.updated || '',
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
                if (currentPolicy.updated) {
                    body.updated = currentPolicy.updated;
                }
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

    async function handlePreviewPolicy(e: Event) {
        e.preventDefault();
        previewError = null;
        previewResult = null;
        previewing = true;
        try {
            const qs = new URLSearchParams({
                method: previewMethod,
                path: previewPath,
            });
            previewResult = await apiRequest<PolicyPreviewResult>(`/path-policies/preview?${qs}`);
        } catch (err) {
            previewError = err instanceof Error ? err.message : String(err);
        } finally {
            previewing = false;
        }
    }

    // --- endpoint CRUD ---

    function openNewEndpoint() {
        endpointForm = { id: '', key: '', base_url: '', enabled: true, is_default: false };
        endpointError = null;
        showEndpointModal = true;
    }

    function openEditEndpoint(ep: UpstreamEndpointDTO) {
        endpointForm = { id: ep.id, key: ep.key, base_url: ep.base_url, enabled: ep.enabled, is_default: ep.is_default };
        endpointError = null;
        showEndpointModal = true;
    }

    async function handleSaveEndpoint(e: Event) {
        e.preventDefault();
        endpointError = null;
        endpointSaving = true;
        try {
            const body: UpstreamEndpointBody = {
                key: endpointForm.key.trim(),
                base_url: endpointForm.base_url.trim(),
                enabled: endpointForm.enabled,
                is_default: endpointForm.is_default,
            };
            if (endpointForm.id) {
                await apiRequest(`/upstream/endpoints/${endpointForm.id}`, { method: 'PUT', body: JSON.stringify(body) });
            } else {
                await apiRequest('/upstream/endpoints', { method: 'POST', body: JSON.stringify(body) });
            }
            showEndpointModal = false;
            await loadData();
        } catch (err) {
            endpointError = err instanceof Error ? err.message : String(err);
        } finally {
            endpointSaving = false;
        }
    }

    async function handleDeleteEndpoint(id: string, key: string) {
        if (!confirm(`Delete endpoint "${key}"?`)) return;
        try {
            await apiRequest(`/upstream/endpoints/${id}`, { method: 'DELETE' });
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function handleProbeEndpointWS(id: string, key: string) {
        probingEndpointID = id;
        try {
            const res = await apiRequest<WebSocketProbeResult>(`/upstream/endpoints/${id}/probe-ws`, { method: 'POST' });
            if (res.websocket_capable) {
                alert(`Endpoint "${key}" supports WebSocket (101 handshake OK).`);
            } else {
                alert(`Endpoint "${key}" does not support WebSocket: ${res.error || 'handshake failed'}`);
            }
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        } finally {
            probingEndpointID = null;
        }
    }

    async function setDefaultEndpoint(ep: UpstreamEndpointDTO) {
        try {
            await apiRequest(`/upstream/endpoints/${ep.id}`, {
                method: 'PUT',
                body: JSON.stringify({ key: ep.key, base_url: ep.base_url, enabled: ep.enabled, is_default: true }),
            });
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    // --- route rule CRUD ---

    function openNewRouteRule() {
        routeRuleForm = { id: '', method: '', path: '', transport: '', target: endpoints.find(e => e.is_default)?.key || '', priority: 100, enabled: true, reason: '', updated: '' };
        isEditingRouteRule = false;
        routeRuleError = null;
        showRouteRuleModal = true;
    }

    function openEditRouteRule(r: RouteRuleDTO) {
        routeRuleForm = {
            id: r.id, method: r.method || '', path: r.path, transport: r.transport || '', target: r.target,
            priority: r.priority, enabled: r.enabled, reason: r.reason || '', updated: r.updated || '',
        };
        isEditingRouteRule = true;
        routeRuleError = null;
        showRouteRuleModal = true;
    }

    async function handleSaveRouteRule(e: Event) {
        e.preventDefault();
        routeRuleError = null;
        routeRuleSaving = true;
        try {
            const body: RouteRuleBody = { ...routeRuleForm };
            if (isEditingRouteRule && routeRuleForm.updated) {
                body.updated = routeRuleForm.updated;
            }
            const url = isEditingRouteRule && routeRuleForm.id
                ? `/route-rules/${routeRuleForm.id}`
                : '/route-rules';
            await apiRequest(url, {
                method: isEditingRouteRule && routeRuleForm.id ? 'PUT' : 'POST',
                body: JSON.stringify(body),
            });
            showRouteRuleModal = false;
            await loadData();
        } catch (err) {
            routeRuleError = err instanceof Error ? err.message : String(err);
        } finally {
            routeRuleSaving = false;
        }
    }

    async function handleDeleteRouteRule(id: string) {
        if (!confirm('Delete this route rule?')) return;
        try {
            await apiRequest(`/route-rules/${id}`, { method: 'DELETE' });
            await loadData();
        } catch (err) {
            alert(err instanceof Error ? err.message : String(err));
        }
    }

    async function handlePreviewRouteRule(e: Event) {
        e.preventDefault();
        routePreviewError = null;
        routePreviewResult = null;
        routePreviewing = true;
        try {
            const qs = new URLSearchParams({
                method: routePreviewMethod,
                path: routePreviewPath,
                transport: routePreviewTransport,
            });
            routePreviewResult = await apiRequest<{ target: string; rule_id?: string; reason?: string }>(`/route-rules/preview?${qs}`);
        } catch (err) {
            routePreviewError = err instanceof Error ? err.message : String(err);
        } finally {
            routePreviewing = false;
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

            if (reauthStep === 'password') {
                const result = await reauth(identity, reauthPassword);
                if (result.status === 'mfa') {
                    reauthMfaId = result.mfaId;
                    if (identity.includes('@')) {
                        try {
                            const req = await requestMfaOtp(identity);
                            reauthOtpId = req.otpId;
                            reauthOtpHint = `OTP sent to ${identity}`;
                        } catch (otpErr) {
                            reauthOtpHint = otpErr instanceof Error ? otpErr.message : String(otpErr);
                        }
                    } else {
                        reauthOtpHint = 'Enter the one-time password from your email.';
                    }
                    reauthStep = 'otp';
                    return;
                }
                await finishReauthWithTicket(result.ticket);
                return;
            }

            // OTP step
            let otpId = reauthOtpId;
            if (!otpId) {
                if (!identity.includes('@')) {
                    throw new Error('Email required to request OTP');
                }
                const req = await requestMfaOtp(identity);
                otpId = req.otpId;
                reauthOtpId = otpId;
            }
            const ticket = await completeReauthMfa(reauthMfaId, otpId, reauthOtp);
            await finishReauthWithTicket(ticket);
        } catch (err) {
            reauthError = err instanceof Error ? err.message : String(err);
        } finally {
            reauthLoading = false;
        }
    }

    async function resendReauthOtp() {
        reauthError = null;
        reauthLoading = true;
        try {
            const identity = $session?.email || '';
            if (!identity.includes('@')) {
                throw new Error('Email required to request OTP');
            }
            const req = await requestMfaOtp(identity);
            reauthOtpId = req.otpId;
            reauthOtpHint = `OTP sent to ${identity}`;
        } catch (err) {
            reauthError = err instanceof Error ? err.message : String(err);
        } finally {
            reauthLoading = false;
        }
    }

    function reauthBackToPassword() {
        reauthStep = 'password';
        reauthMfaId = '';
        reauthOtpId = '';
        reauthOtp = '';
        reauthOtpHint = '';
        reauthError = null;
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
                            <input type="password" id="backend_password" bind:value={probeForm.backend_password} placeholder="leave blank to reuse stored password when already configured" />
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
                    <p class="text-sm text-secondary mt-2">
                        Credentials are shared by all endpoints. Saving credentials applies to the default endpoint.
                    </p>
                    <div class="mt-4 flex justify-end">
                        <button type="submit" disabled={probing}>{probing ? 'Probing...' : 'Probe (validates credentials)'}</button>
                    </div>
                </form>
            </div>

            <div class="panel">
                <div class="flex justify-between items-center mb-4">
                    <div class="metric-label" style="margin:0">Upstream Endpoints</div>
                    <button type="button" class="secondary" onclick={openNewEndpoint}>Add Endpoint</button>
                </div>
                <p class="text-sm text-secondary mb-2">
                    Endpoints are CDN/ingress URLs that share the single upstream credential.
                    The default endpoint serves traffic with no matching rule; WebSocket
                    capability is set by probing each endpoint.
                </p>
                <div class="table-container" style="max-height: 320px;">
                    <table style="min-width: 760px;">
                        <thead>
                            <tr>
                                <th>Key</th>
                                <th>Base URL</th>
                                <th>Default</th>
                                <th>WebSocket</th>
                                <th style="text-align: right;">Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            {#if endpoints.length === 0}
                                <tr>
                                    <td colspan="5" class="text-secondary text-center" style="padding: 1.5rem;">No upstream endpoints configured.</td>
                                </tr>
                            {/if}
                            {#each endpoints as ep}
                                <tr>
                                    <td>
                                        <strong class="mono">{ep.key}</strong>
                                        {#if !ep.enabled}
                                            <span class="text-secondary">(disabled)</span>
                                        {/if}
                                    </td>
                                    <td class="mono truncate" style="max-width: 280px;">{ep.base_url}</td>
                                    <td>
                                        {#if ep.is_default}
                                            <span class="status-ok">default</span>
                                        {:else}
                                            <button class="secondary text-xs" onclick={() => setDefaultEndpoint(ep)}>Make default</button>
                                        {/if}
                                    </td>
                                    <td>
                                        {#if ep.websocket_capable}
                                            <span class="status-ok">capable</span>
                                        {:else if ep.websocket_probed_at}
                                            <span class="status-err">not capable</span>
                                        {:else}
                                            <span class="text-secondary">unprobed</span>
                                        {/if}
                                        {#if ep.websocket_probe_error}
                                            <div class="text-xs text-secondary" title={ep.websocket_probe_error}>{ep.websocket_probe_error}</div>
                                        {/if}
                                    </td>
                                    <td>
                                        <div class="flex gap-2 justify-end">
                                            <button class="secondary text-xs" disabled={probingEndpointID === ep.id} onclick={() => handleProbeEndpointWS(ep.id, ep.key)}>
                                                {probingEndpointID === ep.id ? 'Probing...' : 'Probe WS'}
                                            </button>
                                            <button class="secondary text-xs" onclick={() => openEditEndpoint(ep)}>Edit</button>
                                            <button class="danger text-xs" onclick={() => handleDeleteEndpoint(ep.id, ep.key)}>Del</button>
                                        </div>
                                    </td>
                                </tr>
                            {/each}
                        </tbody>
                    </table>
                </div>
            </div>

            <div class="panel">
                <div class="flex justify-between items-center mb-4">
                    <div class="metric-label" style="margin:0">Route Rules</div>
                    <button type="button" onclick={openNewRouteRule}>Add Rule</button>
                </div>
                <p class="text-sm text-secondary mb-2">
                    Route rules send matching requests to a specific endpoint key. Leave method empty for any
                    method; transport can be <span class="mono">websocket</span> (WebSocket Upgrades only),
                    <span class="mono">http</span>, or empty (all). Higher priority wins. Unmatched requests use the default endpoint.
                </p>
                <form class="flex gap-2 items-end flex-wrap" onsubmit={handlePreviewRouteRule}>
                    <div>
                        <label class="text-sm text-secondary block mb-1">Method</label>
                        <select bind:value={routePreviewMethod}>
                            <option value="GET">GET</option>
                            <option value="POST">POST</option>
                            <option value="PUT">PUT</option>
                            <option value="DELETE">DELETE</option>
                            <option value="">* (Any)</option>
                        </select>
                    </div>
                    <div>
                        <label class="text-sm text-secondary block mb-1">Transport</label>
                        <select bind:value={routePreviewTransport}>
                            <option value="">Any</option>
                            <option value="websocket">websocket</option>
                            <option value="http">http</option>
                        </select>
                    </div>
                    <div style="flex: 1; min-width: 180px;">
                        <label class="text-sm text-secondary block mb-1" for="rr_preview_path">Path</label>
                        <input type="text" id="rr_preview_path" class="mono" bind:value={routePreviewPath} placeholder="/embywebsocket" />
                    </div>
                    <button type="submit" class="secondary" disabled={routePreviewing}>{routePreviewing ? 'Checking...' : 'Preview'}</button>
                </form>
                {#if routePreviewError}
                    <div class="error-message mt-2">{routePreviewError}</div>
                {/if}
                {#if routePreviewResult}
                    <div class="text-sm mt-2">
                        Result:
                        <span class="mono">{routePreviewResult.target || '(default endpoint)'}</span>
                        {#if routePreviewResult.reason}
                            <span class="text-secondary"> — {routePreviewResult.reason}</span>
                        {/if}
                    </div>
                {/if}
                <div class="table-container" style="max-height: 320px; margin-top: 1rem;">
                    <table style="min-width: 800px;">
                        <thead>
                            <tr>
                                <th style="width: 8%">Pri</th>
                                <th style="width: 10%">Method</th>
                                <th style="width: 10%">Transport</th>
                                <th style="width: 30%">Path</th>
                                <th style="width: 12%">Target</th>
                                <th style="width: 5%">On</th>
                                <th style="width: 25%; text-align: right;">Actions</th>
                            </tr>
                        </thead>
                        <tbody>
                            {#if routeRules.length === 0}
                                <tr>
                                    <td colspan="7" class="text-secondary text-center" style="padding: 1.5rem;">No route rules configured.</td>
                                </tr>
                            {/if}
                            {#each routeRules as r}
                                <tr>
                                    <td>{r.priority}</td>
                                    <td class="mono">{r.method || '*'}</td>
                                    <td class="mono">{r.transport || 'any'}</td>
                                    <td class="mono">{r.path}</td>
                                    <td class="mono"><span class="status-ok">{r.target}</span></td>
                                    <td>{r.enabled ? 'Yes' : 'No'}</td>
                                    <td>
                                        <div class="flex gap-2 justify-end">
                                            <button class="secondary text-xs" onclick={() => openEditRouteRule(r)}>Edit</button>
                                            <button class="danger text-xs" onclick={() => handleDeleteRouteRule(r.id)}>Del</button>
                                        </div>
                                    </td>
                                </tr>
                            {/each}
                        </tbody>
                    </table>
                </div>
            </div>
        {/if}

        {#if activeTab === 'policies'}
            <div class="panel mb-4">
                <div class="metric-label mb-2">Matching rules</div>
                <p class="text-sm text-secondary mb-2">
                    Paths use exact match, trailing <span class="mono">*</span> prefix match, or single-segment
                    <span class="mono">{'{id}'}</span> parameters (not regular expressions).
                    Higher priority wins among the same action; <strong>deny always beats allow</strong>.
                    Examples: <span class="mono">/Users/*</span>, <span class="mono">/Items/{'{id}'}</span>.
                </p>
                <form class="flex gap-2 items-end flex-wrap" onsubmit={handlePreviewPolicy}>
                    <div>
                        <label class="text-sm text-secondary block mb-1" for="preview_method">Method</label>
                        <select id="preview_method" bind:value={previewMethod}>
                            <option value="GET">GET</option>
                            <option value="POST">POST</option>
                            <option value="PUT">PUT</option>
                            <option value="DELETE">DELETE</option>
                        </select>
                    </div>
                    <div style="flex: 1; min-width: 180px;">
                        <label class="text-sm text-secondary block mb-1" for="preview_path">Path</label>
                        <input type="text" id="preview_path" class="mono" bind:value={previewPath} required placeholder="/Items/abc" />
                    </div>
                    <button type="submit" class="secondary" disabled={previewing}>{previewing ? 'Checking...' : 'Preview'}</button>
                </form>
                {#if previewError}
                    <div class="error-message mt-2">{previewError}</div>
                {/if}
                {#if previewResult}
                    <div class="text-sm mt-2">
                        Decision:
                        <span class={(previewResult.Allowed ?? previewResult.allowed) ? 'status-ok' : 'status-err'}>
                            {(previewResult.Action || previewResult.action || ((previewResult.Allowed ?? previewResult.allowed) ? 'allow' : 'deny')).toString().toUpperCase()}
                        </span>
                        {#if previewResult.Reason || previewResult.reason}
                            <span class="text-secondary"> — {previewResult.Reason || previewResult.reason}</span>
                        {/if}
                        {#if previewResult.PolicyID || previewResult.policy_id}
                            <span class="text-secondary mono"> (policy {previewResult.PolicyID || previewResult.policy_id})</span>
                        {/if}
                    </div>
                {/if}
            </div>

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
                        <div class="metric-label mb-2">Probe Successful (credentials validated)</div>
                        <div class="data-grid" style="grid-template-columns: 1fr;">
                            <div class="metric-box">
                                <div class="metric-label">Server Name</div>
                                <div class="metric-value" style="font-size: 16px;">{probeResult.server_name}</div>
                            </div>
                            <div class="metric-box">
                                <div class="metric-label">Server ID</div>
                                <div class="metric-value mono" style="font-size: 14px;">{probeResult.server_id}</div>
                            </div>
                            <div class="metric-box">
                                <div class="metric-label">Server Version</div>
                                <div class="metric-value mono" style="font-size: 16px;">{probeResult.server_version}</div>
                            </div>
                            {#if probeResult.backend_user_id}
                                <div class="metric-box">
                                    <div class="metric-label">Backend User ID</div>
                                    <div class="metric-value mono" style="font-size: 14px;">{probeResult.backend_user_id}</div>
                                </div>
                            {/if}
                            <div class="metric-box">
                                <div class="metric-label">Latency</div>
                                <div class="metric-value mono" style="font-size: 16px;">{probeResult.latency_ms} ms</div>
                            </div>
                        </div>
                    </div>
                    <div class="text-sm text-secondary">
                        Applying replaces the stored upstream token and credentials used for backend access.
                        Gateway client sessions are not revoked by this action.
                    </div>
                {/if}
            </div>

            <div class="drawer-footer">
                <button class="secondary" onclick={() => showProbeModal = false}>Cancel</button>
                {#if !probing && probeResult}
                    <button class="danger" onclick={requestApply}>Apply Configuration</button>
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
                        <label class="text-sm text-secondary block mb-1" for="p_path">Path</label>
                        <input type="text" id="p_path" bind:value={currentPolicy.path} required placeholder="/Users/*" class="mono" />
                        <p class="text-sm text-secondary mt-1">
                            Exact path, trailing <span class="mono">*</span> prefix, or <span class="mono">/Items/{'{id}'}</span> single-segment params.
                        </p>
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
                        <label class="text-sm text-secondary block mb-1" for="p_priority">Priority (higher number wins; deny always beats allow)</label>
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

{#if showEndpointModal}
    <div class="overlay" onclick={() => showEndpointModal = false}>
        <div class="drawer" style="width: 480px;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">{endpointForm.id ? 'Edit Endpoint' : 'Add Endpoint'}</h3>
                <button class="icon" onclick={() => showEndpointModal = false}>✕</button>
            </div>
            <div class="drawer-body">
                {#if endpointError}
                    <div class="error-message">{endpointError}</div>
                {/if}
                <form id="endpoint-form" onsubmit={handleSaveEndpoint}>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="ep_key">Key</label>
                        <input type="text" id="ep_key" bind:value={endpointForm.key} required class="mono" placeholder="primary / cf / hk ..." />
                        <p class="text-sm text-secondary mt-1">Unique name for this endpoint; route rules reference it.</p>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="ep_url">Base URL</label>
                        <input type="url" id="ep_url" bind:value={endpointForm.base_url} required placeholder="https://cdn.example.com/emby" />
                    </div>
                    <div class="mb-4 flex items-center gap-4">
                        <label class="flex items-center gap-2 text-sm">
                            <input type="checkbox" bind:checked={endpointForm.enabled} style="width:auto" />
                            Enabled
                        </label>
                        <label class="flex items-center gap-2 text-sm">
                            <input type="checkbox" bind:checked={endpointForm.is_default} style="width:auto" />
                            Default endpoint
                        </label>
                    </div>
                    <p class="text-sm text-secondary">
                        Exactly one endpoint must be the enabled default. Disabling or removing the default
                        promotes another enabled endpoint.
                    </p>
                </form>
            </div>
            <div class="drawer-footer">
                <button class="secondary" onclick={() => showEndpointModal = false}>Cancel</button>
                <button type="submit" form="endpoint-form" disabled={endpointSaving}>{endpointSaving ? 'Saving...' : 'Save Endpoint'}</button>
            </div>
        </div>
    </div>
{/if}

{#if showRouteRuleModal}
    <div class="overlay" onclick={() => showRouteRuleModal = false}>
        <div class="drawer" style="width: 480px;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">{isEditingRouteRule ? 'Edit Route Rule' : 'Add Route Rule'}</h3>
                <button class="icon" onclick={() => showRouteRuleModal = false}>✕</button>
            </div>
            <div class="drawer-body">
                {#if routeRuleError}
                    <div class="error-message">{routeRuleError}</div>
                {/if}
                <form id="route-rule-form" onsubmit={handleSaveRouteRule}>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_path">Path</label>
                        <input type="text" id="rr_path" bind:value={routeRuleForm.path} required class="mono" placeholder="/embywebsocket" />
                        <p class="text-sm text-secondary mt-1">Exact path, trailing <span class="mono">*</span> prefix, or <span class="mono">/Items/{'{id}'}</span> params.</p>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_method">Method</label>
                        <select id="rr_method" bind:value={routeRuleForm.method}>
                            <option value="">* (Any)</option>
                            <option value="GET">GET</option>
                            <option value="POST">POST</option>
                            <option value="PUT">PUT</option>
                            <option value="DELETE">DELETE</option>
                        </select>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_transport">Transport</label>
                        <select id="rr_transport" bind:value={routeRuleForm.transport}>
                            <option value="">Any</option>
                            <option value="websocket">WebSocket</option>
                            <option value="http">HTTP only</option>
                        </select>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_target">Target endpoint key</label>
                        <select id="rr_target" bind:value={routeRuleForm.target} required>
                            {#each endpoints.filter(e => e.enabled) as ep}
                                <option value={ep.key}>{ep.key}{ep.is_default ? ' (default)' : ''}</option>
                            {/each}
                        </select>
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_priority">Priority (higher wins)</label>
                        <input type="number" id="rr_priority" bind:value={routeRuleForm.priority} />
                    </div>
                    <div class="mb-4">
                        <label class="text-sm text-secondary block mb-1" for="rr_reason">Reason (optional)</label>
                        <input type="text" id="rr_reason" bind:value={routeRuleForm.reason} />
                    </div>
                    <div class="mb-4 flex items-center gap-2">
                        <input type="checkbox" id="rr_enabled" bind:checked={routeRuleForm.enabled} style="width:auto" />
                        <label class="text-sm text-secondary" for="rr_enabled">Enabled</label>
                    </div>
                </form>
            </div>
            <div class="drawer-footer">
                <button class="secondary" onclick={() => showRouteRuleModal = false}>Cancel</button>
                <button type="submit" form="route-rule-form" disabled={routeRuleSaving}>{routeRuleSaving ? 'Saving...' : 'Save Rule'}</button>
            </div>
        </div>
    </div>
{/if}

{#if showReauthModal}
    <div class="overlay" style="z-index: 100;" onclick={() => showReauthModal = false}>
        <div class="drawer" style="width: 350px; justify-content: center; max-height: 420px; border-radius: 4px; margin: auto; height: auto;" onclick={(e) => e.stopPropagation()}>
            <div class="drawer-header">
                <h3 class="drawer-title">{reauthStep === 'otp' ? 'Two-factor authentication' : 'Confirm Change'}</h3>
                <button class="icon" onclick={() => showReauthModal = false}>✕</button>
            </div>
            
            <div class="drawer-body">
                {#if reauthStep === 'password'}
                    <p class="text-sm text-secondary mb-4">
                        Re-enter your admin password to apply this change.
                        {#if $session?.email}
                            <br />Identity: <span class="mono">{$session.email}</span>
                        {/if}
                    </p>
                {:else}
                    <p class="text-sm text-secondary mb-4">
                        Enter the one-time password to finish re-authentication.
                    </p>
                    {#if reauthOtpHint}
                        <div class="text-sm text-secondary mb-4">{reauthOtpHint}</div>
                    {/if}
                {/if}
                {#if reauthError}
                    <div class="error-message">{reauthError}</div>
                {/if}
                <form id="reauth-form" onsubmit={handleReauthSubmit}>
                    {#if reauthStep === 'password'}
                        <input type="password" placeholder="Admin Password" bind:value={reauthPassword} required autofocus autocomplete="current-password" />
                    {:else}
                        <input type="text" placeholder="One-time password" bind:value={reauthOtp} required autofocus autocomplete="one-time-code" inputmode="numeric" />
                    {/if}
                </form>
            </div>

            <div class="drawer-footer" style="flex-wrap: wrap; gap: 0.5rem;">
                {#if reauthStep === 'otp'}
                    <button type="button" class="secondary" disabled={reauthLoading} onclick={resendReauthOtp}>Resend OTP</button>
                    <button type="button" class="secondary" disabled={reauthLoading} onclick={reauthBackToPassword}>Back</button>
                {:else}
                    <button class="secondary" onclick={() => showReauthModal = false}>Cancel</button>
                {/if}
                <button type="submit" form="reauth-form" disabled={reauthLoading} class="danger">
                    {#if reauthLoading}
                        Verifying...
                    {:else if reauthStep === 'otp'}
                        Verify OTP
                    {:else}
                        Confirm
                    {/if}
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