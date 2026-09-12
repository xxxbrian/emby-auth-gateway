// Explicit diagnostic control: the same original player, fixture and rapid
// seek sequence run with optional subtitle recovery enabled and disabled.
import { test, expect } from '@playwright/test';
import fs from 'node:fs/promises';

const base = process.env.SUBTITLE_E2E_BASE || 'http://127.0.0.1:18094';
type ProbeWindow = Window & { __subtitleRapidErrors?: string[] };

test.afterEach(async ({ page }, testInfo) => {
  const state = await page.evaluate(() => ({ errors: (window as ProbeWindow).__subtitleRapidErrors || [], videos: [...document.querySelectorAll('video')].map(v => ({ currentTime:v.currentTime,readyState:v.readyState,error:v.error?.code,paused:v.paused })), dialog:[...document.querySelectorAll('h3')].some(h=>h.textContent?.trim()==='Playback Error') }));
  await fs.writeFile(testInfo.outputPath('final-state.json'), JSON.stringify(state, null, 2));
});

test('rapid seeks preserve playback with optional subtitles toggled', async ({ page }, testInfo) => {
  const errors: string[] = [];
  page.on('response', response => {
    const path = new URL(response.url()).pathname;
    if ((path.includes('/audio/eag-') || path.includes('/gateway-subtitles/')) && response.status() >= 400) errors.push(`${response.status()} ${path}`);
  });
  await page.addInitScript(() => {
    const probe = window as ProbeWindow;
    probe.__subtitleRapidErrors = [];
    document.addEventListener('error', event => {
      if (event.target instanceof HTMLVideoElement) probe.__subtitleRapidErrors?.push(`video:${event.target.error?.code}:${event.target.error?.message}`);
    }, true);
    new MutationObserver(() => {
      if ([...document.querySelectorAll('h3')].some(h => h.textContent?.trim() === 'Playback Error')) {
        if (!probe.__subtitleRapidErrors?.includes('dialog')) probe.__subtitleRapidErrors?.push('dialog');
      }
    }).observe(document, { childList: true, subtree: true });
  });
  await page.goto(`${base}/emby/web/`);
  await page.getByText('subtitle-one', { exact: true }).click();
  await page.locator('input[type=password]').fill('viewer-password');
  await page.getByRole('button', { name: 'Sign In', exact: true }).click();
  await page.waitForURL(/#!\/home/);
  await page.goto(`${base}/emby/web/#!/item?id=fixture&serverId=emby-auth-gateway`);
  await expect(page.getByRole('heading', { name: 'Subtitle compatibility fixture', exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'Play', exact: true }).click();
  await page.waitForFunction(() => {
    const video = document.querySelector('video');
    return video && !video.error && video.readyState >= 3 && video.currentTime > .5;
  });
  const rounds = await page.locator('video').evaluate(async element => {
    const video = element as HTMLVideoElement;
    video.pause();
    const results: { currentTime: number; readyState: number; error: number }[] = [];
    for (let round = 0; round < 8; round++) {
      for (const target of [2, 22, 2]) {
        video.currentTime = target;
        await new Promise<void>(resolve => requestAnimationFrame(() => resolve()));
      }
      await new Promise(resolve => setTimeout(resolve, 200));
      results.push({ currentTime: video.currentTime, readyState: video.readyState, error: video.error?.code || 0 });
    }
    return results;
  });
  await page.waitForFunction(() => {
    const video = document.querySelector('video');
    return video && Math.abs(video.currentTime - 2) < 1 && video.readyState >= 3;
  });
  await page.locator('video').evaluate(async element => {
    const video = element as HTMLVideoElement;
    await video.play();
  });
  await page.waitForFunction(() => (document.querySelector('video')?.currentTime || 0) > 3);
  const result = { enabled: process.env.SUBTITLE_E2E_FEATURE_ENABLED !== 'false', rounds, errors, browserErrors: await page.evaluate(() => (window as ProbeWindow).__subtitleRapidErrors || []) };
  await fs.writeFile(testInfo.outputPath('rapid-result.json'), JSON.stringify(result, null, 2));
  await page.screenshot({ path: testInfo.outputPath('rapid-seek.png') });
  expect(result.errors).toEqual([]);
  expect(result.browserErrors).toEqual([]);
});
