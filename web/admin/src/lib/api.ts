import { writable, type Writable } from 'svelte/store';
import type {
  ApiErrorBody,
  ReauthResponse,
  SessionPublic,
} from './types';

export const session: Writable<SessionPublic | null> = writable(null);
export const initialized: Writable<boolean> = writable(false);

let csrfToken: string | null = null;

const ADMIN_API = '/admin/api/v1';
const PB_API = '/api/collections/_superusers';

function errorMessage(data: ApiErrorBody | null | undefined, fallback: string): string {
  return (data && data.message) || fallback;
}

export async function login(identity: string, password: string): Promise<SessionPublic> {
  const res = await fetch(`${PB_API}/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity, password }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };

  if (!res.ok) {
    if (data.mfaId) {
      throw new Error('MFA required; complete via PocketBase admin first');
    }
    throw new Error(errorMessage(data, 'Login failed'));
  }

  const token = data.token;
  if (!token) throw new Error('No token returned');

  const sessRes = await fetch(`${ADMIN_API}/session`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
    credentials: 'include',
  });

  if (!sessRes.ok) {
    const errBody = (await sessRes.json().catch(() => ({}))) as ApiErrorBody;
    throw new Error(errorMessage(errBody, 'Session exchange failed'));
  }

  const sessData = (await sessRes.json()) as SessionPublic;
  csrfToken = sessData.csrf || null;
  session.set(sessData);

  return sessData;
}

export async function reauth(identity: string, password: string): Promise<string> {
  const res = await fetch(`${PB_API}/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity, password }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };
  if (!res.ok) {
    throw new Error(errorMessage(data, 'Re-auth failed'));
  }

  const token = data.token;
  if (!token) throw new Error('No token returned');

  const sessRes = await fetch(`${ADMIN_API}/session/reauth`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(csrfToken ? { 'X-CSRF-Token': csrfToken } : {}),
    },
    body: JSON.stringify({ token }),
    credentials: 'include',
  });

  if (!sessRes.ok) {
    const errBody = (await sessRes.json().catch(() => ({}))) as ApiErrorBody;
    throw new Error(errorMessage(errBody, 'Reauth exchange failed'));
  }

  const sessData = (await sessRes.json()) as ReauthResponse;
  // Reauth recreates the admin session cookie and CSRF.
  csrfToken = sessData.csrf || csrfToken;
  if (sessData.email || sessData.superuser_id) {
    session.set({
      email: sessData.email,
      superuser_id: sessData.superuser_id,
      csrf: sessData.csrf,
      expires_at: sessData.expires_at,
    });
  }
  return sessData.reauth_ticket;
}

export async function logout(): Promise<void> {
  try {
    await fetch(`${ADMIN_API}/session/logout`, {
      method: 'POST',
      credentials: 'include',
      headers: csrfToken ? { 'X-CSRF-Token': csrfToken } : {},
    });
  } catch {
    // best-effort
  }
  csrfToken = null;
  session.set(null);
}

export async function checkSession(): Promise<void> {
  try {
    const res = await fetch(`${ADMIN_API}/session`, {
      method: 'GET',
      credentials: 'include',
    });

    if (res.ok) {
      const data = (await res.json()) as SessionPublic;
      csrfToken = data.csrf || null;
      session.set(data);
    } else {
      session.set(null);
      csrfToken = null;
    }
  } catch {
    session.set(null);
    csrfToken = null;
  } finally {
    initialized.set(true);
  }
}

export async function apiRequest<T = unknown>(
  endpoint: string,
  options: RequestInit = {},
): Promise<T> {
  const url = `${ADMIN_API}${endpoint}`;

  const headers: Record<string, string> = {
    ...(options.headers as Record<string, string> | undefined),
  };

  const method = (options.method || 'GET').toUpperCase();
  if (!['GET', 'HEAD'].includes(method)) {
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
    credentials: 'include',
  });

  if (res.status === 401) {
    session.set(null);
    csrfToken = null;
    throw new Error('Unauthorized');
  }

  let data: (ApiErrorBody & T) | null = null;
  if (res.status !== 204) {
    try {
      data = (await res.json()) as ApiErrorBody & T;
    } catch {
      // no json body
    }
  }

  if (!res.ok) {
    throw new Error(errorMessage(data, `API error: ${res.status}`));
  }

  return data as T;
}
