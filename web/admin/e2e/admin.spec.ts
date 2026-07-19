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
  // PocketBase may rate-limit superuser auth after several rapid logins.
  // If we see the rate limit error, wait and retry.
  const heading = page.getByRole('heading', { name: 'Overview', exact: true });
  const errorEl = page.locator('.error-message');
  try {
    await expect(heading).toBeVisible({ timeout: 5000 });
  } catch {
    // Check if rate-limited and retry with backoff
    if (await errorEl.count() > 0) {
      const text = await errorEl.textContent();
      if (text && /rate limit/i.test(text)) {
        // PocketBase superuser rate limit: wait 30s for recovery
        await page.waitForTimeout(30_000);
        await fillLogin(page, email, password);
        await expect(heading).toBeVisible({ timeout: 10000 });
        return;
      }
    }
    await expect(heading).toBeVisible({ timeout: 10000 });
  }
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
    // Wait for PocketBase auth rate limit window to pass before next login attempt.
    await page.waitForTimeout(3000);
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
      { link: 'Buffer', heading: 'Buffer' },
      { link: 'Traffic', heading: 'Traffic & Audit' },
      { link: 'System', heading: 'System' },
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
    await page.getByRole('button', { name: 'Save User' }).click();
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
    await page.getByRole('button', { name: 'Save User' }).click();

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

    // Ensure at least one row with Disable/Enable action buttons exists.
    const disableButtons = page.getByRole('button', { name: 'Disable' });
    const enableButtons = page.getByRole('button', { name: 'Enable' });
    if ((await disableButtons.count()) === 0 && (await enableButtons.count()) === 0) {
      const suffix = `m${Date.now()}`;
      await page.getByRole('button', { name: 'Create User' }).click();
      await page.fill('#username', `e2e_mobile_${suffix}`);
      await page.fill('#password', 'E2ePass123!');
      await page.fill('#syn_id', `syn-m-${suffix}`);
      await page.getByRole('button', { name: 'Save User' }).click();
      await expect(page.getByText(`e2e_mobile_${suffix}`, { exact: true })).toBeVisible();
    }

    const actionBtn =
      (await disableButtons.count()) > 0
        ? disableButtons.first()
        : enableButtons.first();
    await expect(actionBtn).toBeVisible();

    // Also check row action cluster (Disable/Enable, Pwd, Kick) for the first user row.
    const rowActionButtons = page.locator('table tbody tr').first().getByRole('button');
    await expect(rowActionButtons.first()).toBeVisible();
    const count = await rowActionButtons.count();
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
      const btn = rowActionButtons.nth(i);
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

  test('System Path Policies tab lists policies without error', async ({ page }) => {
    await loginAsAdmin(page);
    await page.getByRole('link', { name: 'System', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'System', exact: true })).toBeVisible();

    await page.getByRole('tab', { name: 'Path Policies' }).click();

    // Expect either a policy table with rows or the empty-state message.
    // Must not show a page-level API error or leave a 404 path.
    await expect(page.locator('.error-message')).toHaveCount(0);
    const empty = page.getByText('No path policies configured.');
    const table = page.locator('table');
    await expect(table).toBeVisible();
    const emptyCount = await empty.count();
    if (emptyCount === 0) {
      await expect(page.locator('table tbody tr').first()).toBeVisible();
    } else {
      await expect(empty).toBeVisible();
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

  test('Buffer page renders with time window controls', async ({ page }) => {
    await loginAsAdmin(page);
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();

    // Time window segmented control uses buttons
    const tabs = page.locator('.segmented-control button[role="tab"]');
    await expect(tabs).toHaveCount(4);
    await expect(tabs.first()).toHaveText('15m');

    // Page body content area must be present
    await expect(page.locator('.page-body')).toBeVisible();

    // Must show either: loading text, disabled notice, error message, or panel content
    const loadingOrContent = page.locator('.page-body .text-secondary, .page-body .disabled-notice, .page-body .error-message, .page-body .panel');
    await expect(loadingOrContent.first()).toBeVisible({ timeout: 5000 });

    // No horizontal document overflow
    const docSize = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(docSize.scrollWidth).toBeLessThanOrEqual(docSize.clientWidth + 1);
  });

  test('Buffer sub-nav, deep-link, mobile, Activity column, and Overview health', async ({ page }) => {
    await loginAsAdmin(page);

    // --- Buffer sub-nav tabs ---
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(3000);

    const subNav = page.locator('.sub-nav');
    if (await subNav.count() > 0) {
      const activeTabEl = subNav.getByRole('tab', { name: /Active Streams/i });
      const recentTab = subNav.getByRole('tab', { name: /Recent Completions/i });
      await expect(activeTabEl).toBeVisible();
      await expect(recentTab).toBeVisible();
      await expect(activeTabEl).toHaveAttribute('aria-selected', 'true');
      await recentTab.click();
      await expect(recentTab).toHaveAttribute('aria-selected', 'true');
      await expect(page.locator('table')).toBeVisible();
    }

    // --- Deep-link feedback ---
    await page.goto('/admin/#/buffer?stream=fake-boot-id:99999');
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(4000);
    await expect(page.locator('.page-body')).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();

    // --- Mobile no overflow ---
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(1000);
    const docSize = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(docSize.scrollWidth, 'mobile no overflow').toBeLessThanOrEqual(docSize.clientWidth + 1);

    // --- Activity transfers Buffer column ---
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.getByRole('link', { name: 'Activity', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();
    await page.getByRole('tab', { name: 'Transfers' }).click();
    await expect(page.locator('th', { hasText: 'Buffer' })).toBeVisible();

    // --- Overview System Health ---
    await page.getByRole('link', { name: 'Overview', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);
    await expect(page.getByText('System Health', { exact: true })).toBeVisible();
  });
});

// --- Buffer integration tests with route mocking ---
// These tests mock buffer API responses to assert exact frontend behavior
// independent of backend deployment state.

test.describe('Buffer mocked API integration', () => {
  test.describe.configure({ mode: 'serial' });
  // Allow longer per-test timeout for rate-limit recovery pauses.
  test.setTimeout(60_000);

  test.beforeAll(async ({ request }) => {
    await assertBaseReachable(request);
  });

  const mockAggregate = {
    enabled: true,
    health: 'healthy' as const,
    health_reasons: [],
    observation_completeness: 'complete' as const,
    allocated_bytes: 52428800,
    hard_budget_bytes: 104857600,
    owned_bytes: 41943040,
    free_bytes: 10485760,
    unallocated_optional_bytes: 52428800,
    request_debt_bytes: 0,
    queued_bytes: 1048576,
    writing_bytes: 524288,
    private_base_bytes: 0,
    active_requests: 2,
    observed_active_requests: 2,
    unobserved_active_requests: 0,
    base_only_requests: 0,
    warning_streams: 0,
    critical_streams: 0,
    pool_contention_count: 0,
    consumer_starvation_count: 0,
    upstream_stall_count: 0,
    downstream_stall_count: 0,
    close_join_stall_count: 0,
    completion_drops: 0,
    live_registration_drops: 0,
  };

  const mockStream = {
    boot_id: 'boot-abc-123',
    stream_id: '42',
    transfer_id: 'xfer-99',
    user_id: 'user-1',
    username: 'alice',
    device: 'Chrome',
    item_id: 'item-555',
    media_mode: 'direct',
    state: 'active',
    producer_state: 'writing',
    consumer_state: 'reading',
    wait_condition: 'none',
    wait_duration_ms: 0,
    health: 'healthy',
    health_reasons: [],
    target_bytes: 26214400,
    owned_bytes: 20971520,
    debt_bytes: 0,
    private_base_bytes: 0,
    queued_bytes: 524288,
    writing_bytes: 262144,
    allocation_blocker: 'none',
    bytes_read: 15728640,
    bytes_written: 10485760,
    started_at: '2026-07-19T20:00:00Z',
    age_ms: 120000,
  };

  async function mockAuthAndLogin(page: Page) {
    // Catch-all for admin API endpoints to prevent 401 session invalidation.
    // Must be registered FIRST so specific route mocks (registered by tests) take priority.
    // Uses route.fallback() so later-registered specific routes override this.
    await page.route('**/admin/api/v1/**', async (route) => {
      const url = route.request().url();
      if (url.includes('/session')) {
        await route.fulfill({ json: {
          email: email,
          superuser_id: 'mock_superuser',
          csrf: 'mock_csrf_token',
          expires_at: new Date(Date.now() + 3600000).toISOString(),
        }});
      } else if (url.includes('/overview')) {
        await route.fulfill({ json: { upstream: null, media_buffer: null }});
      } else if (url.includes('/media-buffer/streams') && !url.includes('streams/')) {
        await route.fulfill({ json: { boot_id: 'mock', items: [], next_cursor: null, has_more: false, observation_completeness: 'complete' }});
      } else if (url.includes('/media-buffer/series')) {
        await route.fulfill({ json: { boot_id: 'mock', window: '15m', interval: '1s', points: [] }});
      } else if (url.includes('/media-buffer/recent')) {
        await route.fulfill({ json: { boot_id: 'mock', items: [] }});
      } else if (url.includes('/activity/')) {
        await route.fulfill({ json: { items: [] }});
      } else {
        await route.fulfill({ json: {} });
      }
    });
    await page.route('**/api/collections/_superusers/auth-with-password', async (route) => {
      await route.fulfill({ json: {
        token: 'mock_pb_token_for_buffer_tests',
        record: { id: 'mock_superuser', email: email },
      }});
    });
    await page.goto('/admin/');
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  }

  async function loginAndMockBufferAPIs(page: Page) {
    // Mock PocketBase auth to bypass rate limiting in mocked integration tests.
    await page.route('**/api/collections/_superusers/auth-with-password', async (route) => {
      await route.fulfill({ json: {
        token: 'mock_pb_token_for_buffer_tests',
        record: { id: 'mock_superuser', email: email },
      }});
    });

    // Mock session endpoints so the SPA considers us logged in.
    await page.route('**/admin/api/v1/session', async (route) => {
      await route.fulfill({ json: {
        email: email,
        superuser_id: 'mock_superuser',
        csrf: 'mock_csrf_token',
        expires_at: new Date(Date.now() + 3600000).toISOString(),
      }});
    });

    // Mock overview to include media_buffer
    await page.route('**/admin/api/v1/overview*', async (route) => {
      await route.fulfill({ json: {
        upstream: null,
        media_buffer: mockAggregate,
      }});
    });

    // Mock streams list
    await page.route('**/admin/api/v1/media-buffer/streams?*', async (route) => {
      const url = new URL(route.request().url());
      const cursor = url.searchParams.get('cursor');
      if (cursor === 'page2') {
        await route.fulfill({ json: {
          boot_id: 'boot-abc-123',
          items: [{ ...mockStream, stream_id: '99', username: 'bob' }],
          next_cursor: null,
          has_more: false,
          observation_completeness: 'complete',
        }});
      } else {
        await route.fulfill({ json: {
          boot_id: 'boot-abc-123',
          items: [mockStream],
          next_cursor: 'page2',
          has_more: true,
          observation_completeness: 'complete',
        }});
      }
    });

    // Mock series with gaps
    await page.route('**/admin/api/v1/media-buffer/series*', async (route) => {
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        window: '15m',
        interval: '1s',
        points: [
          { t: '2026-07-19T20:00:00Z', present: true, domains: { pool: 'coherent', sidecar: 'eventual' }, aggregate: mockAggregate },
          { t: '2026-07-19T20:00:01Z', present: true, domains: { pool: 'coherent', sidecar: 'eventual' }, aggregate: mockAggregate },
          { t: '2026-07-19T20:00:02Z', present: false, domains: null, aggregate: null },
          { t: '2026-07-19T20:00:03Z', present: true, domains: { pool: 'coherent', sidecar: 'eventual' }, aggregate: mockAggregate },
        ],
      }});
    });

    // Mock recent completions
    await page.route('**/admin/api/v1/media-buffer/recent*', async (route) => {
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        items: [],
      }});
    });

    // Navigate to admin — session mock makes us "logged in" immediately
    await page.goto('/admin/');
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  }

  test('direct detail fetch includes boot_id and expands item', async ({ page }) => {
    // Mocked tests need a valid admin session. The loginAsAdmin helper includes
    // rate-limit retry logic with 30s backoff — this handles both fresh instances
    // (no rate limit) and long-lived ones (rate limit from prior serial block).
    let detailRequested = false;
    let detailUrl = '';

    await page.route('**/admin/api/v1/media-buffer/streams/42?*', async (route) => {
      detailRequested = true;
      detailUrl = route.request().url();
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        item: mockStream,
      }});
    });

    await loginAndMockBufferAPIs(page);
    await page.goto('/admin/#/buffer?stream=boot-abc-123:42');
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    expect(detailRequested, 'detail endpoint must be called').toBe(true);
    expect(detailUrl, 'detail URL must include boot_id').toContain('boot_id=boot-abc-123');

    // Stream should be expanded
    const detail = page.locator('#detail-42');
    await expect(detail).toBeVisible();
    await expect(detail).toContainText('alice');
    await expect(detail).toContainText('xfer-99');
  });

  test('stale_boot renders precise restart message', async ({ page }) => {
    await page.route('**/admin/api/v1/media-buffer/streams/42?*', async (route) => {
      await route.fulfill({
        status: 409,
        json: { error: 'stale_boot', message: 'Boot ID does not match current instance' },
      });
    });

    await loginAndMockBufferAPIs(page);
    await page.goto('/admin/#/buffer?stream=old-boot:42');
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    const banner = page.locator('.info-banner');
    await expect(banner).toBeVisible();
    await expect(banner).toContainText('Gateway restarted');
  });

  test('cursor Load more requests next_cursor and appends', async ({ page }) => {
    await loginAndMockBufferAPIs(page);
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    // Should show "Load more" since has_more=true
    const loadMore = page.getByRole('button', { name: /Load more/i });
    await expect(loadMore).toBeVisible();
    // Filter context shows "1 of 1 loaded"
    await expect(page.getByText('1 of 1 loaded')).toBeVisible();

    await loadMore.click();
    await page.waitForTimeout(1500);

    // After loading page2, filter context shows "2 of 2 loaded"
    await expect(page.getByText('2 of 2 loaded')).toBeVisible();
    // Load more should disappear (has_more=false on page2)
    await expect(loadMore).toBeHidden();
  });

  test('series present:false remains a gap in chart', async ({ page }) => {
    await loginAndMockBufferAPIs(page);
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    // Charts should render (SVG paths present)
    const chart = page.locator('.chart').first();
    await expect(chart).toBeVisible();

    // The chart title should mention gaps
    const title = await chart.getAttribute('title');
    expect(title).toContain('gap');
  });

  test('visual: aggregate cards within viewport at 390px and expand visible', async ({ page }) => {
    await loginAndMockBufferAPIs(page);

    // Override recent completions with populated data (last-registered route takes priority)
    await page.route('**/admin/api/v1/media-buffer/recent*', async (route) => {
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        items: [{
          stream_id: 'comp-1',
          boot_id: 'boot-abc-123',
          username: 'dave',
          user_id: 'u4',
          item_id: 'movie-long-name-test',
          device: 'Roku',
          media_mode: 'direct',
          outcome: 'completed',
          peak_owned_bytes: 67108864,
          bytes_written: 52428800,
          duration_ms: 7200000,
          completed_at: '2026-07-19T21:00:00Z',
          transfer_id: 'xfer-comp-1',
          health_reasons: [],
        }],
      }});
    });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    // No document overflow
    const docSize = await page.evaluate(() => ({
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }));
    expect(docSize.scrollWidth, 'no doc overflow at 390px').toBeLessThanOrEqual(docSize.clientWidth + 1);

    // Aggregate metric boxes must be within viewport (not clipped)
    const metricBoxes = page.locator('.metric-box');
    const count = await metricBoxes.count();
    expect(count).toBeGreaterThan(0);
    for (let i = 0; i < count; i++) {
      const box = await metricBoxes.nth(i).boundingBox();
      if (box) {
        expect(box.x + box.width, `metric-box[${i}] right edge`).toBeLessThanOrEqual(390 + 2);
      }
    }

    // --- Active streams table: expand button within container at scrollLeft=0 ---
    const tableContainer = page.locator('.table-container').first();
    await expect(tableContainer).toBeVisible();

    // scrollLeft must be 0
    const scrollInfo = await tableContainer.evaluate(el => ({
      scrollLeft: el.scrollLeft,
      clientWidth: el.clientWidth,
      scrollWidth: el.scrollWidth,
      rect: el.getBoundingClientRect(),
    }));
    expect(scrollInfo.scrollLeft, 'table scrollLeft').toBe(0);
    // scrollWidth should not exceed clientWidth (no horizontal overflow)
    expect(scrollInfo.scrollWidth, 'table no h-overflow').toBeLessThanOrEqual(scrollInfo.clientWidth + 1);

    // First expand button fully inside container bounds
    const expandBtn = page.locator('.streams-table .expand-btn').first();
    await expect(expandBtn).toBeVisible();
    const btnBox = await expandBtn.boundingBox();
    expect(btnBox).not.toBeNull();
    expect(btnBox!.x, 'expand btn left >= container left').toBeGreaterThanOrEqual(scrollInfo.rect.left);
    expect(btnBox!.x + btnBox!.width, 'expand btn right <= container right').toBeLessThanOrEqual(scrollInfo.rect.left + scrollInfo.rect.width + 1);

    // Click expand and verify detail is visible
    await expandBtn.click();
    const detail = page.locator('.stream-detail').first();
    await expect(detail).toBeVisible();

    // --- Recent completions tab: same checks ---
    const recentTab = page.locator('.sub-nav').getByRole('tab', { name: /Recent/i });
    await recentTab.click();
    await page.waitForTimeout(1000);

    const recentContainer = page.locator('.table-container').first();
    const recentScroll = await recentContainer.evaluate(el => ({
      scrollLeft: el.scrollLeft,
      clientWidth: el.clientWidth,
      scrollWidth: el.scrollWidth,
      rect: el.getBoundingClientRect(),
    }));
    expect(recentScroll.scrollLeft, 'recent scrollLeft').toBe(0);
    expect(recentScroll.scrollWidth, 'recent no h-overflow').toBeLessThanOrEqual(recentScroll.clientWidth + 1);

    // Recent expand button within container
    const recentExpand = page.locator('.recent-table .expand-btn').first();
    if (await recentExpand.count() > 0) {
      await expect(recentExpand).toBeVisible();
      const rBtnBox = await recentExpand.boundingBox();
      expect(rBtnBox).not.toBeNull();
      expect(rBtnBox!.x + rBtnBox!.width, 'recent expand right <= container').toBeLessThanOrEqual(recentScroll.rect.left + recentScroll.rect.width + 1);
      expect(rBtnBox!.x, 'recent expand left >= container').toBeGreaterThanOrEqual(recentScroll.rect.left);
    }
  });

  test('visual: inactive segmented tabs have no accent background', async ({ page }) => {
    await loginAndMockBufferAPIs(page);
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();

    // Check the time window segmented control — inactive tabs must not use accent blue
    const inactiveTabs = page.locator('.segmented-control .tab:not(.active)');
    const tabCount = await inactiveTabs.count();
    expect(tabCount).toBeGreaterThan(0);
    for (let i = 0; i < tabCount; i++) {
      const bg = await inactiveTabs.nth(i).evaluate(el => getComputedStyle(el).backgroundColor);
      // Should be transparent (rgba(0,0,0,0)) or a very dark subdued tone, NOT blue (rgb(37,99,235))
      expect(bg, `tab[${i}] bg`).not.toContain('37, 99, 235');
    }
  });

  test('visual: selected transfer row has accent treatment', async ({ page }) => {
    // Mock activity to return a transfer and navigate with buffer pair highlight
    await mockAuthAndLogin(page);
    await page.route('**/admin/api/v1/activity/transfers', async (route) => {
      await route.fulfill({ json: {
        items: [{
          session_id: 'sess-1',
          user_id: 'user-1',
          username: 'alice',
          device: 'Chrome',
          item_id: 'item-1',
          media_mode: 'direct',
          bytes_in: 0,
          bytes_out: 1000,
          started_at: '2026-07-19T20:00:00Z',
          last_seen: '2026-07-19T20:02:00Z',
          media_buffer: { boot_id: 'boot-1', stream_id: '1' },
        }],
      }});
    });

    // Navigate with buffer pair query — Activity matches via media_buffer identity
    await page.goto('/admin/#/activity?tab=transfers&buffer=boot-1:1');
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();
    await page.waitForTimeout(1500);

    // Row ID is derived from boot_id:stream_id pair (colon → underscore)
    const row = page.locator('#transfer-row-boot-1_1');
    await expect(row).toBeVisible();
    await expect(row).toHaveClass(/row-highlighted/);

    // Verify box-shadow includes accent color (left inset)
    const shadow = await row.evaluate(el => getComputedStyle(el).boxShadow);
    expect(shadow, 'row has accent box-shadow').not.toBe('none');
  });

  test('visual: missing buffer pair shows info notice', async ({ page }) => {
    await mockAuthAndLogin(page);
    await page.route('**/admin/api/v1/activity/transfers', async (route) => {
      await route.fulfill({ json: {
        items: [{
          session_id: 'sess-1',
          user_id: 'user-1',
          username: 'bob',
          device: 'TV',
          item_id: 'movie-1',
          media_mode: 'direct',
          bytes_in: 0,
          bytes_out: 5000,
          started_at: '2026-07-19T20:00:00Z',
          last_seen: '2026-07-19T20:02:00Z',
          media_buffer: { boot_id: 'boot-other', stream_id: '99' },
        }],
      }});
    });

    // Navigate with a pair that does NOT match any row
    await page.goto('/admin/#/activity?tab=transfers&buffer=boot-missing:999');
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();
    await page.waitForTimeout(1500);

    // Info notice should be visible
    const notice = page.locator('.info-notice');
    await expect(notice).toBeVisible();
    await expect(notice).toContainText('not found');
  });

  test('disabled 200 shows distinct disabled notice', async ({ page }) => {
    await mockAuthAndLogin(page);
    // Override overview AFTER mockAuthAndLogin — last route registered wins in Playwright
    await page.route('**/admin/api/v1/overview*', async (route) => {
      await route.fulfill({ json: {
        upstream: null,
        media_buffer: { ...mockAggregate, enabled: false, health: 'disabled' },
      }});
    });

    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    await expect(page.locator('.disabled-notice')).toBeVisible();
    await expect(page.locator('.disabled-notice')).toContainText('not enabled');
  });

  test('provider 503 shows distinct unavailable error', async ({ page }) => {
    await mockAuthAndLogin(page);
    // Override overview AFTER mockAuthAndLogin
    await page.route('**/admin/api/v1/overview*', async (route) => {
      await route.fulfill({
        status: 503,
        json: { error: 'provider_unavailable', message: 'Buffer provider unavailable' },
      });
    });

    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    const errorMsg = page.locator('.error-message');
    await expect(errorMsg).toBeVisible();
    await expect(errorMsg).toContainText('provider unavailable');
  });

  test('Overview limited completeness is visible', async ({ page }) => {
    await mockAuthAndLogin(page);
    // Override overview AFTER mockAuthAndLogin
    await page.route('**/admin/api/v1/overview*', async (route) => {
      await route.fulfill({ json: {
        upstream: null,
        media_buffer: {
          ...mockAggregate,
          observation_completeness: 'limited',
          observed_active_requests: 1,
          unobserved_active_requests: 1,
          active_requests: 2,
        },
      }});
    });

    // Force re-fetch by navigating away and back
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await page.getByRole('link', { name: 'Overview', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
    await page.waitForTimeout(3000);

    // Sentinel must show "limited" state
    const sentinel = page.locator('.main-content');
    await expect(sentinel).toContainText(/limited/i);
  });

  test('Buffer-to-Activity opens Transfers and highlights transfer', async ({ page }) => {
    // Mock activity transfers to return a transfer with matching buffer pair
    await page.route('**/admin/api/v1/activity/transfers', async (route) => {
      await route.fulfill({ json: {
        items: [{
          session_id: 'sess-1',
          user_id: 'user-1',
          username: 'alice',
          device: 'Chrome',
          item_id: 'item-555',
          media_mode: 'direct',
          bytes_in: 0,
          bytes_out: 10485760,
          started_at: '2026-07-19T20:00:00Z',
          last_seen: '2026-07-19T20:02:00Z',
          media_buffer: { boot_id: 'boot-abc-123', stream_id: '42' },
        }],
      }});
    });

    await loginAndMockBufferAPIs(page);
    await page.getByRole('link', { name: 'Buffer', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    // Expand stream detail and click activity link (uses buffer pair identity)
    const expandBtn = page.locator('.expand-btn').first();
    await expandBtn.click();
    const activityLink = page.locator('a[href*="#/activity?tab=transfers&buffer="]');
    await expect(activityLink).toBeVisible();
    await activityLink.click();

    // Should navigate to Activity with Transfers tab selected
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();
    const transfersTab = page.getByRole('tab', { name: 'Transfers' });
    await expect(transfersTab).toHaveAttribute('aria-selected', 'true');

    // The highlighted row should exist (ID derived from boot_id:stream_id)
    await page.waitForTimeout(2000);
    const highlightedRow = page.locator('#transfer-row-boot-abc-123_42');
    await expect(highlightedRow).toBeVisible();
    await expect(highlightedRow).toHaveClass(/row-highlighted/);
  });

  test('Activity-to-Buffer issues detail fetch via stream link', async ({ page }) => {
    let bufferDetailCalled = false;

    // mockAuthAndLogin FIRST (registers catch-all), then specific overrides AFTER (take priority)
    await mockAuthAndLogin(page);

    await page.route('**/admin/api/v1/activity/transfers', async (route) => {
      await route.fulfill({ json: {
        items: [{
          session_id: 'sess-1',
          user_id: 'user-1',
          username: 'alice',
          device: 'Chrome',
          item_id: 'item-555',
          media_mode: 'direct',
          bytes_in: 0,
          bytes_out: 10485760,
          started_at: '2026-07-19T20:00:00Z',
          last_seen: '2026-07-19T20:02:00Z',
          media_buffer: { boot_id: 'boot-abc-123', stream_id: '42' },
        }],
      }});
    });

    await page.route('**/admin/api/v1/media-buffer/streams/42?*', async (route) => {
      bufferDetailCalled = true;
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        item: mockStream,
      }});
    });

    // Buffer page endpoints
    await page.route('**/admin/api/v1/media-buffer/streams?*', async (route) => {
      await route.fulfill({ json: {
        boot_id: 'boot-abc-123',
        items: [mockStream],
        next_cursor: null,
        has_more: false,
        observation_completeness: 'complete',
      }});
    });
    await page.route('**/admin/api/v1/media-buffer/series*', async (route) => {
      await route.fulfill({ json: { boot_id: 'boot-abc-123', window: '15m', interval: '1s', points: [] }});
    });
    await page.route('**/admin/api/v1/media-buffer/recent*', async (route) => {
      await route.fulfill({ json: { boot_id: 'boot-abc-123', items: [] }});
    });
    await page.route('**/admin/api/v1/overview*', async (route) => {
      await route.fulfill({ json: { upstream: null, media_buffer: mockAggregate }});
    });

    await page.getByRole('link', { name: 'Activity', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();

    // Switch to Transfers tab
    await page.getByRole('tab', { name: 'Transfers' }).click();
    await page.waitForTimeout(1500);

    // Click the buffer stream link
    const streamLink = page.locator('a.buffer-link').first();
    await expect(streamLink).toBeVisible();
    await streamLink.click();

    // Should navigate to Buffer page
    await expect(page.getByRole('heading', { name: 'Buffer', exact: true })).toBeVisible();
    await page.waitForTimeout(2000);

    // Detail fetch should have been issued
    expect(bufferDetailCalled, 'Buffer detail fetch must be issued from Activity link').toBe(true);
  });
});
