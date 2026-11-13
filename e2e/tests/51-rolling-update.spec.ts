// Rolling node updates: the plan dialog and its refusals. The run itself changes the nodes (package updates, restarts),
// so it is not started here; it is tested through the API in the lab (see docs/reference.md, Rolling updates).
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";
import { dialog } from "../lib/ui";

test.describe.serial("rolling update", () => {
  test.afterAll(async ({ api, n1, n2 }) => {
    for (const n of [n1, n2]) await api.put(`/nodes/${n}/maintenance`, { on: false }).catch(() => {});
  });

  test("Update all nodes shows the plan of both nodes and starts nothing", async ({ page }) => {
    await page.goto("/datacenter/nodes");
    await page.getByRole("button", { name: "Update all nodes" }).click();
    const dlg = dialog(page, "Update all nodes");
    await expect(dlg.getByText("The nodes are updated one at a time")).toBeVisible();
    const table = dlg.locator("table.rollplan");
    await expect(table).toBeVisible({ timeout: 120_000 });
    for (const name of [env.node1, env.node2]) await expect(table.getByRole("row", { name: new RegExp(name) })).toBeVisible();
    for (const h of ["Node", "Updates", "Restart", "Virtual machines"]) await expect(table.getByRole("columnheader", { name: h })).toBeVisible();
    // nothing is started by looking: Start update is a separate button, and is off when there is nothing to do or a choice is open
    await expect(dlg.getByRole("button", { name: "Start update" })).toBeVisible();
    const fits = await dlg.evaluate((e) => e.scrollWidth <= e.clientWidth + 1);
    expect(fits, "the dialog fits without scrolling sideways").toBe(true);
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("a node in maintenance refuses the update with the reason and the way out", async ({ page, api, n2 }) => {
    await api.put(`/nodes/${n2}/maintenance`, { on: true });
    await page.goto("/datacenter/nodes");
    await page.getByRole("button", { name: "Update all nodes" }).click();
    const dlg = dialog(page, "Update all nodes");
    await expect(dlg.getByText("This cannot start yet")).toBeVisible({ timeout: 120_000 });
    await expect(dlg.getByText(new RegExp(`${env.node2} is in maintenance. End maintenance on it first`))).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Start update" })).toBeDisabled();
    // the API says the same and starts nothing
    const r = await api.ctx.fetch("/api/rolling-update", { method: "POST", data: { unmovable: "skip" } });
    expect(r.status()).toBe(409);
    await page.keyboard.press("Escape");
    await api.put(`/nodes/${n2}/maintenance`, { on: false });
  });

  test("the Updates tab of a node offers Update this node for that node only", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/updates`);
    await page.getByRole("button", { name: "Update this node" }).click();
    const dlg = dialog(page, "Update this node");
    const table = dlg.locator("table.rollplan");
    await expect(table).toBeVisible({ timeout: 120_000 });
    await expect(table.getByRole("row", { name: new RegExp(env.node1) })).toBeVisible();
    await expect(table.getByRole("row", { name: new RegExp(env.node2) })).toHaveCount(0);
    await page.setViewportSize({ width: 390, height: 844 });
    const fits = await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1);
    expect(fits, "the page does not scroll sideways at phone width").toBe(true);
    await page.keyboard.press("Escape");
  });
});
