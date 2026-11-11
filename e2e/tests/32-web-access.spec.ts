// Settings > Web access: switching the listener to a self-signed HTTPS address, confirming from the new
// origin, the automatic rollback when it is not confirmed, the reverse proxy snippets, and (when HAProxy is
// available, E2E_HAPROXY=/path/to/haproxy) a real pass through HAProxy configured from the generated snippet.
//
// The controller under test must run in plain HTTP mode and be configured from stored settings, not from the
// -listen flag (which fixes the settings). The listener moves to https://<host>:E2E_HTTPS_PORT and is put back.
import { Browser, Page } from "@playwright/test";
import { ChildProcess, execFileSync, spawn } from "child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "fs";
import { createServer } from "net";
import { tmpdir } from "os";
import { join } from "path";
import { Api } from "../lib/api";
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";
import { dialog, field, pick } from "../lib/ui";

const u = new URL(env.base);
const HTTPS_PORT = process.env.E2E_HTTPS_PORT ?? "8446";
const HTTPS = `https://${u.hostname}:${HTTPS_PORT}`;
const PLAIN = `${u.hostname}:${u.port}`; // host:port of the plain listener
const OUT = "test-results/web-access";

const plainSettings = { mode: "off", listen: PLAIN };

async function status(api: Api) {
  return (await api.get("/web")).status;
}

/** Puts the controller back on its plain HTTP address, whatever state a failed test left it in. */
async function restorePlain(api: Api) {
  let a: Api;
  try {
    a = await Api.login(); // a fresh session on the plain address (falls back to the given one)
  } catch {
    a = api;
  }
  for (let i = 0; i < 3; i++) {
    try {
      let s = await status(a);
      if (s.pending) {
        await a.post("/web/revert", {});
        await new Promise((r) => setTimeout(r, 1500));
        s = await status(a);
      }
      if (s.settings.mode === "off") return;
      await a.put("/web", { settings: plainSettings });
      await a.post("/web/confirm", {});
    } catch {}
  }
}

async function secureContext(browser: Browser, base: string) {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, baseURL: base, storageState: { cookies: [], origins: [] } });
  return ctx;
}

async function signIn(page: Page, base: string) {
  await page.goto(base + "/");
  await page.getByLabel("Username").fill(env.user);
  await page.getByLabel("Password").fill(env.pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
}

test.describe.serial("web access", () => {
  test.beforeAll(async ({ api }) => {
    mkdirSync(OUT, { recursive: true });
    const s = await status(api);
    test.skip(!!s.locked, "the controller was started with listener flags; the settings are fixed");
    expect(s.settings.mode, "start this suite against a controller in plain HTTP mode").toBe("off");
  });
  test.afterAll(async ({ api }) => {
    await restorePlain(api);
  });

  test("self-signed: apply, confirm on the new origin, Secure cookie, wss, then back to HTTP", async ({ page, browser, api }) => {
    test.setTimeout(180_000);
    await page.goto("/web-access");
    await expect(page.getByRole("heading", { name: "Web access" })).toBeVisible();
    await expect(page.getByText("HTTP on " + PLAIN)).toBeVisible();
    await pick(page, "Mode", "Self-signed certificate");
    await field(page, "HTTPS address").fill(`${u.hostname}:${HTTPS_PORT}`);
    await field(page, "Public URL").fill(""); // an earlier run may have left one
    await field(page, "Names").fill(`${u.hostname}\nlocalhost`);
    await page.getByRole("button", { name: "Check", exact: true }).click();
    await expect(page.getByRole("list", { name: "Checks" })).toContainText("can be bound");
    await page.getByRole("button", { name: "Apply", exact: true }).click();
    await dialog(page, "Apply web access settings").getByRole("button", { name: "Apply", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Change waiting for confirmation" })).toBeVisible();
    await expect(page.getByRole("link", { name: HTTPS + "/" })).toBeVisible();
    await page.screenshot({ path: `${OUT}/pending-old-origin.png`, fullPage: true });

    // The new origin: a fresh browser context (nothing trusted, no cookie) signs in over HTTPS.
    const ctx = await secureContext(browser, HTTPS);
    const p2 = await ctx.newPage();
    await signIn(p2, HTTPS);
    const cookie = (await ctx.cookies()).find((c) => c.name === "virt_session");
    expect(cookie?.secure, "session cookie is Secure over HTTPS").toBe(true);
    expect(cookie?.httpOnly).toBe(true);
    // the event stream connects over wss
    const wss = await p2.evaluate(
      (url) =>
        new Promise<string>((resolve) => {
          const ws = new WebSocket(url);
          const t = setTimeout(() => resolve("timeout"), 10_000);
          ws.onopen = () => {
            clearTimeout(t);
            ws.close();
            resolve("open");
          };
          ws.onerror = () => {
            clearTimeout(t);
            resolve("error");
          };
        }),
      `wss://${new URL(HTTPS).host}/api/events`,
    );
    expect(wss).toBe("open");
    const hsts = await p2.request.get(HTTPS + "/api/status");
    expect(hsts.headers()["strict-transport-security"], "HSTS is off unless chosen").toBeUndefined();

    await p2.goto(HTTPS + "/web-access");
    await expect(p2.getByRole("heading", { name: "Keep this setting" })).toBeVisible();
    await expect(p2.getByText("SHA-256 fingerprint")).toBeVisible();
    await expect(p2.locator(".wa-fp code")).toHaveText(/^([0-9A-F]{2}:){31}[0-9A-F]{2}$/);
    await p2.screenshot({ path: `${OUT}/keep-new-origin.png`, fullPage: true });
    await p2.getByRole("button", { name: "Keep this setting" }).click();
    await expect(p2.getByRole("heading", { name: "Keep this setting" })).toHaveCount(0);
    await expect(p2.getByText(/^HTTPS on /)).toBeVisible();

    // Confirmed: the old plain address is gone.
    await expect.poll(async () => (await fetch(env.base + "/api/status").then((r) => r.status).catch(() => 0)), { timeout: 15_000 }).toBe(0);

    // Back to plain HTTP from the HTTPS page, confirmed on the plain origin.
    await pick(p2, "Mode", "Off (HTTP)");
    await field(p2, "HTTP address").fill(PLAIN);
    await p2.getByRole("button", { name: "Apply", exact: true }).click();
    await dialog(p2, "Apply web access settings").getByRole("button", { name: "Apply", exact: true }).click();
    await expect(p2.getByRole("heading", { name: "Change waiting for confirmation" })).toBeVisible();
    await page.goto("/web-access");
    await expect(page.getByRole("heading", { name: "Keep this setting" })).toBeVisible();
    await page.getByRole("button", { name: "Keep this setting" }).click();
    await expect(page.getByRole("heading", { name: "Keep this setting" })).toHaveCount(0);
    await expect(page.getByText("HTTP on " + PLAIN, { exact: false }).first()).toBeVisible();
    await ctx.close();
    expect((await status(api)).settings.mode).toBe("off");
  });

  test("not confirmed: the listener goes back to plain HTTP by itself", async ({ api, playwright }) => {
    test.setTimeout(150_000);
    const win: number = (await status(api)).windowSeconds;
    await api.put("/web", { settings: { mode: "selfsigned", httpsListen: `${u.hostname}:${HTTPS_PORT}`, names: [u.hostname, "localhost"] } });
    const tls = await playwright.request.newContext({ ignoreHTTPSErrors: true });
    expect((await tls.get(HTTPS + "/api/status")).status(), "the new listener answers during the window").toBe(200);
    expect((await api.get("/web")).status.pending).toBeTruthy();
    // A second change is refused while one waits.
    const again = await api.ctx.fetch("/api/web", { method: "PUT", data: { settings: plainSettings } });
    expect(again.status()).toBe(400);

    await api.waitFor(
      "the new listener to close",
      async () => (await tls.get(HTTPS + "/api/status").then(() => false, () => true)) || undefined,
      (win + 20) * 1000,
      2000,
    );
    const s = await status(api);
    expect(s.settings.mode).toBe("off");
    expect(s.pending).toBeFalsy();
    expect((await api.ctx.get("/api/status")).status()).toBe(200);
    await tls.dispose();
  });

  test("the check list refuses an address that is in use, and Apply changes nothing", async ({ api }) => {
    const busy = createServer();
    await new Promise<void>((r) => busy.listen(0, u.hostname, r));
    const port = (busy.address() as { port: number }).port;
    try {
      const settings = { mode: "selfsigned", httpsListen: `${u.hostname}:${port}`, names: ["localhost"] };
      const body = await (await api.ctx.fetch("/api/web/check", { method: "POST", data: { settings } })).json();
      expect(body.ok).toBe(false);
      expect(JSON.stringify(body.checks)).toContain("cannot listen on");
      const res = await api.ctx.fetch("/api/web", { method: "PUT", data: { settings } });
      expect(res.status()).toBe(400);
      expect((await status(api)).settings.mode).toBe("off");
    } finally {
      busy.close();
    }
  });

  test("reverse proxy mode shows ready-to-copy configuration for five proxies", async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await page.goto("/web-access");
    await pick(page, "Mode", "Behind a reverse proxy");
    await field(page, "Public URL").fill("https://vm.e2e.example");
    await field(page, "HTTP address").fill("127.0.0.1:9090");
    for (const [name, needle] of [
      ["nginx", "server_name vm.e2e.example;"],
      ["Apache httpd", "ServerName vm.e2e.example"],
      ["HAProxy", "ssl crt /etc/haproxy/certs/vm.e2e.example.pem"],
      ["Caddy", "vm.e2e.example {"],
      ["Traefik", "Host(`vm.e2e.example`)"],
    ] as const) {
      const card = page.locator(".wa-snippet").filter({ has: page.getByRole("heading", { name, exact: true }) });
      await expect(card.locator("pre")).toContainText(needle);
      await expect(card.locator("pre")).toContainText("127.0.0.1:9090");
    }
    // the box scrolls inside its card: it never makes the page wider
    const over = await page.evaluate(() => {
      const c = document.querySelector(".content-scroll");
      return Math.max(document.documentElement.scrollWidth - window.innerWidth, c ? c.scrollWidth - c.clientWidth : 0);
    });
    expect(over).toBeLessThanOrEqual(1);
    const nginx = page.locator(".wa-snippet").filter({ has: page.getByRole("heading", { name: "nginx", exact: true }) });
    await nginx.getByRole("button", { name: "Copy nginx configuration" }).click();
    await expect(nginx.getByRole("status")).toHaveText("Copied");
    const clip = await page.evaluate(() => navigator.clipboard.readText());
    expect(clip).toContain("proxy_set_header Upgrade");
    expect(clip).toContain("proxy_request_buffering off");
    expect(clip).toContain("proxy_pass http://127.0.0.1:9090;");
    await page.screenshot({ path: `${OUT}/proxy-snippets.png`, fullPage: true });
  });

  test("a bad proposal is refused with a check list and changes nothing", async ({ page, api }) => {
    await page.goto("/web-access");
    await pick(page, "Mode", "ACME (Let's Encrypt)");
    await field(page, "Domain names").fill("vm.e2e.example");
    await page.getByRole("button", { name: "Check", exact: true }).click();
    await expect(page.getByRole("list", { name: "Checks" })).toContainText("accept the certificate authority");
    await expect(page.getByRole("list", { name: "Checks" })).toContainText("TLS-ALPN-01 is validated on port 443");
    expect((await status(api)).settings.mode).toBe("off");
    // ACME cannot be issued in the lab (no public DNS): the page shows the status rows only.
    await page.getByLabel("I accept the certificate authority's terms of service").check();
    await page.getByRole("button", { name: "Check", exact: true }).click();
    await expect(page.getByRole("list", { name: "Checks" })).not.toContainText("accept the certificate authority");
  });
});

// A real pass through HAProxy. Needs a haproxy binary: E2E_HAPROXY=/path/to/haproxy (it is run from a temp
// config on high ports, nothing is installed or changed system-wide).
const HAPROXY = process.env.E2E_HAPROXY;
test.describe.serial("through HAProxy", () => {
  test.skip(!HAPROXY, "set E2E_HAPROXY to a haproxy binary to run this");
  const TLS_PORT = process.env.E2E_PROXY_TLS_PORT ?? "8481";
  const HTTP_PORT = process.env.E2E_PROXY_HTTP_PORT ?? "8482";
  const PUBLIC = `https://${u.hostname}:${TLS_PORT}`;
  let proc: ChildProcess | undefined;
  let dir = "";

  test.afterAll(async ({ api }) => {
    if (proc?.pid) {
      try {
        process.kill(proc.pid);
      } catch {}
    }
    await restorePlain(api);
  });

  test("snippet-derived config proxies the UI, cookies are Secure, wss works, forwarded headers are believed", async ({ api, browser, playwright }) => {
    test.setTimeout(150_000);
    // proxy mode on the SAME address: the listener is taken over. HAProxy connects from 127.0.0.2, which is the
    // only trusted address, so a request straight to the controller from 127.0.0.1 is not believed.
    const settings = { mode: "proxy", listen: PLAIN, publicUrl: PUBLIC, trustedProxies: ["127.0.0.2"] };
    const snip = (await api.post("/web/snippets", { settings })).snippets.find((s: any) => s.id === "haproxy");
    expect(snip.text).toContain(PLAIN);
    dir = mkdtempSync(join(tmpdir(), "e2e-haproxy-"));
    execFileSync("openssl", ["req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "2", "-subj", `/CN=${u.hostname}`, "-addext", `subjectAltName=IP:${u.hostname}`, "-keyout", join(dir, "k.pem"), "-out", join(dir, "c.pem")], { stdio: "ignore" });
    writeFileSync(join(dir, "bundle.pem"), readFileSync(join(dir, "c.pem")) + "\n" + readFileSync(join(dir, "k.pem")));
    // Only the listen addresses, the certificate path and the source address are adapted from the generated text.
    const cfg = (snip.text as string)
      .replace(/^ {4}bind :80$/m, `    bind ${u.hostname}:${HTTP_PORT}`)
      .replace(/bind :443 ssl crt \S+/, `bind ${u.hostname}:${TLS_PORT} ssl crt ${join(dir, "bundle.pem")}`)
      .replace(/(server virticle \S+) check/, "$1 source 127.0.0.2 check");
    expect(cfg).toContain(`bind ${u.hostname}:${TLS_PORT} ssl`);
    expect(cfg).toContain("source 127.0.0.2");
    writeFileSync(join(dir, "haproxy.cfg"), "global\n    maxconn 200\n\n" + cfg);
    execFileSync(HAPROXY!, ["-c", "-f", join(dir, "haproxy.cfg")], { stdio: "pipe" });
    proc = spawn(HAPROXY!, ["-db", "-f", join(dir, "haproxy.cfg")], { stdio: "ignore" });
    writeFileSync(join(dir, "haproxy.pid"), String(proc.pid));

    const r = await api.put("/web", { settings });
    expect(r.pending.gen).toBeGreaterThan(0);

    const ctx = await secureContext(browser, PUBLIC);
    const page = await ctx.newPage();
    await api.waitFor("haproxy to answer", async () => (await (await playwright.request.newContext({ ignoreHTTPSErrors: true })).get(PUBLIC + "/api/status").then((x) => x.ok(), () => false)) || undefined, 20_000, 500);
    await signIn(page, PUBLIC);
    const cookie = (await ctx.cookies()).find((c) => c.name === "virt_session");
    expect(cookie?.secure, "Secure cookie because the trusted proxy says https").toBe(true);
    const wss = await page.evaluate(
      (url) =>
        new Promise<string>((resolve) => {
          const ws = new WebSocket(url);
          const t = setTimeout(() => resolve("timeout"), 10_000);
          ws.onopen = () => {
            clearTimeout(t);
            ws.close();
            resolve("open");
          };
          ws.onerror = () => {
            clearTimeout(t);
            resolve("error");
          };
        }),
      `wss://${new URL(PUBLIC).host}/api/events`,
    );
    expect(wss, "the event stream upgrades through HAProxy").toBe("open");

    // Straight to the controller (peer 127.0.0.1 is not a trusted proxy): forwarded headers are ignored.
    const direct = await playwright.request.newContext();
    const dl = await direct.post(env.base + "/api/login", { data: { username: env.user, password: env.pass }, headers: { "X-Forwarded-Proto": "https", "X-Forwarded-For": "9.9.9.9", Origin: env.base } });
    expect(dl.ok()).toBe(true);
    expect(dl.headers()["set-cookie"] ?? "", "a spoofed X-Forwarded-Proto must not make the cookie Secure").not.toMatch(/Secure/i);
    const audit = (await api.get("/audit?limit=20")).entries.filter((e: any) => e.path === "/api/login");
    expect(audit.some((e: any) => e.ip === "9.9.9.9"), "spoofed X-Forwarded-For must not reach the audit log").toBe(false);

    // A large body passes the proxy unbuffered and unlimited: 40 MB to an endpoint that rejects it after reading the headers.
    const big = Buffer.alloc(40 * 1024 * 1024, 7);
    const up = await page.request.put(PUBLIC + "/api/nodes/1/pools/nonexistent/volumes/x.img", { data: big, headers: { "Content-Type": "application/octet-stream" }, timeout: 60_000 });
    expect([400, 404, 500, 503], `upload status ${up.status()}`).toContain(up.status());

    // Confirm through the proxy: the request arrives on the new listener (this proves the proxy path works).
    await page.goto(PUBLIC + "/web-access");
    await expect(page.getByRole("heading", { name: "Keep this setting" })).toBeVisible();
    await page.screenshot({ path: `${OUT}/haproxy-keep.png`, fullPage: true });
    await page.getByRole("button", { name: "Keep this setting" }).click();
    await expect(page.getByRole("heading", { name: "Keep this setting" })).toHaveCount(0);
    await expect(page.getByText(/^Forwarding headers are believed from 127\.0\.0\.2/)).toBeVisible();

    // Leave proxy mode: apply Off through the proxy, confirm on the plain origin.
    await pick(page, "Mode", "Off (HTTP)");
    await field(page, "HTTP address").fill(PLAIN);
    await page.getByRole("button", { name: "Apply", exact: true }).click();
    await dialog(page, "Apply web access settings").getByRole("button", { name: "Apply", exact: true }).click();
    // The address stays the same, so the page's next request already arrives on the new listener.
    await expect(page.getByRole("heading", { name: "Keep this setting" })).toBeVisible();
    await api.waitFor("pending change", async () => (await status(api)).pending || undefined, 10_000, 500);
    await api.post("/web/confirm", {});
    expect((await status(api)).settings.mode).toBe("off");
    await ctx.close();
    await direct.dispose();
  });
});
