<script lang="ts">
    import { completeMfa, login, requestMfaOtp } from '../lib/api';

    let identity = $state('');
    let password = $state('');
    let otp = $state('');
    let otpId = $state('');
    let mfaId = $state('');
    let step = $state<'password' | 'otp'>('password');
    let error = $state('');
    let loading = $state(false);
    let otpHint = $state('');

    async function handlePasswordSubmit(e: Event) {
        e.preventDefault();
        error = '';
        loading = true;
        try {
            const result = await login(identity, password);
            if (result.status === 'mfa') {
                mfaId = result.mfaId;
                // Auto-request OTP when identity looks like an email (superuser identity).
                if (identity.includes('@')) {
                    try {
                        const req = await requestMfaOtp(identity);
                        otpId = req.otpId;
                        otpHint = `OTP sent to ${identity}`;
                    } catch (otpErr) {
                        // Still show OTP step; user can retry send.
                        otpHint = otpErr instanceof Error ? otpErr.message : String(otpErr);
                    }
                } else {
                    otpHint = 'Enter the one-time password from your email.';
                }
                step = 'otp';
                return;
            }
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    async function handleOtpSubmit(e: Event) {
        e.preventDefault();
        error = '';
        loading = true;
        try {
            let id = otpId;
            if (!id) {
                if (!identity.includes('@')) {
                    throw new Error('Email required to request OTP');
                }
                const req = await requestMfaOtp(identity);
                id = req.otpId;
                otpId = id;
            }
            await completeMfa(mfaId, id, otp);
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    async function resendOtp() {
        error = '';
        loading = true;
        try {
            if (!identity.includes('@')) {
                throw new Error('Email required to request OTP');
            }
            const req = await requestMfaOtp(identity);
            otpId = req.otpId;
            otpHint = `OTP sent to ${identity}`;
        } catch (err) {
            error = err instanceof Error ? err.message : String(err);
        } finally {
            loading = false;
        }
    }

    function backToPassword() {
        step = 'password';
        mfaId = '';
        otpId = '';
        otp = '';
        otpHint = '';
        error = '';
    }
</script>

<div class="login-container">
    {#if step === 'password'}
        <form class="login-box panel" onsubmit={handlePasswordSubmit}>
            <h2 class="mb-4" style="margin-top: 0;">Emby Auth Gateway</h2>
            
            {#if error}
                <div class="error-message">{error}</div>
            {/if}

            <div class="mb-4">
                <label class="text-sm text-secondary mb-2" style="display: block" for="identity">Admin Email</label>
                <input type="text" id="identity" bind:value={identity} required autocomplete="username" />
            </div>
            
            <div class="mb-4">
                <label class="text-sm text-secondary mb-2" style="display: block" for="password">Password</label>
                <input type="password" id="password" bind:value={password} required autocomplete="current-password" />
            </div>

            <button type="submit" style="width: 100%" disabled={loading}>
                {loading ? 'Authenticating...' : 'Sign In'}
            </button>
        </form>
    {:else}
        <form class="login-box panel" onsubmit={handleOtpSubmit}>
            <h2 class="mb-4" style="margin-top: 0;">Two-factor authentication</h2>

            {#if error}
                <div class="error-message">{error}</div>
            {/if}
            {#if otpHint}
                <div class="text-sm text-secondary mb-4">{otpHint}</div>
            {/if}

            <div class="mb-4">
                <label class="text-sm text-secondary mb-2" style="display: block" for="otp">One-time password</label>
                <input type="text" id="otp" bind:value={otp} required autocomplete="one-time-code" inputmode="numeric" />
            </div>

            <button type="submit" style="width: 100%" disabled={loading} class="mb-2">
                {loading ? 'Verifying...' : 'Verify OTP'}
            </button>
            <button type="button" class="secondary" style="width: 100%" disabled={loading} onclick={resendOtp}>
                Resend OTP
            </button>
            <button type="button" class="secondary" style="width: 100%; margin-top: 0.5rem" disabled={loading} onclick={backToPassword}>
                Back
            </button>
        </form>
    {/if}
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
