import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, field } from "../lib/ui";

// Snapshot actions answer at once and run as tasks; the Tasks panel shows them, running ones on top.
const VM = `${P}async1`;

test.describe.serial("async tasks", () => {
  let vm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    await cleanVMs(api, VM);
    vm = await createCirros(api, n1, VM);
    await api.put("/me/prefs", { dockH: "", tasksDock: "open" });
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
  });

  test("the API: ?async=1 answers 202 with a task id, without it the call waits", async ({ api }) => {
    const base = `/nodes/${vm.node}/vms/${vm.uuid}`;
    const res = await api.ctx.fetch(`/api${base}/snapshots?async=1`, { method: "POST", data: { name: "a1" } });
    expect(res.status()).toBe(202);
    const body = await res.json();
    expect(body.accepted).toBe(true);
    expect(body.task).toBeGreaterThan(0);
    await expect
      .poll(async () => (await api.get("/tasks")).find((t: any) => t.id === body.task)?.status, { timeout: 120_000 })
      .toBe("ok");
    expect((await api.get(`${base}/snapshots`)).map((s: any) => s.name)).toContain("a1");
    // a bad request is still refused at once, as before
    const bad = await api.ctx.fetch(`/api${base}/snapshots?async=1`, { method: "POST", data: { name: "bad name!" } });
    expect(bad.status()).toBe(400);
    // without the flag: done when the call returns
    await api.post(`${base}/snapshots`, { name: "s1" });
    expect((await api.get(`${base}/snapshots`)).map((s: any) => s.name)).toContain("s1");
  });

  test("Take snapshot closes at once; the task shows in the panel and the snapshot appears", async ({ page }) => {
    await page.setViewportSize({ width: 1360, height: 860 });
    await openVM(page, vm, "snapshots");
    // hold the server's answer back: the dialog must not wait for the snapshot, only for the 202
    let status = 0;
    page.on("response", (r) => {
      if (r.url().includes("/snapshots?async=1")) status = r.status();
    });
    await page.getByRole("button", { name: "Take snapshot" }).click();
    const dlg = dialog(page, "Take snapshot");
    await field(dlg, "Name").fill("ui1");
    await dlg.getByRole("button", { name: "Take snapshot" }).click();
    await expect(dlg).toBeHidden();
    expect(status).toBe(202);
    const row = page.locator(".dock .dk-row", { hasText: "snapshot create" }).first();
    await expect(row).toBeVisible();
    await expect(row).toContainText("Ok", { timeout: 120_000 });
    await expect(page.locator(".srow", { hasText: "ui1" }).first()).toBeVisible();
  });

  test("the panel opens three rows high by default and running tasks stay on top", async ({ page, api }) => {
    await page.setViewportSize({ width: 1360, height: 860 });
    await openVM(page, vm);
    const dock = page.locator(".dock");
    const h = await dock.evaluate((e) => e.getBoundingClientRect().height);
    expect(h).toBeLessThan(150);
    // three whole rows are in view under the column heads
    const rows = page.locator(".dock .dk-rows .dk-row");
    await expect(rows.nth(2)).toBeVisible();
    const fit = await page.evaluate(() => {
      const body = document.querySelector(".dock-body")!.getBoundingClientRect();
      return [...document.querySelectorAll(".dock .dk-rows .dk-row")].filter((r) => r.getBoundingClientRect().bottom <= body.bottom + 1).length;
    });
    expect(fit).toBeGreaterThanOrEqual(3);
    // ordering is by state first: a running task above newer finished ones (pure check of the page's order)
    const order = await page.evaluate(() => [...document.querySelectorAll(".dock .dk-rows .dk-row")].map((r) => r.getAttribute("data-status")));
    const firstDone = order.findIndex((s) => s !== "running");
    expect(order.slice(firstDone === -1 ? order.length : firstDone)).not.toContain("running");
  });
});
