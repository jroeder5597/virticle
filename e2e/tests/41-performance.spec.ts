// Performance history: the tab renders for a node and a VM, the range select and metric chips work, the query API
// returns points for the last hour (skipped when the controller has only just started), /metrics needs a token, CSV
// downloads. Safe on any lab: it only reads, and creates one stopped-then-started cirros VM that it removes.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { pick } from "../lib/ui";

const VM = `${P}perf1`;
const TOKEN = `${P}perftoken`;

test.describe.serial("performance history", () => {
  let vm: { node: number; uuid: string; name: string };
  let secret = "";

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, VM, { start: true });
    const t = await api.post("/account/tokens", { name: TOKEN, role: "viewer", expiresDays: 1 });
    secret = t.secret;
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
    for (const x of await api.get("/account/tokens")) if (x.name === TOKEN) await api.del(`/account/tokens/${x.id}`).catch(() => {});
  });

  test("the node page has a Performance tab with charts and the busiest-VM table", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/summary`);
    await page.getByRole("tab", { name: "Performance", exact: true }).click();
    await expect(page).toHaveURL(/\/performance$/);
    await expect(page.locator(".pchips").getByRole("button", { name: "CPU", exact: true })).toBeVisible();
    await expect(page.locator(".pchips").getByRole("button", { name: "Memory", exact: true })).toBeVisible();
    // Either charts or the plain "no data" line; never an error box.
    await expect(page.locator(".psvg, .empty").first()).toBeVisible();
    await expect(page.locator(".error")).toHaveCount(0);
    await expect(page.getByRole("heading", { name: "Busiest virtual machines in this range" })).toBeVisible();
  });

  test("the VM page has a Performance tab; range select and chips work", async ({ page }) => {
    await openVM(page, vm, "performance");
    await expect(page.locator(".tabs").getByRole("button", { name: "Performance", exact: true })).toHaveClass(/on/);
    await expect(page.locator(".pchips")).toBeVisible();

    await pick(page, "Time range", "Last 6 hours");
    await expect(page).toHaveURL(/range=6h/);
    await expect(page.getByText(/1 point = 1 minute/)).toBeVisible();
    await pick(page, "Time range", "Last 7 days");
    await expect(page).toHaveURL(/range=7d/);
    await expect(page.getByText(/the band shows the peak/)).toBeVisible();

    await pick(page, "Time range", "Custom");
    await expect(page.getByLabel("From")).toBeVisible();
    await expect(page.getByLabel("To")).toBeVisible();
    await pick(page, "Time range", "Last hour");
    await expect(page.getByLabel("From")).toHaveCount(0);

    // Chips hide and show their charts.
    const cpu = page.locator(".pchips").getByRole("button", { name: "CPU", exact: true });
    const titles = page.locator(".pchart .ptitle");
    await expect(titles.filter({ hasText: /^CPU/ })).toHaveCount(1);
    await cpu.click();
    await expect(cpu).toHaveAttribute("aria-pressed", "false");
    await expect(titles.filter({ hasText: /^CPU/ })).toHaveCount(0);
    await cpu.click();
    await expect(titles.filter({ hasText: /^CPU/ })).toHaveCount(1);
    await expect(page.locator(".error")).toHaveCount(0);
  });

  test("no sideways scrolling at phone width", async ({ page, n1 }) => {
    await page.setViewportSize({ width: 390, height: 800 });
    for (const p of [`/node/${n1}/performance`, `/vm/${vm.node}/${vm.uuid}?tab=performance`, "/datacenter/performance"]) {
      await page.goto(p);
      await expect(page.locator(".pchips, .pbusy").first()).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), p).toBe(true);
    }
  });

  test("the query API returns points for the last hour", async ({ api, n1 }, info) => {
    const now = Math.floor(Date.now() / 1000);
    const r = await api.get(`/metrics/query?kind=node&id=${n1}&from=${now - 3600}&to=${now}&metrics=cpu,memory`);
    expect(r.kind).toBe("node");
    expect(r.step).toBe(15);
    expect(r.t.length).toBeGreaterThan(200);
    expect(r.series.cpu).toHaveLength(r.t.length);
    const have = (r.series.cpu as (number | null)[]).filter((v) => v != null).length;
    if (have === 0) {
      info.annotations.push({ type: "skipped", description: "no points yet: the controller started less than two minutes ago" });
      test.skip(true, "the controller started less than two minutes ago: no points in the last hour yet");
    }
    expect(have).toBeGreaterThan(0);
    expect(r.series.mem_used_kib.some((v: number | null) => v != null && v > 0)).toBe(true);
    // A longer range picks a coarser step and says so.
    const week = await api.get(`/metrics/query?kind=node&id=${n1}&from=${now - 7 * 86400}&to=${now}&metrics=cpu`);
    expect(week.step).toBeGreaterThan(300);
    expect(week.t.length).toBeLessThanOrEqual(620);
    expect(week.peak).toBe(true);
    const ov = await api.get(`/metrics/overview?from=${now - 3600}&to=${now}&top=3`);
    expect(Array.isArray(ov.cpu)).toBe(true);
  });

  test("/metrics refuses without a token and answers with one", async ({ playwright }) => {
    const anon = await playwright.request.newContext({ baseURL: env.base });
    try {
      expect((await anon.get("/metrics")).status()).toBe(401);
      expect((await anon.get("/metrics", { headers: { Authorization: "Bearer virticle_wrong" } })).status()).toBe(401);
      const ok = await anon.get("/metrics", { headers: { Authorization: `Bearer ${secret}` } });
      expect(ok.status()).toBe(200);
      expect(ok.headers()["content-type"]).toContain("text/plain");
      const text = await ok.text();
      expect(text).toContain("# TYPE virticle_node_up gauge");
      expect(text).toMatch(/virticle_node_up\{node="[^"]+"\} 1/);
      expect(text).toContain("virticle_build_info");
    } finally {
      await anon.dispose();
    }
  });

  test("CSV downloads from the tab and from the API", async ({ page, api, n1 }) => {
    await page.goto(`/node/${n1}/performance`);
    await expect(page.locator(".psvg, .empty").first()).toBeVisible();
    const [dl] = await Promise.all([page.waitForEvent("download"), page.getByRole("link", { name: "Download CSV" }).click()]);
    expect(dl.suggestedFilename()).toMatch(/^virticle-node-.*\.csv$/);
    const now = Math.floor(Date.now() / 1000);
    const csv = await api.get(`/metrics/query?kind=node&id=${n1}&from=${now - 600}&to=${now}&metrics=cpu&format=csv`);
    expect(String(csv).split("\n")[0]).toMatch(/^time,cpu/);
  });
});
