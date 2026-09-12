import { defineConfig, devices } from '@playwright/test';
import os from 'node:os';
import path from 'node:path';

export default defineConfig({
  testDir: './subtitle-e2e',
  testMatch: process.env.SUBTITLE_E2E_MODE === 'rapid' ? '**/rapid.spec.ts' : '**/subtitles.spec.ts',
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 20_000 },
  reporter: [['list']],
  outputDir: process.env.SUBTITLE_E2E_ARTIFACTS || path.join(os.tmpdir(), 'eag-subtitle-browser-results'),
  use: {
    ...devices['Desktop Chrome'],
    browserName: 'chromium',
    headless: true,
    launchOptions: { args: ['--autoplay-policy=no-user-gesture-required'] },
    screenshot: 'only-on-failure',
    actionTimeout: 20_000,
    navigationTimeout: 30_000,
    trace: 'off',
  },
});
