import { test, expect, type Page } from '@playwright/test';
import fs from 'node:fs/promises';
import type { TranscodingPage } from '../src/lib/types';

const base = process.env.AUDIO_E2E_BASE || 'http://127.0.0.1:18093';
type MediaVideo = HTMLVideoElement & { webkitAudioDecodedByteCount?: number };

async function loginViewer(page: Page, name: string) {
  await page.goto(`${base}/emby/web/`);
  await page.getByText(name, { exact: true }).click();
  await page.locator('input[type=password]').fill('viewer-password');
  await page.getByRole('button', { name: 'Sign In', exact: true }).click();
  await page.waitForURL(/#!\/home/);
  await page.goto(`${base}/emby/web/#!/item?id=fixture&serverId=emby-auth-gateway`);
  await expect(page.getByRole('heading', { name: 'Audio compatibility fixture', exact: true })).toBeVisible();
}

async function start(page: Page) {
  const response = page.waitForResponse(r => {
    const u = new URL(r.url());
    return u.pathname.endsWith('/PlaybackInfo') && u.searchParams.get('IsPlayback') === 'true';
  });
  const beginning = page.getByText('From Beginning', { exact: true });
  if (await beginning.isVisible()) await beginning.click();
  else await page.getByRole('button', { name: 'Play', exact: true }).click();
  const result = await response;
  expect(result.status()).toBe(200);
  const info = await result.json() as { PlaySessionId: string };
  expect(info.PlaySessionId).toMatch(/^eag-[a-f0-9]{32}$/);
  await page.waitForFunction(() => {
    const v = document.querySelector('video') as MediaVideo | null;
    return v && !v.error && v.readyState >= 3 && v.currentTime > .5 && (v.webkitAudioDecodedByteCount || 0) > 0;
  });
  return info.PlaySessionId;
}

async function seek(page: Page, position: number) {
  const before = await page.locator('video').evaluate(v => (v as MediaVideo).webkitAudioDecodedByteCount || 0);
  // Emby Web's native HLS seek path sets this same media-element property.
  await page.locator('video').evaluate((v, t) => { (v as HTMLVideoElement).currentTime = t; }, position);
  await page.waitForFunction(({ position, before }) => {
    const v = document.querySelector('video') as MediaVideo | null;
    return v && !v.error && v.readyState >= 3 && v.currentTime > position + .5 && v.currentTime < position + 6 && (v.webkitAudioDecodedByteCount || 0) > before;
  }, { position, before });
}

async function pause(page: Page, paused: boolean) {
  await page.mouse.move(600,500);
  const isPaused=await page.locator('video').evaluate(v=>(v as HTMLVideoElement).paused);
  if(isPaused!==paused)await page.getByRole('button',{name:paused?'Pause':'Play',exact:true}).click();
  await page.waitForFunction(paused=>document.querySelector('video')?.paused===paused,paused,{timeout:15000});
}

test.afterEach(async ({browser},info)=>{
  if(info.status===info.expectedStatus)return;
  const context=await browser.newContext();
  try {
    const page=await context.newPage();await page.goto(`${base}/admin/`);
    await page.locator('#identity').fill('admin@test.local');await page.locator('#password').fill('adminpass123');
    await page.getByRole('button',{name:'Sign In',exact:true}).click();
    await page.getByRole('heading',{name:'Overview',exact:true}).waitFor();
    for(const endpoint of ['jobs','recent']){
      const response=await context.request.get(`${base}/admin/api/v1/transcoding/${endpoint}`);
      if(response.ok())await fs.writeFile(info.outputPath(`${endpoint}.json`),JSON.stringify(await response.json(),null,2));
    }
  } finally {await context.close();}
});

test('original Emby Web converts audio, seeks, switches tracks, and exposes isolated Admin tasks', async ({ page, browser }, testInfo) => {
  const mediaFailures: string[] = [];
  page.on('response', r => {
    const u = new URL(r.url());
    if (u.pathname.includes('/audio/eag-') && r.status() >= 400) mediaFailures.push(`${r.status()} ${u.pathname}`);
  });
  await loginViewer(page, 'audio-one');
  const initialID = await start(page);
  await pause(page,true);
  await page.locator('video').evaluate(v => { (v as HTMLVideoElement).currentTime = 250; });
  await page.waitForFunction(() => { const v = document.querySelector('video'); return v && v.paused && Math.abs(v.currentTime - 250) < 1 && v.readyState >= 3; });
  await pause(page,false);
  await seek(page, 10);
  await seek(page, 300);
  await seek(page, 30);
  await page.screenshot({ path: testInfo.outputPath('native-audio-playback.png') });

  await pause(page,true);
  await page.mouse.move(600, 600);
  await page.getByRole('button', { name: 'Audio', exact: true }).click();
  const oldSource = await page.locator('video').evaluate(v => (v as HTMLVideoElement).currentSrc);
  const switchedResponse = page.waitForResponse(r => {
    const u = new URL(r.url());
    return u.pathname.endsWith('/PlaybackInfo') && u.searchParams.get('IsPlayback') === 'true' && u.searchParams.get('AudioStreamIndex') === '2';
  });
  await page.getByRole('button', { name: 'English AAC 2 ch', exact: true }).click();
  const switched = await switchedResponse;
  expect(switched.status()).toBe(200);
  const switchedID = ((await switched.json()) as { PlaySessionId: string }).PlaySessionId;
  expect(switchedID).not.toBe(initialID);
  await page.waitForFunction(oldSource => {
    const v = document.querySelector('video') as MediaVideo | null;
    return v && v.currentSrc !== oldSource && !v.error && v.readyState >= 3 && v.currentTime >= 29 && v.currentTime < 45 && (v.webkitAudioDecodedByteCount || 0) > 0;
  }, oldSource);
  await page.locator('video').evaluate(v => (v as HTMLVideoElement).pause());

  const adminContext = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const admin = await adminContext.newPage();
  await admin.goto(`${base}/admin/`);
  await admin.locator('#identity').fill('admin@test.local');
  await admin.locator('#password').fill('adminpass123');
  await admin.getByRole('button', { name: 'Sign In', exact: true }).click();
  await expect(admin.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  const jobs = async () => {
    const response = await adminContext.request.get(`${base}/admin/api/v1/transcoding/jobs?limit=200`);
    expect(response.status()).toBe(200);
    return response.json() as Promise<TranscodingPage>;
  };
  await expect.poll(async () => {
    const result = await jobs();
    return result.items.some(j => j.id === switchedID && j.audio_source === 'aac') && !result.items.some(j => j.id === initialID);
  }, { intervals: [500, 1000] }).toBe(true);
  const current = await jobs();
  expect(current.items.find(j => j.id === switchedID)?.playback_id).toBe(initialID);
  await admin.goto(`${base}/admin/#/transcoding?job=${switchedID}&boot=${current.boot_id}`);
  await expect(admin.getByRole('region', { name: 'Conversion task details' })).toBeVisible();
  await expect(admin.getByText('Generation progress, not watched progress', { exact: true })).toBeVisible();
  await admin.screenshot({ path: testInfo.outputPath('admin-transcoding-desktop.png'), fullPage: true });
  await admin.setViewportSize({ width: 390, height: 844 });
  await expect.poll(() => admin.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await admin.screenshot({ path: testInfo.outputPath('admin-transcoding-mobile.png'), fullPage: true });
  await admin.getByRole('region',{name:'Conversion task details'}).scrollIntoViewIfNeeded();
  await admin.screenshot({path:testInfo.outputPath('admin-transcoding-mobile-details.png')});

  const peers = await Promise.all(['audio-two', 'audio-three'].map(async name => {
    const context = await browser.newContext();
    const p = await context.newPage();
    await loginViewer(p, name);
    const id = await start(p);
    return { context, page: p, id, name };
  }));
  await Promise.all(peers.map((p, i) => seek(p.page, 100 + i * 100)));
  const pausedTime = await page.locator('video').evaluate(v => (v as HTMLVideoElement).currentTime);
  await expect.poll(async () => (await jobs()).items.filter(j => j.id === switchedID || peers.some(p => p.id === j.id)).length).toBe(3);
  expect(await page.locator('video').evaluate(v => (v as HTMLVideoElement).paused)).toBe(true);
  expect(await page.locator('video').evaluate(v => (v as HTMLVideoElement).currentTime)).toBeCloseTo(pausedTime, 1);

  await page.mouse.move(600, 600);
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await expect.poll(async () => (await jobs()).items.some(j => j.id === switchedID)).toBe(false);
  await seek(peers[0].page, 45);
  await pause(peers[1].page,true);
  // Run through the entire fractional-frame-rate fixture, crossing every
  // segment boundary and cache refill without changing video encoding.
  const completed=await peers[0].page.locator('video').evaluate(async element=>{
    const video=element as MediaVideo;
    const ended=new Promise<boolean>((resolve,reject)=>{
      video.addEventListener('ended',()=>resolve(true),{once:true});
      video.addEventListener('error',()=>reject(new Error(video.error?.message||'media error')),{once:true});
    });
    video.currentTime=0;video.playbackRate=8;await video.play();return ended;
  });
  expect(completed).toBe(true);
  expect(mediaFailures).toEqual([]);
  for (const peer of peers) await peer.context.close();
  await adminContext.close();
});
