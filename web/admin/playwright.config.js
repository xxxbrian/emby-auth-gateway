import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env.ADMIN_E2E_BASE || 'http://127.0.0.1:18090';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: [['list']],
  outputDir: 'test-results',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL,
    ...devices['Desktop Chrome'],
    screenshot: 'only-on-failure',
    trace: 'off',
    video: 'off',
  },
  projects: [
    {
      name: 'chromium',
      use: { browserName: 'chromium' },
    },
  ],
});
