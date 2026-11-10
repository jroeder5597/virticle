import { defineConfig, devices } from "@playwright/test";
import { authFile, env } from "./lib/env";

export default defineConfig({
  testDir: "./tests",
  workers: 1, // the lab has room for a handful of tiny VMs, so everything runs serially
  fullyParallel: false,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [["list"], ["html", { open: "never" }]],
  globalSetup: "./global-setup.ts",
  globalTeardown: "./global-teardown.ts",
  use: {
    ...devices["Desktop Chrome"],
    baseURL: env.base,
    storageState: authFile,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    actionTimeout: 15_000,
    viewport: { width: 1360, height: 860 },
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
