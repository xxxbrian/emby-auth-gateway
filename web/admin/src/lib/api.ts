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

export type LoginResult =
  | { status: 'ok'; session: SessionPublic }
  | { status: 'mfa'; mfaId: string };

async function exchangeSession(token: string): Promise<SessionPublic> {
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

/**
 * Password login against PocketBase superusers.
 * When MFA is enabled, auth-with-password returns 401 + mfaId (no token).
 */
export async function login(identity: string, password: string): Promise<LoginResult> {
  const res = await fetch(`${PB_API}/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity, password }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };

  if (!res.ok) {
    if (data.mfaId) {
      return { status: 'mfa', mfaId: data.mfaId };
    }
    throw new Error(errorMessage(data, 'Login failed'));
  }

  const token = data.token;
  if (!token) throw new Error('No token returned');

  const sessData = await exchangeSession(token);
  return { status: 'ok', session: sessData };
}

/**
 * Complete MFA by requesting an OTP (email) then authenticating with otpId + OTP password.
 * PB 0.39 flow: request-otp → auth-with-otp (with mfaId query/body).
 */
export async function requestMfaOtp(email: string): Promise<{ otpId: string }> {
  const res = await fetch(`${PB_API}/request-otp`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email }),
  });
  const data = (await res.json().catch(() => ({}))) as ApiErrorBody & { otpId?: string };
  if (!res.ok) {
    throw new Error(errorMessage(data, 'OTP request failed'));
  }
  if (!data.otpId) throw new Error('No otpId returned');
  return { otpId: data.otpId };
}

/**
 * Finish MFA second factor via auth-with-otp (otpId + one-time password + mfaId).
 */
export async function completeMfa(
  mfaId: string,
  otpId: string,
  otp: string,
): Promise<SessionPublic> {
  const res = await fetch(`${PB_API}/auth-with-otp`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ otpId, password: otp, mfaId }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };
  if (!res.ok) {
    throw new Error(errorMessage(data, 'MFA verification failed'));
  }
  const token = data.token;
  if (!token) throw new Error('No token returned');
  return exchangeSession(token);
}

export type ReauthResult =
  | { status: 'ok'; ticket: string }
  | { status: 'mfa'; mfaId: string };

async function exchangeReauthTicket(token: string): Promise<string> {
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

/**
 * Password re-auth for sensitive admin mutations (upstream reconfigure).
 * When MFA is enabled, returns mfaId so the caller can complete OTP then
 * call completeReauthMfa (same PB flow as login).
 */
export async function reauth(identity: string, password: string): Promise<ReauthResult> {
  const res = await fetch(`${PB_API}/auth-with-password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ identity, password }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };
  if (!res.ok) {
    if (data.mfaId) {
      return { status: 'mfa', mfaId: data.mfaId };
    }
    throw new Error(errorMessage(data, 'Re-auth failed'));
  }

  const token = data.token;
  if (!token) throw new Error('No token returned');
  const ticket = await exchangeReauthTicket(token);
  return { status: 'ok', ticket };
}

/**
 * Finish MFA second factor for reauth and return a reauth_ticket.
 */
export async function completeReauthMfa(
  mfaId: string,
  otpId: string,
  otp: string,
): Promise<string> {
  const res = await fetch(`${PB_API}/auth-with-otp`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ otpId, password: otp, mfaId }),
  });

  const data = (await res.json()) as ApiErrorBody & { token?: string };
  if (!res.ok) {
    throw new Error(errorMessage(data, 'MFA verification failed'));
  }
  const token = data.token;
  if (!token) throw new Error('No token returned');
  return exchangeReauthTicket(token);
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
