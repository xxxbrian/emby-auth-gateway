import { test, expect, type APIRequestContext, type Page } from '@playwright/test';

const baseURL = process.env.ADMIN_E2E_BASE || 'http://127.0.0.1:18090';
const email = process.env.ADMIN_E2E_EMAIL || 'admin@test.local';
const password = process.env.ADMIN_E2E_PASSWORD || 'adminpass123';

async function assertBaseReachable(request: APIRequestContext): Promise<void> {
  try {
    const res = await request.get('/admin/', { timeout: 5_000 });
    if (res.status() >= 500) {
      throw new Error(
        `Admin base URL ${baseURL} returned HTTP ${res.status()}. Start the gateway or set ADMIN_E2E_BASE.`,
      );
    }
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    // Re-throw reachability failures as hard failures (never skip).
    if (message.includes('Admin base URL')) {
      throw err;
    }
    throw new Error(
      `Admin base URL not reachable at ${baseURL} (${message}). Start the gateway (see web/admin/scripts/run-admin-e2e.sh) or set ADMIN_E2E_BASE.`,
    );
  }
}

async function fillLogin(page: Page, identity: string, pass: string): Promise<void> {
  await page.goto('/admin/');
  await expect(page.locator('#identity')).toBeVisible();
  await page.fill('#identity', identity);
  await page.fill('#password', pass);
  await page.click('button[type="submit"]');
}

async function loginAsAdmin(page: Page): Promise<void> {
  await fillLogin(page, email, password);
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
}

/** Capture all string values currently in localStorage / sessionStorage. */
async function readStorage(page: Page): Promise<{
  localKeys: string[];
  sessionKeys: string[];
  localValues: string[];
  sessionValues: string[];
}> {
  return page.evaluate(() => {
    const localKeys = Object.keys(localStorage);
    const sessionKeys = Object.keys(sessionStorage);
    return {
      localKeys,
      sessionKeys,
      localValues: localKeys.map((k) => localStorage.getItem(k) ?? ''),
      sessionValues: sessionKeys.map((k) => sessionStorage.getItem(k) ?? ''),
    };
  });
}

test.describe.configure({ mode: 'serial' });

test.describe('Admin SPA regression', () => {
  test.beforeAll(async ({ request }) => {
    await assertBaseReachable(request);
  });

  test('login page loads', async ({ page }) => {
    await page.goto('/admin/');
    await expect(page.getByRole('heading', { name: 'Emby Auth Gateway' })).toBeVisible();
    await expect(page.locator('#identity')).toBeVisible();
    await expect(page.locator('#password')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign In' })).toBeVisible();
  });

  test('bad password shows error', async ({ page }) => {
    await fillLogin(page, email, 'definitely-wrong-password');
    await expect(page.locator('.error-message')).toBeVisible();
    await expect(page.locator('.error-message')).not.toHaveText('');
    // Still on login — no Overview shell.
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toHaveCount(0);
  });

  test('good login reaches Overview and stores no client tokens', async ({ page }) => {
    let pbToken = '';

    page.on('response', async (response) => {
      try {
        if (!response.url().includes('/auth-with-password') || !response.ok()) {
          return;
        }
        const data = (await response.json()) as { token?: string };
        if (typeof data.token === 'string' && data.token.length > 0) {
          pbToken = data.token;
        }
      } catch {
        // ignore non-JSON or already-consumed bodies
      }
    });

    await loginAsAdmin(page);
    expect(pbToken, 'expected PocketBase token from auth-with-password').not.toEqual('');

    const storage = await readStorage(page);

    // Prefer empty stores (no framework keys expected for this SPA).
    expect(storage.localKeys, `localStorage keys: ${storage.localKeys.join(',')}`).toEqual([]);
    expect(storage.sessionKeys, `sessionStorage keys: ${storage.sessionKeys.join(',')}`).toEqual(
      [],
    );

    // Token string must not appear in any stored value.
    for (const value of [...storage.localValues, ...storage.sessionValues]) {
      expect(value, 'storage value must not contain PB token').not.toContain(pbToken);
    }
  });

  test('nav pages show expected headings', async ({ page }) => {
    await loginAsAdmin(page);

    const pages = [
      { link: 'Users', heading: 'Users' },
      { link: 'Activity', heading: 'Activity' },
      { link: 'Traffic', heading: 'Traffic & Audit' },
      { link: 'System', heading: 'System & Upstream' },
      { link: 'Overview', heading: 'Overview' },
    ];

    for (const { link, heading } of pages) {
      await page.getByRole('link', { name: link, exact: true }).click();
      await expect(page.getByRole('heading', { name: heading, exact: true })).toBeVisible();
    }
  });

  test('create user succeeds; duplicate username surfaces error', async ({ page }) => {
    await loginAsAdmin(page);
    await page.getByRole('link', { name: 'Users', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Users', exact: true })).toBeVisible();

    const suffix = `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
    const username = `e2e_user_${suffix}`;
    const syntheticId = `syn-${suffix}`;
    const userPassword = 'E2ePass123!';

    await page.getByRole('button', { name: 'Create User' }).click();
    await expect(page.getByRole('heading', { name: 'Create User' })).toBeVisible();

    await page.fill('#username', username);
    await page.fill('#password', userPassword);
    await page.fill('#syn_id', syntheticId);

    const createRespPromise = page.waitForResponse(
      (r) =>
        r.url().includes('/admin/api/v1/users') &&
        r.request().method() === 'POST' &&
        r.status() !== 0,
    );
    await page.getByRole('button', { name: 'Save' }).click();
    const createResp = await createRespPromise;
    expect(createResp.status()).toBe(200);
    const createdBody = (await createResp.json()) as {
      id?: string;
      username?: string;
      synthetic_user_id?: string;
      enabled?: boolean;
      ok?: boolean;
    };
    // Contract: UserDTO fields present; never bare {ok:true}.
    expect(createdBody.ok).toBeUndefined();
    expect(createdBody.id).toBeTruthy();
    expect(createdBody.username).toBe(username);
    expect(createdBody.synthetic_user_id).toBe(syntheticId);
    expect(createdBody.enabled).toBe(true);

    // Form closes on success; row appears.
    await expect(page.getByRole('heading', { name: 'Create User' })).toHaveCount(0, {
      timeout: 15_000,
    });
    await expect(page.getByText(username, { exact: true })).toBeVisible();

    // Duplicate create must not silently succeed.
    await page.getByRole('button', { name: 'Create User' }).click();
    await page.fill('#username', username);
    await page.fill('#password', userPassword);
    await page.fill('#syn_id', `other-${syntheticId}`);
    await page.getByRole('button', { name: 'Save' }).click();

    await expect(page.locator('.error-message')).toBeVisible();
    await expect(page.locator('.error-message')).toContainText(
      /already exists|409|conflict|user_exists/i,
    );
    // Form still open (create did not succeed).
    await expect(page.getByRole('heading', { name: 'Create User' })).toBeVisible();
  });

  test('mobile Users action buttons are fully visible', async ({ page }) => {
    const viewport = { width: 390, height: 844 };
    await page.setViewportSize(viewport);
    await loginAsAdmin(page);
    await page.getByRole('link', { name: 'Users', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Users', exact: true })).toBeVisible();

    // Ensure at least one row with action buttons exists.
    let actionButtons = page.locator('.action-row button');
    if ((await actionButtons.count()) === 0) {
      const suffix = `m${Date.now()}`;
      await page.getByRole('button', { name: 'Create User' }).click();
      await page.fill('#username', `e2e_mobile_${suffix}`);
      await page.fill('#password', 'E2ePass123!');
      await page.fill('#syn_id', `syn-m-${suffix}`);
      await page.getByRole('button', { name: 'Save' }).click();
      await expect(page.getByText(`e2e_mobile_${suffix}`, { exact: true })).toBeVisible();
      actionButtons = page.locator('.action-row button');
    }

    await expect(actionButtons.first()).toBeVisible();
    const count = await actionButtons.count();
    expect(count).toBeGreaterThan(0);

    // No horizontal document overflow (do not scroll before measuring).
    const docSize = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(
      docSize.scrollWidth,
      `document scrollWidth ${docSize.scrollWidth} > clientWidth ${docSize.clientWidth}`,
    ).toBeLessThanOrEqual(docSize.clientWidth + 1);

    // Each action button fully within the viewport without scrolling into view.
    for (let i = 0; i < count; i++) {
      const btn = actionButtons.nth(i);
      const box = await btn.boundingBox();
      expect(box, `button ${i} missing bounding box`).not.toBeNull();
      if (!box) continue;
      expect(box.width, `button ${i} width`).toBeGreaterThan(0);
      expect(box.height, `button ${i} height`).toBeGreaterThan(0);
      expect(box.x, `button ${i} left edge`).toBeGreaterThanOrEqual(-1);
      expect(box.y, `button ${i} top edge`).toBeGreaterThanOrEqual(-1);
      expect(box.x + box.width, `button ${i} right edge`).toBeLessThanOrEqual(viewport.width + 1);
      expect(box.y + box.height, `button ${i} bottom edge`).toBeLessThanOrEqual(
        viewport.height + 1,
      );
    }
  });

  test('logout returns to login', async ({ page }) => {
    await loginAsAdmin(page);
    await page.getByRole('button', { name: 'Logout' }).click();
    await expect(page.getByRole('heading', { name: 'Emby Auth Gateway' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign In' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toHaveCount(0);

    // Session must not survive reload.
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Emby Auth Gateway' })).toBeVisible();
    await expect(page.locator('#identity')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign In' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toHaveCount(0);

    // Cookie session endpoint should reject after logout.
    const sessionRes = await page.request.get('/admin/api/v1/session');
    expect(sessionRes.status(), await sessionRes.text()).toBe(401);
  });
});
