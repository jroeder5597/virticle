// Nodes page: maintenance (one action, with the offer to move the VMs), evacuation of one prefixed VM.
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, row } from "../lib/ui";

const VM = `${P}evac1`;

test.describe.serial("nodes", () => {
  test.afterAll(async ({ api, n1, n2 }) => {
    // never leave a node in maintenance
    for (const n of [n1, n2]) await api.put(`/nodes/${n}/maintenance`, { on: false }).catch(() => {});
    await cleanVMs(api, VM);
  });

  test("the node list shows both nodes online with hardware, and the table fits", async ({ page }) => {
    await page.goto("/nodes");
    await expect(page.getByRole("heading", { name: "Nodes" })).toBeVisible();
    for (const name of [env.node1, env.node2]) {
      const r = row(page, name);
      await expect(r).toBeVisible();
      await expect(r.locator(".badge", { hasText: "online" })).toBeVisible();
      await expect(r).toContainText(/\d+ ×/); // CPU count and model
      await expect(r).toContainText(/\d+(\.\d)? GiB total/);
    }
    // updates and encrypted migration live on the node page, so the list fits the screen
    const fits = await page.locator(".table-scroll").evaluate((e) => e.scrollWidth <= e.clientWidth + 1);
    expect(fits, "nodes table fits without scrolling sideways").toBe(true);
  });

  test("maintenance toggles on and off and the evacuation dialog honours it", async ({ page, api, n1, n2 }) => {
    await page.goto("/nodes");
    const r2 = row(page, env.node2);
    // one Maintenance action; a separate Evacuate button exists only for a node that is already in maintenance
    await expect(r2.getByRole("button", { name: "Evacuate" })).toHaveCount(0);
    await r2.getByRole("button", { name: "Maintenance", exact: true }).click();
    // a node with VMs offers to move them in the same step: not now
    const ask = dialog(page, `Maintenance on ${env.node2}`);
    await expect(r2.locator(".badge.maint").or(ask)).toBeVisible();
    if (await ask.isVisible()) {
      await ask.getByLabel(/Move its virtual machines to other nodes/).uncheck();
      await ask.getByRole("button", { name: "Start maintenance" }).click();
    }
    await expect(r2.locator(".badge.maint")).toBeVisible();
    expect((await api.nodes()).find((n) => n.id === n2)?.maintenance).toBe(true);
    // a node in maintenance is not offered as an evacuation target
    await api.put(`/nodes/${n1}/maintenance`, { on: true });
    await page.goto("/nodes");
    await row(page, env.node1).getByRole("button", { name: "Evacuate" }).click();
    const dlg = dialog(page, `Evacuate ${env.node1}`);
    await dlg.getByText("Advanced", { exact: true }).click();
    await expect(dlg.locator("label.check", { hasText: env.node2 }).locator("input")).toBeDisabled();
    await expect(dlg.locator("label.check", { hasText: env.node2 }).locator(".badge.maint")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
    // turn both off again
    await api.put(`/nodes/${n1}/maintenance`, { on: false });
    await page.goto("/nodes");
    await r2.getByRole("button", { name: "End maintenance" }).click();
    await expect(r2.locator(".badge.maint")).toHaveCount(0);
    await expect(r2.getByRole("button", { name: "Maintenance", exact: true })).toBeVisible();
    expect((await api.nodes()).find((n) => n.id === n2)?.maintenance).toBeFalsy();
  });

  test("evacuate node 1: tick only the e2e VM, check the plan, run it", async ({ page, api, n1, n2 }) => {
    test.setTimeout(300_000);
    const vm = await createCirros(api, n1, VM);
    await page.goto("/nodes");
    // Maintenance on node 1 offers, in the same step, to move its VMs: that opens the plan, which is checked at once
    await row(page, env.node1).getByRole("button", { name: "Maintenance", exact: true }).click();
    const ask = dialog(page, `Maintenance on ${env.node1}`);
    await expect(ask.getByLabel(/Move its virtual machines to other nodes/)).toBeChecked();
    await ask.getByRole("button", { name: "Start maintenance" }).click();
    const dlg = dialog(page, `Evacuate ${env.node1}`);
    const plan = dlg.locator(".evacplan");
    await expect(plan).toBeVisible({ timeout: 90_000 });
    await expect(plan.locator("tbody tr", { hasText: VM })).toContainText(env.node2);
    // untick every VM that is not ours: other work on this lab must never move
    const boxes = plan.locator('input[type="checkbox"]');
    const n = await boxes.count();
    for (let i = 0; i < n; i++) {
      const label = (await boxes.nth(i).getAttribute("aria-label")) ?? "";
      if (!label.includes(VM)) await boxes.nth(i).uncheck();
    }
    await expect(plan.getByLabel(`Move ${VM}`)).toBeChecked();
    const go = dlg.getByRole("button", { name: /^Evacuate \d+ VM/ });
    await expect(go).toHaveText("Evacuate 1 VM(s)");
    await go.click();
    // progress table: our VM goes to node 2 and finishes ok
    const res = dlg.locator("tbody tr", { hasText: VM });
    await expect(res).toContainText(env.node2, { timeout: 30_000 });
    await expect(res.locator(".badge.es-ok")).toBeVisible({ timeout: 240_000 });
    await expect(dlg).toContainText("Finished.", { timeout: 60_000 });
    await dlg.getByRole("button", { name: "Close", exact: true }).click();
    const got = await api.vmByName(VM);
    expect(got.node).toBe(n2);
    // and nothing else of ours was touched
    expect((await api.vms()).filter((v) => v.name.startsWith(P) && v.name !== VM)).toHaveLength(0);
  });
});
