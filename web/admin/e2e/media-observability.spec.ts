import { test, expect, type Page } from '@playwright/test';
import { mkdir } from 'node:fs/promises';
import type { BufferAggregate, BufferCompletion, BufferSeriesResponse, BufferStream, MediaItem, Playback } from '../src/lib/types';
import type { AuditRecord } from '../src/lib/audit-types';
import type { UserMediaItem } from '../src/lib/user-media-types';

// Browser plugin not available. These exercise the real SPA with deterministic
// API fixtures; Go httptest covers authentication, transport and persistence.
const source = 'catalogue-qa';
const boot = 'boot-media-qa';
const stamp = '2026-09-12T08:00:00Z';
const poster = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j2ioAAAAASUVORK5CYII=', 'base64');
const episode: MediaItem = {
  id: 'episode-6', source_ref: source, status: 'available', fetched_at: stamp,
  type: 'Episode', name: 'The Signal', series_name: 'Beyond the Horizon', season_number: 1, episode_number: 6,
  production_year: 2025, runtime_ticks: 27_000_000_000,
  image: `/admin/api/v1/media/items/episode-6/images/Primary?size=small&source_ref=${source}`,
  overview: 'A radio signal leads the crew to an abandoned observatory above the clouds.',
  genres: ['Science Fiction', 'Drama'], community_rating: 8.7, official_rating: 'TV-PG', container: 'mkv',
  media_streams: [{ type: 'Video', codec: 'hevc', width: 1920, height: 1080 }, { type: 'Audio', codec: 'aac', language: 'en', channels: 2 }],
};

function aggregate(overrides: Partial<BufferAggregate> = {}): BufferAggregate {
  return { enabled: true, health: 'healthy', health_reasons: [], hard_budget_bytes: 64 * 1048576,
    allocated_bytes: 32 * 1048576, owned_bytes: 24 * 1048576, free_bytes: 8 * 1048576, unallocated_optional_bytes: 32 * 1048576,
    private_base_bytes: 1048576, queued_bytes: 8 * 1048576, writing_bytes: 1048576,
    active_requests: 1, observed_active_requests: 1, unobserved_active_requests: 0, observation_completeness: 'complete',
    base_only_requests: 0, indebted_requests: 0, request_debt_bytes: 0, buffer_acquire_count: 0, pool_contention_count: 0,
    consumer_starvation_count: 0, upstream_stall_count: 0, downstream_stall_count: 0, close_join_stall_count: 0,
    warning_streams: 0, critical_streams: 0, completion_drops: 0, live_registration_drops: 0, ...overrides };
}
function stream(id = '42', overrides: Partial<BufferStream> = {}): BufferStream {
  return { boot_id: boot, stream_id: id, transfer_id: `transfer-${id}`, user_id: 'user-a', username: 'alice', device: 'Living room TV',
    item_id: episode.id, source_ref: source, media_mode: 'range', state: 'active', producer_state: 'reading_optional', consumer_state: 'writing',
    allocation_blocker: 'none', target_bytes: 24 * 1048576, owned_bytes: 16 * 1048576, debt_bytes: 0, private_base_bytes: 1048576,
    queued_bytes: 8 * 1048576, writing_bytes: 1048576, bytes_read: 64 * 1048576, bytes_written: 55 * 1048576,
    wait_condition: 'none', wait_started_at: null, wait_duration_ms: 0, health: 'healthy', health_reasons: [], started_at: stamp, age_ms: 60000, ...overrides };
}
function completion(id: string): BufferCompletion {
  return { completion_id: id, stream_id: `stream-${id}`, boot_id: boot, transfer_id: `transfer-${id}`, user_id: 'user-a', username: `viewer-${id}`,
    device: 'Living room TV', item_id: episode.id, source_ref: source, media_mode: 'range', final_state: 'closing', final_producer_state: 'done',
    final_consumer_state: 'done', final_allocation_blocker: 'none', outcome: 'upstream_error', started_at: stamp, completed_at: stamp, duration_ms: 120000,
    bytes_read: 64000000, bytes_written: 63000000, peak_owned_bytes: 24000000, peak_debt_bytes: 0, peak_queued_bytes: 16000000, peak_writing_bytes: 1000000,
    invariant_observed: false, waits_ms: { buffer_acquire: { total: 50, max: 50 }, pool_contention: { total: 0, max: 0 },
      consumer_starvation: { total: 12000, max: 12000 }, upstream_stall: { total: 12000, max: 12000 }, downstream_stall: { total: 0, max: 0 }, close_join_stall: { total: 0, max: 0 } } };
}
function playback(id = 'session-a', overrides: Partial<Playback> = {}): Playback {
  return { session_id: id, user_id: 'user-a', username: id, device: 'TV', item_id: episode.id, source_ref: source,
    item_name: 'Saved episode title', position_ticks: 9_000_000_000, is_paused: false, last_seen: stamp, started_at: stamp, ...overrides };
}
function audit(id = 'error-a'): AuditRecord {
  return { id, created: stamp, event: 'media_copy_failed', gateway_user_id: 'user-a', synthetic_user_id: '', remote_ip: '127.0.0.1',
    method: 'GET', path: '/Videos/{id}/stream', status: 206, upstream_status: 206, duration_ms: 12000, bytes_transferred: 1048576,
    error_kind: 'unexpected_eof', direction: 'upstream', message: `The upstream response ended before its expected length (${id}).`,
    response_committed: true, is_error: true, severity: 'error' };
}
function localMedia(): UserMediaItem {
  return { id: 'local-state-a', item_id: episode.id, item_name: episode.name!, item_type: 'Episode', series_name: episode.series_name!, season_number: 1,
    episode_number: 6, runtime_ticks: 27_000_000_000, position_ticks: 9_000_000_000, played: false, is_favorite: true, last_played_at: stamp,
    updated_at: stamp, orphaned: false, source_ref: source };
}

interface FixtureOptions {
  aggregate?: BufferAggregate; streams?: BufferStream[]; nextStreams?: BufferStream[]; playbacks?: Playback[];
  media?: Record<string, MediaItem>; brokenImages?: boolean; recentPages?: boolean;
}
async function fixture(page: Page, options: FixtureOptions = {}) {
  const calls: URL[] = []; const errors: string[] = [];
  page.on('pageerror', err => errors.push(err.message));
  page.on('console', message => { if (message.type() === 'error' && !message.text().startsWith('Failed to load resource:')) errors.push(message.text()); });
  const current = options.aggregate || aggregate(); const rows = options.streams || [stream()];
  const media = { [episode.id]: episode, ...options.media };
  await page.route('**/admin/api/v1/**', async route => {
    const url = new URL(route.request().url()); calls.push(url);
    const path = url.pathname.replace('/admin/api/v1', ''); const q = url.searchParams;
    const json = async (value: unknown) => route.fulfill({ json: value });
    if (path === '/session') return json({ email: 'qa@example.test', superuser_id: 'qa-admin', csrf: 'test-csrf', expires_at: '2099-01-01T00:00:00Z' });
    if (path === '/overview') return json({ upstream: null, media_buffer: current, traffic: { error_rate_15m: .05 }, now: stamp, boot_id: boot });
    if (path === '/media-buffer') return json({ boot_id: boot, now: stamp, started_at: stamp, media_buffer: current });
    if (path === '/media-buffer/streams') return json({ boot_id: boot, items: q.has('cursor') ? options.nextStreams || [] : rows,
      next_cursor: !q.has('cursor') && options.nextStreams ? 'stream-page-2' : null, has_more: !q.has('cursor') && !!options.nextStreams, observation_completeness: 'complete' });
    if (path.startsWith('/media-buffer/streams/')) return json({ boot_id: boot, item: [...rows, ...options.nextStreams || []].find(row => row.stream_id === path.split('/').at(-1)) || null });
    if (path === '/media-buffer/series') {
      const response: BufferSeriesResponse = { boot_id: boot, started_at: stamp, available_from: stamp, window: q.get('window') || '24h', interval: '1m', points: [
        { t: '2026-09-12T08:00:00Z', present: true, domains: { pool: 'coherent', sidecar: 'eventual' }, aggregate: aggregate(), peaks: { upstream_stall_count: 1 } },
        { t: '2026-09-12T08:01:00Z', present: false, domains: null, aggregate: null },
        { t: '2026-09-12T08:02:00Z', present: true, domains: { pool: 'coherent', sidecar: 'eventual' }, aggregate: aggregate({ allocated_bytes: 16 * 1048576 }), peaks: { upstream_stall_count: 2 } },
      ] }; return json(response);
    }
    if (path === '/media-buffer/recent') return json({ boot_id: boot, items: options.recentPages ? [completion(q.has('cursor') ? 'older' : 'newest')] : [],
      next_cursor: options.recentPages && !q.has('cursor') ? 'completion-page-2' : null, has_more: !!options.recentPages && !q.has('cursor'),
      capacity: 2048, retained_count: options.recentPages ? 2 : 0, evicted_count: 7, retention_seconds: 86400, oldest_retained_at: stamp, available_from: stamp, started_at: stamp });
    if (path.startsWith('/media-buffer/recent/')) return json({ boot_id: boot, item: completion(path.split('/').at(-1)!) });
    if (path === '/activity/playbacks') return json({ items: options.playbacks || [playback()] });
    if (path === '/activity/transfers') return json({ items: [{ ...playback(), media_mode: 'range', bytes_in: 64000000, bytes_out: 55000000, media_buffer: { boot_id: boot, stream_id: '42' } }] });
    if (path === '/users') return json({ items: [{ id: 'user-a', username: 'alice', synthetic_user_id: 'local-alice', enabled: true, created: stamp }] });
    if (path === '/users/user-a/media') return json({ items: [localMedia()], view: q.get('view') || 'recent', next_cursor: '', has_more: false });
    if (path === '/audit') return json({ items: [audit(q.has('cursor') ? 'error-b' : 'error-a')], has_more: !q.has('cursor'), next_cursor: q.has('cursor') ? '' : 'audit-page-2', from: q.get('from'), to: q.get('to') });
    if (path.startsWith('/audit/')) return json(audit(path.split('/').at(-1)));
    if (path === '/media/items') return json({ items: (q.get('ids') || '').split(',').map(id => media[id] || { id, source_ref: source, status: 'missing' }) });
    if (path.includes('/images/')) return options.brokenImages ? route.fulfill({ status: 404, json: { message: 'Media image is unavailable' } }) : route.fulfill({ contentType: 'image/png', body: poster });
    if (path.startsWith('/media/items/')) return json(media[path.split('/').at(-1)!] || { id: path.split('/').at(-1), source_ref: source, status: 'missing' });
    return json({ items: [] });
  });
  return { calls, errors };
}
async function noOverflow(page: Page) {
  const size = await page.evaluate(() => ({ width: document.documentElement.clientWidth, scroll: document.documentElement.scrollWidth }));
  expect(size.scroll, 'document must stay inside viewport').toBeLessThanOrEqual(size.width + 1);
}
async function screenshot(page: Page, name: string) {
  await mkdir('/tmp/eag-admin-qa', { recursive: true });
  await page.screenshot({ path: `/tmp/eag-admin-qa/${name}.png`, fullPage: false });
}

test.describe('Media and observability acceptance', () => {
  test('visible media is batched once and its drawer preserves route and keyboard focus', async ({ page }) => {
    const state = await fixture(page, { playbacks: [playback(), playback('session-b')] });
    await page.goto('/admin/#/activity?tab=playbacks');
    await expect(page.getByRole('heading', { name: 'Activity', exact: true })).toBeVisible();
    const buttons = page.getByRole('button', { name: 'View media: The Signal', exact: true });
    await expect(buttons).toHaveCount(2);
    const batches = state.calls.filter(url => url.pathname.endsWith('/media/items'));
    expect(batches).toHaveLength(1); expect(batches[0].searchParams.get('ids')).toBe(episode.id);
    await buttons.first().click();
    const drawer = page.getByRole('dialog', { name: 'Media details' });
    await expect(drawer).toBeVisible();
    await expect(drawer.getByRole('heading', { name: 'The Signal', exact: true })).toBeVisible();
    await expect(drawer).toContainText('Beyond the Horizon · S01E06');
    await expect(drawer).toContainText(episode.overview!);
    await expect(drawer).toContainText('1920 × 1080');
    await expect(drawer.getByRole('img', { name: 'Cover for The Signal' })).toBeVisible();
    expect(page.url()).toContain('tab=playbacks'); expect(page.url()).toContain('media=episode-6');
    await screenshot(page, 'media-details-desktop');
    await page.keyboard.press('Escape');
    await expect(drawer).toBeHidden(); await expect(buttons.first()).toBeFocused();
    expect(page.url()).not.toContain('media='); expect(page.url()).toContain('tab=playbacks');
    expect(state.errors).toEqual([]);
  });

  test('missing, unverified and broken images degrade without losing activity rows', async ({ page }) => {
    const state = await fixture(page, { brokenImages: true, playbacks: [playback(), playback('removed', { item_id: 'deleted', item_name: 'Saved deleted title' }), playback('legacy', { item_id: 'legacy-id', source_ref: null, item_name: 'Legacy title' })] });
    await page.goto('/admin/#/activity');
    await expect(page.getByRole('button', { name: 'View media: The Signal' })).toBeVisible();
    await expect(page.getByText('Media no longer exists', { exact: true })).toBeVisible();
    await expect(page.getByText('Media source cannot be verified', { exact: true })).toBeVisible();
    expect(state.calls.filter(url => url.pathname.endsWith('/media/items')).every(url => !url.searchParams.get('ids')?.includes('legacy-id'))).toBe(true);
    await page.getByRole('button', { name: 'View media: The Signal' }).click();
    const drawer = page.getByRole('dialog', { name: 'Media details' });
    await expect(drawer.getByText('NO IMAGE', { exact: true })).toBeVisible();
    await expect(drawer).toContainText(episode.overview!);
    await page.keyboard.press('Escape');
    await expect(page.locator('tbody tr')).toHaveCount(3); expect(state.errors).toEqual([]);
  });

  test('a shared media link survives a reload without losing its underlying view', async ({ page }) => {
    const state = await fixture(page);
    await page.goto(`/admin/#/activity?tab=transfers&media=${episode.id}&media_source=${source}`);
    const drawer = page.getByRole('dialog', { name: 'Media details' });
    await expect(drawer).toContainText(episode.overview!);
    await page.reload();
    await expect(drawer).toContainText(episode.overview!);
    await drawer.getByRole('button', { name: 'Close media details' }).click();
    await expect(drawer).toBeHidden();
    await expect(page.getByRole('tab', { name: 'Transfers', exact: true })).toHaveAttribute('aria-selected', 'true');
    expect(page.url()).not.toContain('media='); expect(page.url()).toContain('tab=transfers');
    expect(state.errors).toEqual([]);
  });

  test('logout clears media cache before the next admin session loads its catalogue', async ({ page }) => {
    const state = await fixture(page);
    await page.goto('/admin/#/activity');
    await expect(page.getByRole('button', { name: 'View media: The Signal' })).toBeVisible();
    await page.getByRole('button', { name: 'Logout', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Sign In', exact: true })).toBeVisible();
    let release = () => {}; let requested = false;
    const ready = new Promise<void>(resolve => { release = resolve; });
    await page.route('**/admin/api/v1/media/items?*', async route => {
      requested = true; await ready;
      await route.fulfill({ json: { items: [{ ...episode, name: 'Fresh catalogue title' }] } });
    });
    await page.route('**/api/collections/_superusers/auth-with-password', route => route.fulfill({ json: { token: 'synthetic-login-token', record: { id: 'qa-admin', email: 'qa@example.test' } } }));
    try {
      await page.locator('#identity').fill('qa@example.test'); await page.locator('#password').fill('synthetic-password');
      await page.getByRole('button', { name: 'Sign In', exact: true }).click();
      await expect.poll(() => requested).toBe(true);
      await expect(page.getByRole('button', { name: 'View media: The Signal' })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'View media: Saved episode title' })).toBeVisible();
    } finally { release(); }
    await expect(page.getByRole('button', { name: 'View media: Fresh catalogue title' })).toBeVisible();
    expect(state.errors).toEqual([]);
  });

  test('idle Buffer defaults to 24h and preserves chart gaps and accessible sample inspection', async ({ page }) => {
    const state = await fixture(page, { aggregate: aggregate({ health: 'idle', active_requests: 0, observed_active_requests: 0 }), streams: [] });
    await page.goto('/admin/#/buffer');
    await expect(page.getByRole('tab', { name: '24h', exact: true })).toHaveAttribute('aria-selected', 'true');
    await expect(page.getByRole('heading', { name: 'History · 24h', exact: true })).toBeVisible();
    await expect(page.getByRole('figure')).toHaveCount(4);
    expect(state.calls.some(url => url.pathname.endsWith('/media-buffer/series') && url.searchParams.get('window') === '24h')).toBe(true);
    const chart = page.getByRole('figure', { name: 'Optional pool memory', exact: true });
    const path = await chart.locator('path').first().getAttribute('d');
    expect((path || '').match(/M/g)).toHaveLength(2);
    const slider = page.getByRole('slider', { name: 'Inspect Optional pool memory sample' });
    await slider.focus(); await page.keyboard.press('Home'); await page.keyboard.press('ArrowRight');
    await expect(chart.locator('figcaption')).toContainText('Not observed');
    await expect(page.getByText('Gaps mean no observation.', { exact: false })).toBeVisible();
    await screenshot(page, 'buffer-idle-history-desktop'); expect(state.errors).toEqual([]);
  });

  for (const scenario of [
    { name: 'upstream stall', overrides: { consumer_state: 'waiting_for_data', wait_condition: 'upstream_stall', wait_duration_ms: 12000, health: 'warning', health_reasons: ['upstream_stall'] } as Partial<BufferStream>, text: 'Upstream read stalled', node: '01 · Upstream', health: 'warning' },
    { name: 'normal full queue', overrides: { producer_state: 'waiting_for_buffer', allocation_blocker: 'at_target', wait_condition: 'buffer_acquire', wait_duration_ms: 12000 } as Partial<BufferStream>, text: 'At buffer limit · waiting for client to drain', node: '02 · Gateway queue', health: 'healthy' },
  ]) test(`Buffer pipeline explains ${scenario.name} using backend health`, async ({ page }) => {
    const state = await fixture(page, { streams: [stream('42', scenario.overrides)] });
    await page.goto('/admin/#/buffer?stream=boot-media-qa:42');
    const detail = page.getByRole('region', { name: 'Stream 42 detail' });
    await expect(detail).toBeVisible();
    const pipeline = detail.locator('.pipeline');
    await expect(pipeline).toContainText(scenario.text);
    await expect(pipeline.locator('.health')).toHaveText(scenario.health);
    await expect(pipeline.locator('.node').filter({ hasText: scenario.node })).toHaveClass(/waiting/);
    await expect(pipeline).toContainText('not video or client playback progress');
    await expect(pipeline.getByRole('img')).toHaveAttribute('aria-label', /queued; .* allowance reference; .* allocated/);
    expect(state.calls.some(url => url.pathname.endsWith('/streams/42') && url.searchParams.get('boot_id') === boot)).toBe(true);
    expect(state.errors).toEqual([]);
  });

  test('completion paging keeps its cursor and detail after the ten-second refresh', async ({ page }) => {
    await page.clock.install();
    const state = await fixture(page, { recentPages: true });
    await page.goto('/admin/#/buffer?tab=recent');
    await expect(page.getByText('viewer-newest', { exact: true })).toBeVisible();
    await expect(page.getByText(/Retained 2 \/ 2048 records/)).toBeVisible();
    await page.getByRole('button', { name: 'Next page', exact: true }).click();
    await expect(page.getByText('viewer-older', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Toggle completion detail', exact: true }).click();
    const detail = page.getByRole('region', { name: 'Completion stream-older detail' });
    await expect(detail).toBeVisible(); await expect(detail).toContainText('transfer-older');
    const before = state.calls.filter(url => url.pathname.endsWith('/media-buffer/recent') && url.searchParams.has('cursor'));
    await page.clock.fastForward(10_500);
    await expect.poll(() => state.calls.filter(url => url.pathname.endsWith('/media-buffer/recent') && url.searchParams.has('cursor')).length).toBeGreaterThan(before.length);
    await expect(detail).toBeVisible(); await expect(page.getByText('viewer-newest', { exact: true })).toHaveCount(0);
    const later = state.calls.filter(url => url.pathname.endsWith('/media-buffer/recent') && url.searchParams.has('cursor'));
    expect(later.every(url => url.searchParams.get('cursor') === 'completion-page-2')).toBe(true);
    expect(later.every(url => url.searchParams.get('from') === before[0].searchParams.get('from') && url.searchParams.get('to') === before[0].searchParams.get('to'))).toBe(true);
    expect(state.calls.some(url => url.pathname.endsWith('/recent/older') && url.searchParams.get('boot_id') === boot)).toBe(true);
    expect(state.errors).toEqual([]);
  });

  test('a later active-stream page stays selected through polling', async ({ page }) => {
    await page.clock.install();
    const state = await fixture(page, { streams: [stream('42')], nextStreams: [stream('43', { username: 'later-page-viewer' })] });
    await page.goto('/admin/#/buffer');
    await page.getByRole('button', { name: /Next page|Load more streams/ }).click();
    await expect(page.getByText('later-page-viewer', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Toggle detail for stream 43' }).click();
    await expect(page.getByRole('region', { name: 'Stream 43 detail' })).toBeVisible();
    await page.clock.fastForward(5_500);
    await expect.poll(() => state.calls.filter(url => url.pathname.endsWith('/media-buffer/streams') && url.searchParams.get('cursor') === 'stream-page-2').length).toBeGreaterThan(1);
    await expect(page.getByRole('region', { name: 'Stream 43 detail' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'First page', exact: true })).toBeVisible(); expect(state.errors).toEqual([]);
  });

  test('recorded errors include interrupted HTTP 206 and retain applied filters during pagination', async ({ page }) => {
    const state = await fixture(page);
    await page.goto('/admin/#/');
    await page.getByRole('button', { name: '24h', exact: true }).click();
    await expect(page.getByRole('link', { name: 'View recorded errors in the last 24h' })).toHaveAttribute('href', /window=24h/);
    await page.getByRole('link', { name: 'Errors · last 15m ↗', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Errors', exact: true })).toHaveAttribute('aria-pressed', 'true');
    await expect(page.getByRole('button', { name: '15m', exact: true })).toHaveAttribute('aria-pressed', 'true');
    await expect(page.getByText('Interrupted', { exact: true })).toBeVisible();
    await page.locator('.audit-filter-panel summary').click();
    await page.getByRole('combobox', { name: 'Direction', exact: true }).selectOption('upstream');
    await page.getByLabel('Error code', { exact: true }).fill('unexpected_eof');
    await page.getByLabel('User ID', { exact: true }).fill('user-a');
    await page.getByRole('button', { name: 'Apply filters' }).click();
    await expect.poll(() => state.calls.filter(url => url.pathname.endsWith('/audit')).at(-1)?.searchParams.get('error_kind')).toBe('unexpected_eof');
    const rowButton = page.getByRole('button', { name: /unexpected eof.*error-a/ });
    await rowButton.click();
    const details = page.getByRole('region', { name: 'Audit record details' });
    await expect(details).toContainText('The HTTP response had already started');
    await expect(details).toContainText('206 / 206'); await expect(details).toContainText('1.0 MiB');
    await page.getByRole('button', { name: 'Load more records' }).click();
    await expect(page.getByText('2 records loaded', { exact: true })).toBeVisible();
    const paged = state.calls.filter(url => url.pathname.endsWith('/audit') && url.searchParams.has('cursor')).at(-1)!;
    expect(paged.searchParams.get('direction')).toBe('upstream'); expect(paged.searchParams.get('error_kind')).toBe('unexpected_eof'); expect(paged.searchParams.get('user_id')).toBe('user-a');
    await screenshot(page, 'recorded-error-desktop');
    await page.keyboard.press('Escape'); await expect(details).toBeHidden(); await expect(rowButton).toBeFocused();
    expect(state.errors).toEqual([]);
  });

  test('user resume and favorites expose only the supplied local playback state', async ({ page }) => {
    const state = await fixture(page);
    await page.goto('/admin/#/users');
    await page.getByRole('button', { name: 'View media for alice' }).click();
    const panel = page.getByRole('region', { name: 'Media for alice' });
    await expect(panel).toBeVisible();
    await panel.getByRole('button', { name: 'Continue watching', exact: true }).click();
    await expect.poll(() => state.calls.filter(url => url.pathname.endsWith('/users/user-a/media')).at(-1)?.searchParams.get('view')).toBe('resume');
    await expect(panel.getByRole('progressbar')).toHaveAttribute('value', /33\.33/);
    await panel.getByRole('button', { name: 'Favorites', exact: true }).click();
    await expect.poll(() => state.calls.filter(url => url.pathname.endsWith('/users/user-a/media')).at(-1)?.searchParams.get('view')).toBe('favorites');
    await panel.getByRole('button', { name: 'View media: The Signal' }).click();
    const drawer = page.getByRole('dialog', { name: 'Media details' });
    await expect(drawer.getByRole('heading', { name: 'Gateway user state' })).toBeVisible();
    await expect(drawer).toContainText('15:00 / 45:00');
    await expect(drawer.locator('.facts div').filter({ hasText: 'Favorite' })).toContainText('Yes');
    await expect(drawer.locator('.facts div').filter({ hasText: 'Watched' })).toContainText('No');
    await page.keyboard.press('Escape');
    await expect(panel.getByRole('button', { name: 'Favorites', exact: true })).toHaveAttribute('aria-pressed', 'true');
    expect(state.errors).toEqual([]);
  });

  test('390px layout keeps Buffer, media dialog and error details within the viewport', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    const state = await fixture(page, { streams: [stream('42', { health: 'warning', health_reasons: ['upstream_stall'], wait_condition: 'upstream_stall', wait_duration_ms: 12000 })] });
    await page.goto('/admin/#/buffer?stream=boot-media-qa:42');
    await expect(page.getByRole('region', { name: 'Stream 42 detail' })).toBeVisible(); await noOverflow(page);
    const axisSize = await page.getByRole('figure', { name: 'Optional pool memory', exact: true }).locator('svg').evaluate(svg => {
      const text = svg.querySelector('text')!;
      return parseFloat(getComputedStyle(text).fontSize) * svg.getBoundingClientRect().width / (svg as SVGSVGElement).viewBox.baseVal.width;
    });
    expect(axisSize, 'chart labels must remain readable after SVG scaling').toBeGreaterThanOrEqual(10.9);
    // Metadata is intentionally lazy; bring the actual row into view first.
    await page.locator('.streams-table .media-cell').first().scrollIntoViewIfNeeded();
    const media = page.getByRole('button', { name: 'View media: The Signal' });
    await media.click();
    const drawer = page.getByRole('dialog', { name: 'Media details' });
    await expect(drawer).toBeVisible();
    const bounds = await drawer.boundingBox(); expect(bounds!.width).toBeLessThanOrEqual(390); expect(bounds!.x).toBeGreaterThanOrEqual(0);
    await expect(drawer.getByRole('button', { name: 'Close media details' })).toBeInViewport();
    await screenshot(page, 'media-details-mobile');
    await page.keyboard.press('Escape'); await expect(drawer).toBeHidden(); await expect(media).toBeFocused(); await noOverflow(page);
    await page.goto('/admin/#/traffic?view=errors');
    await page.getByRole('button', { name: /unexpected eof.*error-a/ }).click();
    await expect(page.getByRole('region', { name: 'Audit record details' })).toBeVisible(); await noOverflow(page);
    expect(state.errors).toEqual([]);
  });
});
