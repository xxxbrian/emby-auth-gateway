import { writable } from 'svelte/store';

export const session = writable(null);
export const initialized = writable(false);

let csrfToken = null;

// Base API endpoints
const ADMIN_API = '/admin/api/v1';
const PB_API = '/api/collections/_superusers';

export async function login(identity, password) {
    try {
        const res = await fetch(`${PB_API}/auth-with-password`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ identity, password })
        });
        
        const data = await res.json();
        
        if (!res.ok) {
            // Check for MFA
            if (data.mfaId) {
                throw new Error('MFA required; complete via PocketBase admin first');
            }
            throw new Error(data.message || 'Login failed');
        }

        const token = data.token;
        if (!token) throw new Error('No token returned');

        // Exchange for admin session
        const sessRes = await fetch(`${ADMIN_API}/session`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ token }),
            credentials: 'include'
        });

        if (!sessRes.ok) {
            const errBody = await sessRes.json().catch(() => ({}));
            throw new Error(errBody.message || 'Session exchange failed');
        }

        const sessData = await sessRes.json();
        csrfToken = sessData.csrf || sessData.csrf_token || null;
        session.set(sessData);
        
        return sessData;
    } catch (err) {
        throw err;
    }
}

export async function reauth(identity, password) {
    try {
        const res = await fetch(`${PB_API}/auth-with-password`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ identity, password })
        });
        
        const data = await res.json();
        if (!res.ok) {
            throw new Error(data.message || 'Re-auth failed');
        }

        const token = data.token;
        if (!token) throw new Error('No token returned');

        const sessRes = await fetch(`${ADMIN_API}/session/reauth`, {
            method: 'POST',
            headers: { 
                'Content-Type': 'application/json',
                'X-CSRF-Token': csrfToken
            },
            body: JSON.stringify({ token }),
            credentials: 'include'
        });

        if (!sessRes.ok) {
            const errBody = await sessRes.json().catch(() => ({}));
            throw new Error(errBody.message || 'Reauth exchange failed');
        }
        
        const sessData = await sessRes.json();
        // Reauth recreates the admin session cookie and CSRF.
        csrfToken = sessData.csrf || csrfToken;
        if (sessData.email || sessData.superuser_id) {
            session.set({
                email: sessData.email,
                superuser_id: sessData.superuser_id,
                csrf: sessData.csrf,
            });
        }
        return sessData.reauth_ticket;
    } catch (err) {
        throw err;
    }
}

export async function logout() {
    try {
        await fetch(`${ADMIN_API}/session/logout`, {
            method: 'POST',
            credentials: 'include',
            headers: csrfToken ? { 'X-CSRF-Token': csrfToken } : {}
        });
    } catch(e) {}
    csrfToken = null;
    session.set(null);
}

export async function checkSession() {
    try {
        const res = await fetch(`${ADMIN_API}/session`, {
            method: 'GET',
            credentials: 'include'
        });
        
        if (res.ok) {
            const data = await res.json();
            csrfToken = data.csrf || data.csrf_token || null;
            session.set(data);
        } else {
            session.set(null);
            csrfToken = null;
        }
    } catch (err) {
        session.set(null);
        csrfToken = null;
    } finally {
        initialized.set(true);
    }
}

export async function apiRequest(endpoint, options = {}) {
    const url = `${ADMIN_API}${endpoint}`;
    
    const headers = {
        ...options.headers
    };

    if (options.method && !['GET', 'HEAD'].includes(options.method.toUpperCase())) {
        if (csrfToken) {
            headers['X-CSRF-Token'] = csrfToken;
        }
        if (!headers['Content-Type'] && !(options.body instanceof FormData)) {
            headers['Content-Type'] = 'application/json';
        }
    }

    const res = await fetch(url, {
        ...options,
        headers,
        credentials: 'include'
    });

    if (res.status === 401) {
        session.set(null);
        csrfToken = null;
        throw new Error('Unauthorized');
    }

    let data = null;
    if (res.status !== 204) {
        try {
            data = await res.json();
        } catch (e) {
            // no json body
        }
    }

    if (!res.ok) {
        throw new Error((data && data.message) || `API error: ${res.status}`);
    }

    return data;
}
