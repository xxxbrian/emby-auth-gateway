import { test, expect, type Page, type APIRequestContext, type Response } from '@playwright/test';
import fs from 'node:fs/promises';

const diagnostics = new WeakMap<Page, string[]>();

const base = process.env.SUBTITLE_E2E_BASE || 'http://127.0.0.1:18094';
const upstream = process.env.SUBTITLE_E2E_UPSTREAM || 'http://127.0.0.1:18096/emby';
type Stream = { Index: number; Type: string; Codec: string; DeliveryUrl?: string; DisplayTitle?: string };
type PlaybackInfo = { PlaySessionId: string; MediaSources: { MediaStreams: Stream[]; DefaultSubtitleStreamIndex?: number }[] };
type Snapshot = { enabled: boolean; active: number; jobs: { id: string; tracks: { index: number; state: string; reason?: string }[] }[] };

async function stats(request: APIRequestContext) {
  const response = await request.get(`${upstream}/eag-fixture/stats`);
  expect(response.status()).toBe(200);
  return response.json() as Promise<{ rawRequests: number; rawBytes: number; subtitleRequests: number }>;
}

async function snapshot(request: APIRequestContext) {
  const response = await request.get(`${base}/admin/api/v1/subtitles`);
  expect(response.status()).toBe(200);
  return response.json() as Promise<Snapshot>;
}

async function loginAdmin(page: Page) {
  await page.goto(`${base}/admin/`);
  await page.locator('#identity').fill('admin@test.local');
  await page.locator('#password').fill('adminpass123');
  await page.getByRole('button', { name: 'Sign In', exact: true }).click();
  await expect(page.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
}

async function loginViewer(page: Page) {
  await page.bringToFront();
  await page.goto(`${base}/emby/web/`);
  await page.getByText('subtitle-one', { exact: true }).click();
  await page.locator('input[type=password]').fill('viewer-password');
  await page.getByRole('button', { name: 'Sign In', exact: true }).click();
  await page.waitForURL(/#!\/home/);
  await page.goto(`${base}/emby/web/#!/item?id=fixture&serverId=emby-auth-gateway`);
  await expect(page.getByRole('heading', { name: 'Subtitle compatibility fixture', exact: true })).toBeVisible();
}

async function play(page: Page) {
  await page.bringToFront();
  const response = page.waitForResponse(r => new URL(r.url()).pathname.endsWith('/PlaybackInfo') && new URL(r.url()).searchParams.get('IsPlayback') === 'true');
  const started = page.waitForResponse(r => r.request().method() === 'POST' && new URL(r.url()).pathname.endsWith('/Sessions/Playing'));
  const beginning = page.getByText('From Beginning', { exact: true });
  if (await beginning.isVisible()) await beginning.click();
  else await page.getByRole('button', { name: 'Play', exact: true }).click();
  const result = await response;
  expect(result.status()).toBe(200);
  // A media element can advance before the vendor player's initial play
  // promise settles. Wait for its own playback-start report before acting.
  expect((await started).status()).toBe(204);
  await page.waitForFunction(() => {
    const video = document.querySelector('video');
    return video && !video.error && !video.paused && !video.seeking && video.readyState >= 3 && video.currentTime > .5;
  });
  await expect(page.getByRole('heading', { name: 'Playback Error', exact: true })).toBeHidden();
  return result;
}

async function cueAt(page: Page, position: number, expected: string) {
  // Pause through the player controls so its state and the element agree.
  await page.mouse.move(600, 500);
  if (!(await page.locator('video').evaluate(v => (v as HTMLVideoElement).paused))) {
    await page.getByRole('button', { name: 'Pause', exact: true }).click();
  }
  await page.waitForFunction(() => document.querySelector('video')?.paused === true);
  await page.locator('video').evaluate((element, position) => {
    const video = element as HTMLVideoElement;
    video.currentTime = position;
  }, position);
  // Original Emby Web paints WebVTT into its subtitle overlay rather than a
  // native showing TextTrack on Chromium. Verify the visible rendered cue.
  await expect(page.getByText(expected, { exact: true })).toBeVisible();
  await page.waitForFunction(position => {
    const video = document.querySelector('video');
    return video && Math.abs(video.currentTime-position)<1 && video.readyState>=3;
  }, position);
  await expect.poll(() => page.locator('video').evaluate(v => (v as HTMLVideoElement).error?.code || 0)).toBe(0);
  await expect(page.getByRole('heading', { name: 'Playback Error', exact: true })).toBeHidden();
}

async function prepareLanguage(page: Page, observed: Response, index: number) {
  const request = observed.request();
  // Replay the same authenticated protocol request with a deliberate language
  // selection. This exercises the server; the vendor player remains unchanged.
  const wire = { url: request.url(), headers: await request.allHeaders(), body: request.postData() || '{}', index };
  return page.evaluate(async wire => {
    const url = new URL(wire.url);
    url.searchParams.set('SubtitleStreamIndex', String(wire.index));
    url.searchParams.set('IsPlayback', 'true');
    const headers: Record<string, string> = {};
    for (const [key, value] of Object.entries(wire.headers)) {
      if (['x-emby-authorization', 'x-emby-token', 'content-type', 'x-mediabrowser-token'].includes(key.toLowerCase())) headers[key] = value;
    }
    const body = JSON.parse(wire.body) as Record<string, unknown>;
    body.SubtitleStreamIndex = wire.index;
    body.IsPlayback = true;
    const response = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body), credentials: 'same-origin' });
    if (!response.ok) throw new Error(`subtitle selection status ${response.status}`);
    return response.json();
  }, wire) as Promise<PlaybackInfo>;
}

test.afterEach(async ({ page, browser }, testInfo) => {
  if (testInfo.status === testInfo.expectedStatus) return;
  await fs.writeFile(testInfo.outputPath('browser-events.json'), JSON.stringify(diagnostics.get(page) || [], null, 2));
  await fs.writeFile(testInfo.outputPath('browser-media.json'), JSON.stringify(await page.evaluate(() => ({
    text: document.body.innerText,
    media: [...document.querySelectorAll('video')].map(v => ({ error:v.error?.message,currentTime:v.currentTime,readyState:v.readyState,paused:v.paused,seeking:v.seeking,networkState:v.networkState }))
  })), null, 2));
  const context = await browser.newContext();
  try {
    const admin = await context.newPage();
    await loginAdmin(admin);
    for (const path of ['subtitles', 'transcoding/jobs', 'transcoding/recent']) {
      const response = await context.request.get(`${base}/admin/api/v1/${path}`);
      if (response.ok()) await fs.writeFile(testInfo.outputPath(`${path.replace('/', '-')}.json`), await response.body());
    }
  } finally { await context.close(); }
});

test('native playback stays untouched; original Web recovers, displays, seeks and switches text subtitles', async ({ page, browser, playwright }, testInfo) => {
  const adminContext = await browser.newContext();
  const admin = await adminContext.newPage();
  await loginAdmin(admin);
  expect((await snapshot(adminContext.request)).enabled).toBe(true);
  const native = await playwright.request.newContext({ extraHTTPHeaders: { 'X-Emby-Authorization': 'Emby Client="Emby Theater", Device="desktop", DeviceId="subtitle-native-test", Version="1"' } });
  const authResponse = await native.post(`${base}/emby/Users/AuthenticateByName`, { data: { Username: 'subtitle-one', Pw: 'viewer-password' } });
  expect(authResponse.status()).toBe(200);
  const auth = await authResponse.json() as { AccessToken: string; User: { Id: string } };
  const headers = { 'X-Emby-Token': auth.AccessToken };
  const beforeNative = await stats(native);
  for (const isPlayback of [false, true]) {
    const response = await native.post(`${base}/emby/Items/fixture/PlaybackInfo?IsPlayback=${isPlayback}&UserId=${auth.User.Id}`, {
      headers, data: { IsPlayback: isPlayback, EnableDirectPlay: true, EnableDirectStream: true, EnableTranscoding: false,
        DeviceProfile: { DirectPlayProfiles: [{ Type: 'Video', Container: 'mkv', VideoCodec: 'h264', AudioCodec: 'eac3,aac' }] } },
    });
    expect(response.status()).toBe(200);
    const info = await response.json() as PlaybackInfo;
    expect(info.MediaSources[0].MediaStreams.filter(s => s.Type === 'Subtitle').map(s => s.Index)).toEqual([3, 4, 5]);
    expect(info.MediaSources[0].MediaStreams.every(s => !s.DeliveryUrl?.includes('gateway-subtitles'))).toBe(true);
  }
  expect(await stats(native)).toEqual(beforeNative);
  expect((await snapshot(adminContext.request)).jobs).toHaveLength(0);
  const nativeMedia = await native.get(`${base}/emby/Videos/fixture/original.mkv?MediaSourceId=fixture-source&Static=true`, { headers: { ...headers, Range: 'bytes=0-1023' } });
  expect(nativeMedia.status()).toBe(206);
  expect((await nativeMedia.body()).length).toBe(1024);
  const afterNative = await stats(native);
  expect(afterNative.rawRequests).toBe(beforeNative.rawRequests + 1);
  expect(afterNative.subtitleRequests).toBe(beforeNative.subtitleRequests);
  expect((await snapshot(adminContext.request)).jobs).toHaveLength(0);

  const mediaFailures: string[] = [];
  const events: string[] = [];
  diagnostics.set(page, events);
  page.on('console', message => {
    if(events.length>=1000)events.shift();
    events.push(message.text().replace(/(?:https?|wss?):\/\/[^\s]+/g, '[url]').slice(0, 700));
  });
  page.on('response', async response => {
    const path = new URL(response.url()).pathname;
    if ((path.includes('/gateway-subtitles/') || path.includes('/audio/eag-')) && response.status() >= 400) { mediaFailures.push(`${response.status()} ${path}`); events.push(`${response.status()} ${path}`); }
    if(path.endsWith('/PlaybackInfo')){
      try {
        const info=await response.json() as PlaybackInfo;
        const u=new URL(response.url());
        events.push(JSON.stringify({playbackInfo:response.status(),isPlayback:u.searchParams.get('IsPlayback'),subtitle:u.searchParams.get('SubtitleStreamIndex'),info:JSON.parse(JSON.stringify(info).replace(/https?:[^"\s]+/g,'[url]').replace(/api_key=[^&"\s]+/g,'api_key=[redacted]'))}));
      }catch{/* An aborted diagnostic response must not affect playback. */}
    }
  });
  await loginViewer(page);
  await expect.poll(async () => (await snapshot(adminContext.request)).jobs.some(job => job.tracks.some(track => track.index === 3 && track.state === 'ready'))).toBe(true);
  await page.reload();
  await expect(page.getByRole('heading', { name: 'Subtitle compatibility fixture', exact: true })).toBeVisible();
  const observed = await play(page);
  const firstInfo = await observed.json() as PlaybackInfo;
  expect(firstInfo.MediaSources[0].MediaStreams.some(s => s.Index === 3 && s.DeliveryUrl?.includes('/gateway-subtitles/'))).toBe(true);
  await cueAt(page, 2, '第一句中文字幕');
  await cueAt(page, 22, '后面的中文字幕');
  await page.screenshot({ path: testInfo.outputPath('web-recovered-chinese.png') });

  await prepareLanguage(page, observed, 4);
  await expect.poll(async () => (await snapshot(adminContext.request)).jobs.some(job => job.tracks.some(track => track.index === 4 && track.state === 'ready'))).toBe(true);
  await page.goto(`${base}/emby/web/#!/item?id=fixture&serverId=emby-auth-gateway`);
  await expect(page.getByRole('heading', { name: 'Subtitle compatibility fixture', exact: true })).toBeVisible();
  await play(page);
  await page.mouse.move(600, 500);
  await page.getByRole('button', { name: 'Subtitles', exact: true }).click();
  await page.getByRole('button', { name: /English \(SUBRIP\)$/ }).click();
  await cueAt(page, 2, 'First English subtitle');
  await cueAt(page, 22, 'Later English subtitle');
  await page.screenshot({ path: testInfo.outputPath('web-recovered-english.png') });
  expect(mediaFailures).toEqual([]);

  await admin.bringToFront();
  await admin.goto(`${base}/admin/#/subtitles`);
  await expect(admin.getByRole('heading', { name: 'Web subtitles', exact: true })).toBeVisible();
  await admin.getByRole('button', { name: 'Subtitle details for Subtitle compatibility sample', exact: true }).click();
  const details = admin.getByRole('region', { name: 'Subtitle track details', exact: true });
  await expect(details).toBeVisible();
  await expect(details.getByText('Chinese Simplified (SUBRIP)', { exact: true })).toBeVisible();
  await expect(details.getByText('English (SUBRIP)', { exact: true })).toBeVisible();
  await admin.screenshot({ path: testInfo.outputPath('admin-web-subtitles-desktop.png'), fullPage: true });
  await admin.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => admin.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await admin.screenshot({ path: testInfo.outputPath('admin-web-subtitles-mobile.png'), fullPage: true });

  // Re-check the independent native identity after the same account's Web
  // recovery is ready: list projection and permissions must stay native.
  const nativeAfter = await native.post(`${base}/emby/Items/fixture/PlaybackInfo?IsPlayback=false&UserId=${auth.User.Id}`, { headers, data: { IsPlayback: false } });
  const nativeInfo = await nativeAfter.json() as PlaybackInfo;
  expect(nativeInfo.MediaSources[0].MediaStreams.filter(s => s.Type === 'Subtitle').map(s => s.Index)).toEqual([3, 4, 5]);
  expect(nativeInfo.MediaSources[0].MediaStreams.every(s => !s.DeliveryUrl?.includes('gateway-subtitles'))).toBe(true);
  await page.bringToFront();
  await page.mouse.move(600, 500);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect.poll(async () => (await snapshot(adminContext.request)).active).toBe(0);
  await native.dispose();
  await adminContext.close();
});
