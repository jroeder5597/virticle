// Console preview card, click modes, standalone console page, console toolbar, roles.
import { Browser, Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";

const RUN = `${P}cons1`;
const OFF = `${P}cons2`;
const VIEWER = `${P}cviewer`;
const PASS = "e2e-user-pass-123";
const SHOT = /\/screenshot\?/;

/** Counts screenshot requests the page makes during `ms`. */
async function countShots(page: Page, ms: number): Promise<number> {
  let n = 0;
  const on = (r: { url(): string }) => SHOT.test(r.url()) && n++;
  page.on("request", on);
  await page.waitForTimeout(ms);
  page.off("request", on);
  return n;
}

const setHidden = (page: Page, hidden: boolean) =>
  page.evaluate((h) => {
    Object.defineProperty(document, "visibilityState", { get: () => (h ? "hidden" : "visible"), configurable: true });
    document.dispatchEvent(new Event("visibilitychange"));
  }, hidden);

test.describe.serial("console preview and extras", () => {
  let run: { node: number; uuid: string; name: string };
  let off: { node: number; uuid: string; name: string };
  const shotUrl = (v: { node: number; uuid: string }, q = "") => `/api/nodes/${v.node}/vms/${v.uuid}/screenshot${q}`;

  test.beforeAll(async ({ api, n1 }) => {
    run = await createCirros(api, n1, RUN, { start: true });
    off = await createCirros(api, n1, OFF);
    await api.post("/users", { username: VIEWER, password: PASS, role: "viewer" });
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, RUN, OFF);
    for (const u of await api.get("/users")) if (u.username.startsWith(P)) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("screenshot endpoint: PNG at two sizes, JPEG, refusals", async ({ api }) => {
    const png = async (q: string) => {
      const res = await api.ctx.get(shotUrl(run, q));
      expect(res.status()).toBe(200);
      expect(res.headers()["content-type"]).toBe("image/png");
      expect(res.headers()["cache-control"]).toBe("no-store");
      const b = await res.body();
      expect(b.subarray(1, 4).toString()).toBe("PNG");
      return b.readUInt32BE(16); // IHDR width
    };
    const full = await png("");
    const small = await png("?w=320");
    expect(small).toBe(320);
    expect(full).toBeGreaterThan(320);
    const j = await api.ctx.get(shotUrl(run, "?w=400&fmt=jpeg"));
    expect(j.headers()["content-type"]).toBe("image/jpeg");
    expect((await api.ctx.get(shotUrl(run, "?w=5"))).status()).toBe(400);
    const r = await api.ctx.get(shotUrl(off));
    expect(r.status()).toBe(409);
    expect((await r.json()).error).toMatch(/not running/);
  });

  test("overview: still preview appears; shut-off VM shows a static placeholder and asks for nothing", async ({ page }) => {
    await openVM(page, run);
    const img = page.locator(".cc-open img");
    await expect(img).toBeVisible({ timeout: 30_000 });
    expect(await img.evaluate((e: HTMLImageElement) => e.naturalWidth)).toBeGreaterThan(100);
    const before = await page.locator(".cc-box").boundingBox();
    const n = await (async () => {
      const c = countShots(page, 3000);
      await page.getByRole("button", { name: "Refresh preview" }).click();
      return c;
    })();
    expect(n).toBe(1);
    expect((await page.locator(".cc-box").boundingBox())!.height).toBe(before!.height);

    let asked = 0;
    page.on("request", (r) => SHOT.test(r.url()) && asked++);
    await openVM(page, off);
    await expect(page.locator(".cc-note")).toContainText("shut off");
    await page.waitForTimeout(1500);
    expect(asked).toBe(0);
  });

  test("refresh runs only while the card is on screen and the tab is visible", async ({ page }) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width: 1280, height: 600 });
    await openVM(page, run);
    await page.locator(".cc-box").scrollIntoViewIfNeeded();
    await expect(page.locator(".cc-open img")).toBeVisible({ timeout: 30_000 });
    const visible = await countShots(page, 23_000);
    expect(visible, "requests in 23 s while visible").toBeGreaterThanOrEqual(2);
    expect(visible).toBeLessThanOrEqual(3);

    await setHidden(page, true);
    expect(await countShots(page, 23_000), "requests in 23 s with the tab hidden").toBe(0);
    await setHidden(page, false);
    await expect(page.locator(".cc-open img")).toBeVisible();

    // scroll the card out of view: a short window scrolled to the top shows only the cards above it
    await page.setViewportSize({ width: 1280, height: 300 });
    await page.evaluate(() => {
      document.scrollingElement!.scrollTo(0, 0);
      document.querySelector("main")!.scrollTo(0, 0);
    });
    const gone = await page.locator(".cc-box").evaluate((e) => {
      const r = e.getBoundingClientRect();
      return r.bottom <= 0 || r.top >= innerHeight;
    });
    expect(gone, "the card is scrolled out of view").toBe(true);
    await page.waitForTimeout(500);
    expect(await countShots(page, 23_000), "requests in 23 s with the card off screen").toBe(0);
  });

  test("click opens the standalone console in a new tab", async ({ page, context }) => {
    await openVM(page, run);
    const [pop] = await Promise.all([context.waitForEvent("page"), page.locator(".cc-open").click()]);
    await pop.waitForLoadState();
    expect(pop.url()).toContain(`/console/${run.node}/${run.uuid}`);
    expect(await pop.evaluate(() => window.opener)).toBeNull();
    await expect(pop).toHaveTitle(RUN);
    await expect(pop.locator(".console.vc.page")).toBeVisible();
    await expect(pop.getByText("Connected")).toBeVisible({ timeout: 30_000 });
    await expect(pop.locator(".topbar, .sidebar")).toHaveCount(0);
    const box = await pop.locator(".console.vc.page").boundingBox();
    expect(box!.height).toBeGreaterThan(pop.viewportSize()!.height - 2);
    await pop.close();
  });

  test("inline mode: live console in the card, collapse returns the still image; setting is shared with the Account page", async ({ page }) => {
    await openVM(page, run);
    await page.getByRole("button", { name: "Console preview click behaviour" }).click();
    await page.getByRole("menuitemradio", { name: /inside the card/ }).click();
    expect(await page.evaluate(() => localStorage.getItem("vp-console-click"))).toBe("inline");
    await page.locator(".cc-open").click();
    const con = page.locator(".cc .console.inline");
    await expect(con.getByText("Connected")).toBeVisible({ timeout: 30_000 });
    await expect(con.locator("canvas")).toBeVisible();
    await con.getByRole("button", { name: "Collapse" }).click();
    await expect(page.locator(".cc-open img")).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(".cc .console")).toHaveCount(0);

    await page.goto("/account");
    const radio = page.getByRole("radiogroup", { name: "Console preview opens" });
    await expect(radio.getByRole("radio", { name: "inside the card" })).toBeChecked();
    await radio.getByRole("radio", { name: "in a new tab" }).check();
    expect(await page.evaluate(() => localStorage.getItem("vp-console-click"))).toBe("tab");
  });

  test("toolbar: keys menu, paste dialog, screenshot download, scale toggle, fullscreen, Ctrl+Alt+Del reaches the guest", async ({ page, api }) => {
    test.setTimeout(150_000);
    await openVM(page, run, "console");
    await expect(page.getByText("Connected")).toBeVisible({ timeout: 30_000 });
    const bar = page.locator(".console .toolbar");
    const sent: string[] = [];
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Network.enable");
    cdp.on("Network.webSocketFrameSent", (e) => sent.push(Buffer.from(e.response.payloadData, "base64").toString("hex")));

    await bar.getByRole("button", { name: "Keys" }).click();
    for (const k of ["Ctrl+Alt+F1", "Ctrl+Alt+F6", "Alt+Tab", "Super (Windows key)", "Print Screen"]) await expect(page.getByRole("menuitem", { name: k })).toBeVisible();
    await page.keyboard.press("Escape");

    await bar.getByRole("button", { name: "Paste text" }).click();
    const dlg = page.getByRole("dialog", { name: "Paste text into the guest" });
    await expect(dlg).toContainText("US keyboard layout");
    await dlg.getByRole("textbox").fill("Ab1!é");
    await expect(dlg).toContainText("1 character cannot be typed");
    await expect(dlg.getByRole("button", { name: "Type 4 characters" })).toBeVisible();
    await dlg.getByRole("button", { name: "Cancel" }).click();

    const [dl] = await Promise.all([page.waitForEvent("download"), bar.getByRole("button", { name: "Screenshot" }).click()]);
    expect(dl.suggestedFilename()).toMatch(new RegExp(`^${RUN}-\\d{8}-\\d{6}\\.png$`));

    const fit = bar.getByRole("button", { name: /Scale to fit is on/ });
    await fit.click();
    await expect(bar.getByRole("button", { name: /Actual size is on/ })).toHaveAttribute("aria-pressed", "false");
    await bar.getByRole("button", { name: /Actual size is on/ }).click();

    await bar.getByRole("button", { name: "Full screen" }).click();
    await expect.poll(() => page.evaluate(() => document.fullscreenElement?.className ?? "")).toContain("console");
    await bar.getByRole("button", { name: "Leave full screen" }).click();
    await expect.poll(() => page.evaluate(() => document.fullscreenElement === null)).toBe(true);

    // Ctrl+Alt+Del goes out as Control, Alt, Delete down and up (QEMU extended key events). The cirros kernel has no
    // keyboard driver, so what a guest does with the keys was checked by hand against a Debian VM.
    sent.length = 0;
    await bar.getByRole("button", { name: "Ctrl+Alt+Del" }).click();
    await expect
      .poll(() => sent.filter((h) => h.startsWith("ff00")).map((h) => h.slice(0, 24)), { timeout: 5000 })
      .toEqual(["ff0000010000ffe30000001d", "ff0000010000ffe900000038", "ff0000010000ffff000000d3", "ff0000000000ffff000000d3", "ff0000000000ffe900000038", "ff0000000000ffe30000001d"]);

  });

  test("phone width: the toolbar fits", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 800 });
    await openVM(page, run, "console");
    await expect(page.getByText("Connected")).toBeVisible({ timeout: 30_000 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(0);
    const bad = await page.locator(".console .toolbar button").evaluateAll((bs) => bs.filter((b) => b.getBoundingClientRect().right > innerWidth).length);
    expect(bad).toBe(0);
  });

  test("serial console: copy all, clear, full screen", async ({ page }) => {
    // The serial stream is a stand-in: closing a live libvirt console stream can hang the node's connection (an
    // existing library problem, see the report), and this test is about the toolbar.
    // (page.routeWebSocket would also break noVNC, which checks the socket object it is given.)
    await page.addInitScript(() => {
      const Real = window.WebSocket;
      window.WebSocket = new Proxy(Real, {
        construct(target, args) {
          if (!String(args[0]).endsWith("/serial")) return new target(...(args as [string]));
          const fake: any = { readyState: 0, binaryType: "", send() {}, close() {} };
          setTimeout(() => {
            fake.readyState = 1;
            fake.onopen?.({});
            fake.onmessage?.({ data: new TextEncoder().encode("login: \r\n").buffer });
          }, 50);
          return fake;
        },
      });
    });
    await openVM(page, run, "console");
    await page.getByRole("button", { name: "Serial console" }).click();
    await expect(page.getByText("Connected")).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(".xterm-rows")).toContainText("login:");
    const bar = page.locator(".console .toolbar");
    for (const b of ["Copy all", "Clear", "Full screen"]) await expect(bar.getByRole("button", { name: b })).toBeVisible();
    await bar.getByRole("button", { name: "Clear" }).click();
    await expect(page.locator(".xterm-rows")).not.toContainText("login:");
    await bar.getByRole("button", { name: "Full screen" }).click();
    await expect.poll(() => page.evaluate(() => document.fullscreenElement?.className ?? "")).toContain("console");
  });

  test("viewer: no preview request, no console", async ({ browser }: { browser: Browser }) => {
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base });
    const page = await ctx.newPage();
    let asked = 0;
    page.on("request", (r) => SHOT.test(r.url()) && asked++);
    await page.goto("/");
    await page.getByLabel("Username").fill(VIEWER);
    await page.getByLabel("Password").fill(PASS);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
    await openVM(page, run);
    await expect(page.locator(".cc-note")).toContainText("operators and administrators");
    await page.waitForTimeout(1500);
    expect(asked).toBe(0);
    await expect(page.locator(".tabs").getByRole("button", { name: "Console" })).toHaveCount(0);
    expect((await ctx.request.get(shotUrl(run))).status()).toBe(403);
    await page.goto(`/console/${run.node}/${run.uuid}`);
    await expect(page.getByText("operators and administrators")).toBeVisible();
    await expect(page.locator(".console")).toHaveCount(0);
    await ctx.close();
  });
});
