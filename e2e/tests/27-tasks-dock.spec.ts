// The docked Tasks panel: layout, live rows from the event stream, collapse state kept per user on the server
// (and mirrored in the browser so a reload does not flash), phone default, and request count.
import { Browser, expect as pwExpect } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { Api } from "../lib/api";

const VM = `${P}dock1`;
const BIG = `${P}dockbig.qcow2`;
const OTHER = `${P}dockuser`;
const PASS = "e2e-dock-pass-123";
const DOCK = ".dock";

async function signInAs(browser: Browser, user: string, pass: string, viewport = { width: 1280, height: 720 }) {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base, viewport });
  const page = await ctx.newPage();
  await page.goto("/");
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
  return { ctx, page };
}

test.describe.serial("tasks dock", () => {
  let vm: { node: number; uuid: string; name: string };

  test.beforeAll(async ({ api }) => {
    // The admin's stored choice may come from an earlier run: start from "open".
    await api.put("/me/prefs", { tasksDock: "open" });
    if (!(await api.get("/users")).some((u: any) => u.username === OTHER)) await api.post("/users", { username: OTHER, password: PASS, role: "operator" });
  });
  test.afterAll(async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/pools/default/images/cancel`, { name: BIG }).catch(() => {});
    await cleanVMs(api, VM);
    for (const u of await api.get("/users")) if (u.username === OTHER) await api.del(`/users/${u.id}`).catch(() => {});
    await api.put("/me/prefs", { tasksDock: "open" }).catch(() => {});
  });

  test("open by default, three rows high; the page does not grow; the main area scrolls", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, VM);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    const dock = page.locator(DOCK);
    await expect(dock).toBeVisible();
    await expect(page.getByRole("button", { name: "Hide tasks" })).toHaveAttribute("aria-expanded", "true");
    const box = (await dock.boundingBox())!;
    expect(Math.round(box.height)).toBe(134); // the bar, the column heads and three rows
    expect(Math.round(box.y + box.height)).toBe(720);
    // the tree keeps its full height next to it
    const tree = (await page.locator(".sidebar").boundingBox())!;
    expect(Math.round(tree.y + tree.height)).toBe(720);
    expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBe(720);
    await expect(dock.getByRole("link", { name: "All tasks" })).toHaveAttribute("href", "/datacenter/tasks");

    // a long page in a short window: only the main area scrolls
    await page.setViewportSize({ width: 1280, height: 560 });
    await openVM(page, vm, "hardware");
    const m = await page.evaluate(() => {
      const c = document.querySelector(".content-scroll")!;
      const before = c.scrollTop;
      c.scrollTo(0, 99999);
      return { room: c.scrollHeight - c.clientHeight, moved: c.scrollTop - before, doc: document.documentElement.scrollHeight, win: innerHeight };
    });
    expect(m.room).toBeGreaterThan(50);
    expect(m.moved).toBeGreaterThan(0);
    expect(m.doc).toBe(m.win);
    // the last thing on the page can be scrolled clear of the dock
    const last = await page.evaluate(() => {
      const c = document.querySelector(".content-scroll")!.getBoundingClientRect();
      const main = document.querySelector("main")!.getBoundingClientRect();
      return main.bottom <= c.bottom + 1;
    });
    expect(last).toBe(true);
  });

  test("starting a VM adds a row without a reload; a failed action shows its error", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    const dock = page.locator(DOCK);
    await expect(dock.locator(".dock-table, .dock-note").first()).toBeVisible();
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    const started = dock.locator(".dk-row", { hasText: VM }).filter({ hasText: /start/ });
    await expect(started.first()).toBeVisible();
    await expect(started.first()).toContainText("Ok");
    // the target is a link to the VM
    await expect(started.first().getByRole("link", { name: VM })).toHaveAttribute("href", `/vm/${vm.node}/${vm.uuid}`);
    await expect(dock.getByText(/\d+ running/)).toBeVisible();

    // a long download: running with a progress bar, then cancelled, which fails with a message
    await api.post(`/nodes/${n1}/pools/default/images`, { id: "debian-13", name: BIG });
    const big = dock.locator(".dk-row", { hasText: BIG });
    await expect(big.first()).toContainText("Running", { timeout: 30_000 });
    await expect(big.first().locator(".dk-bar")).toBeVisible();
    await expect(dock.locator(".dock-count")).toContainText(/[1-9]\d* running/);
    await api.post(`/nodes/${n1}/pools/default/images/cancel`, { name: BIG });
    await expect(big.first()).toContainText("Error", { timeout: 30_000 });
    const err = big.first().locator(".dk-err");
    await expect(err).toContainText("cancelled");
    await expect(err).toHaveAttribute("title", /cancelled/);
    // one line, truncated by CSS; a click expands it
    const h1 = (await err.boundingBox())!.height;
    await err.click();
    await expect(err).toHaveAttribute("aria-expanded", "true");
    expect((await err.boundingBox())!.height).toBeGreaterThanOrEqual(h1);
    // the existing toast still appears, above the dock
    const toast = page.locator(".toast", { hasText: "failed" }).first();
    await expect(toast).toBeVisible();
    const tb = (await toast.boundingBox())!;
    const db = (await dock.boundingBox())!;
    expect(tb.y + tb.height).toBeLessThanOrEqual(db.y + 1);
  });

  test("no request storm: one GET /api/tasks in 30 s with a task running", async ({ page, api, n1 }) => {
    test.setTimeout(120_000);
    await api.post(`/nodes/${n1}/pools/default/images`, { id: "debian-13", name: BIG });
    let n = 0;
    page.on("request", (r) => {
      if (new URL(r.url()).pathname === "/api/tasks") n++;
    });
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    await expect(page.locator(DOCK).locator(".dk-row", { hasText: BIG }).first()).toContainText("Running", { timeout: 30_000 });
    await page.waitForTimeout(30_000);
    expect(n, "GET /api/tasks requests in 30 s").toBe(1);
    await api.post(`/nodes/${n1}/pools/default/images/cancel`, { name: BIG }).catch(() => {});
  });

  test("collapse keeps only the bar, grows the main area, survives a reload and follows the user to another browser", async ({ page, browser }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/datacenter");
    const scroll = page.locator(".content-scroll");
    await expect(page.locator(`${DOCK} .dock-table, ${DOCK} .dock-note`).first()).toBeVisible();
    const openH = (await scroll.boundingBox())!.height;
    await page.getByRole("button", { name: "Hide tasks" }).click();
    await expect(page.getByRole("button", { name: "Show tasks" })).toHaveAttribute("aria-expanded", "false");
    await expect(page.locator(`${DOCK} .dock-table`)).toHaveCount(0);
    const bar = (await page.locator(DOCK).boundingBox())!;
    expect(bar.height).toBeLessThan(40);
    expect((await scroll.boundingBox())!.height).toBeGreaterThan(openH + 100);
    await expect(page.locator(DOCK).getByText(/\d+ running/)).toBeVisible();

    // saved on the server for this user
    await pwExpect.poll(async () => (await (await page.request.get("/api/me/prefs")).json()).tasksDock).toBe("closed");
    // reload: collapsed from the first paint (the browser's copy), and still after the server answered
    await page.reload({ waitUntil: "domcontentloaded" });
    await expect(page.locator(`${DOCK}.closed`)).toBeVisible();
    await page.waitForTimeout(1500);
    await expect(page.locator(`${DOCK}.closed`)).toBeVisible();

    // the same user in a fresh browser: collapsed there too
    const second = await signInAs(browser, env.user, env.pass);
    await expect(second.page.locator(`${DOCK}.closed`)).toBeVisible();
    await expect(second.page.getByRole("button", { name: "Show tasks" })).toBeVisible();
    await second.ctx.close();

    // a different user is unaffected
    const other = await signInAs(browser, OTHER, PASS);
    await expect(other.page.locator(`${DOCK}.open`)).toBeVisible();
    await other.ctx.close();

    // open it again: that persists too
    await page.getByRole("button", { name: "Show tasks" }).click();
    await expect(page.locator(`${DOCK}.open`)).toBeVisible();
    await pwExpect.poll(async () => (await (await page.request.get("/api/me/prefs")).json()).tasksDock).toBe("open");
    const third = await signInAs(browser, env.user, env.pass);
    await expect(third.page.locator(`${DOCK}.open`)).toBeVisible();
    await third.ctx.close();
  });

  test("phone width: collapsed by default, then shows status, target and progress only", async ({ browser }) => {
    const { ctx, page } = await signInAs(browser, OTHER, PASS, { width: 390, height: 844 });
    const dock = page.locator(DOCK);
    await expect(dock.locator(".dock-bar")).toBeVisible();
    await expect(dock).toHaveClass(/closed/);
    await page.getByRole("button", { name: "Show tasks" }).click();
    await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: /^Status$/ })).toBeVisible();
    await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: /^Target$/ })).toBeVisible();
    await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: /^Result$/ })).toBeVisible();
    for (const h of ["Action", "Node", "By", "Started", "Duration"]) await expect(dock.locator(".dk-head .dk-cell").filter({ hasText: new RegExp(`^${h}$`) })).toBeHidden();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    expect(await page.evaluate(() => document.documentElement.scrollHeight - innerHeight)).toBeLessThanOrEqual(0);
    // put it back for the next run of this user
    await ctx.close();
  });

  test("the preferences endpoint refuses unknown keys and bad values, and an API token has none", async ({ api }) => {
    for (const body of [{ nope: "x" }, { tasksDock: "sideways" }, { tasksDock: 1 }, {}]) {
      const res = await api.ctx.fetch("/api/me/prefs", { method: "PUT", data: body });
      expect(res.status(), JSON.stringify(body)).toBe(400);
    }
    const tok = await api.post("/account/tokens", { name: `${P}dockprefs`, role: "viewer", expiresDays: 1 });
    const t = (await Api.login()).ctx;
    const res = await t.fetch("/api/me/prefs", { headers: { Authorization: `Bearer ${tok.secret}` } });
    expect(res.status()).toBe(403);
    await t.dispose();
    for (const x of await api.get("/account/tokens")) if (x.name === `${P}dockprefs`) await api.del(`/account/tokens/${x.id}`).catch(() => {});
  });
});
