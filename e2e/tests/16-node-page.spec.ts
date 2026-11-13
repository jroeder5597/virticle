// Node page: opened from the Nodes list, every tab renders, host interfaces and the default pool are shown.
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";

const TABS: [string, string][] = [
  ["Summary", "summary"],
  ["Virtual machines", "vms"],
  ["Storage", "storage"],
  ["Network", "network"],
  ["Topology", "topology"],
  ["Updates", "updates"],
  ["Settings", "settings"],
];

test.describe.serial("node page", () => {
  test("opens from the node list and every tab renders without an error", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("tbody tr").filter({ hasText: env.node1 }).getByRole("link", { name: env.node1 }).click();
    await expect(page).toHaveURL(/\/node\/\d+$/);
    await expect(page.getByRole("heading", { name: env.node1, level: 1 })).toBeVisible();
    await expect(page.locator(".nodetitle .badge", { hasText: "online" })).toBeVisible();
    for (const [label, slug] of TABS) {
      await page.getByRole("tab", { name: label, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`/node/\\d+/${slug}$`));
      await expect(page.getByRole("tab", { name: label, exact: true })).toHaveAttribute("aria-selected", "true");
      await page.waitForTimeout(1500);
      await expect(page.locator(".error")).toHaveCount(0);
    }
  });

  test("summary shows host facts", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    await expect(page.locator(".card", { hasText: "Host" }).first()).toContainText(/\d+ ×/);
    await expect(page.locator(".card", { hasText: "Inventory" })).toContainText(/\d+ of \d+/);
  });

  test("storage tab shows the default pool, network tab lists host interfaces", async ({ page }) => {
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    await page.getByRole("tab", { name: "Storage", exact: true }).click();
    await expect(page.locator("tbody tr").filter({ hasText: "default" }).first()).toBeVisible();
    await page.getByRole("tab", { name: "Network", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Host interfaces" })).toBeVisible();
    await expect(page.locator(".nicbox tbody tr").first()).toBeVisible();
    expect(await page.locator(".nicbox tbody tr").count()).toBeGreaterThan(0);
  });

  test("an unknown node shows the empty state", async ({ page }) => {
    await page.goto("/node/99999");
    await expect(page.getByText("There is nothing at this address")).toBeVisible();
  });

  test("no sideways scroll at 390px on any tab", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/nodes");
    await page.locator("main").getByRole("link", { name: env.node1 }).click();
    for (const [label] of TABS) {
      await page.getByRole("tab", { name: label, exact: true }).click();
      await page.waitForTimeout(1200);
      const wide = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      expect(wide, `${label} overflows`).toBeLessThanOrEqual(0);
    }
  });
});
