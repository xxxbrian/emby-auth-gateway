import { defineConfig, devices } from '@playwright/test';
import os from 'node:os';
import path from 'node:path';

export default defineConfig({
  testDir: './audio-e2e',
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 20_000 },
  reporter: [['list']],
  outputDir: process.env.AUDIO_E2E_ARTIFACTS || path.join(os.tmpdir(), 'eag-audio-browser-results'),
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
