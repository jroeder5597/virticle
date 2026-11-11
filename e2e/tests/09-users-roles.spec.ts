// Users page, roles as seen by each role, password change, API tokens, audit log.
import { Browser, expect as pwExpect, request } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { Api } from "../lib/api";
import { autoDialogs, dialog, field, row } from "../lib/ui";

const OP = `${P}op`;
const VIEWER = `${P}viewer`;
const PASS = "e2e-user-pass-123";
const PASS2 = "e2e-user-pass-456";
const VM = `${P}rbac1`;

async function loginStatus(user: string, pass: string) {
  const c = await request.newContext({ baseURL: env.base });
  const s = (await c.post("/api/login", { data: { username: user, password: pass } })).status();
  await c.dispose();
  return s;
}

async function signInAs(browser: Browser, user: string, pass: string) {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base, viewport: { width: 1360, height: 860 } });
  const page = await ctx.newPage();
  await page.goto("/");
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
  return { ctx, page };
}

test.describe.serial("users and roles", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
    for (const u of await api.get("/users")) if (u.username.startsWith(P)) await api.del(`/users/${u.id}`).catch(() => {});
    for (const t of await api.get("/account/tokens")) if (t.name.startsWith(P)) await api.del(`/account/tokens/${t.id}`).catch(() => {});
  });

  test("admin adds an operator and a viewer on the Users page", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, VM);
    await page.goto("/users");
    for (const [name, role] of [[OP, "operator"], [VIEWER, "viewer"]]) {
      await page.getByRole("button", { name: "Add user" }).click();
      const dlg = dialog(page, "Add user");
      await expect(field(dlg, "Username")).toBeFocused();
      await field(dlg, "Username").fill(name);
      await field(dlg, "Password").fill(PASS);
      await field(dlg, "Role").selectOption(role);
      await dlg.getByRole("button", { name: "Add" }).click();
      await expect(dlg).toBeHidden();
      await expect(row(page, name)).toBeVisible();
      await expect(page.getByLabel(`Role of ${name}`)).toHaveValue(role);
    }
    const users = await api.get("/users");
    expect(users.find((u: any) => u.username === OP).role).toBe("operator");
    expect(users.find((u: any) => u.username === VIEWER).role).toBe("viewer");
  });

  test("a too-short password is refused in the dialog", async ({ page }) => {
    await page.goto("/users");
    await page.getByRole("button", { name: "Add user" }).click();
    const dlg = dialog(page, "Add user");
    await field(dlg, "Username").fill(`${P}short`);
    await field(dlg, "Password").fill("short");
    await dlg.getByRole("button", { name: "Add" }).click();
    await expect(dlg.locator(".error")).toContainText(/at least 10/);
    await page.keyboard.press("Escape");
  });

  test("viewer: read-only everywhere, 403 message on a forced request", async ({ browser }) => {
    const { ctx, page } = await signInAs(browser, VIEWER, PASS);
    const nav = page.locator(".topbar");
    await expect(nav.getByRole("link", { name: "Users" })).toHaveCount(0);
    await expect(nav.getByRole("link", { name: "Audit" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "New VM" })).toBeHidden();
    await expect(row(page, VM).getByRole("button", { name: "Start" })).toBeHidden();

    await page.goto(`/vm/${vm.node}/${vm.uuid}`);
    await expect(page.getByRole("heading", { level: 1, name: VM })).toBeVisible();
    for (const b of ["Start", "Delete", "Clone", "Migrate"]) await expect(page.locator(".head").getByRole("button", { name: b })).toBeHidden();
    await expect(page.getByPlaceholder("add tag")).toHaveCount(0);
    await expect(page.locator(".tabs").getByRole("button", { name: "Console" })).toHaveCount(0);
    await expect(page.getByRole("checkbox", { name: /^No$|^Yes$/ })).toBeDisabled();

    // Hardware: every input and select is disabled, no action button is shown
    await page.locator(".tabs").getByRole("button", { name: "Hardware" }).click();
    await expect(page.getByRole("heading", { name: "Boot order" })).toBeVisible();
    const hw = page.locator("fieldset.plain");
    const inputs = hw.locator("input, select, textarea");
    const n = await inputs.count();
    for (let i = 0; i < n; i++) await expect(inputs.nth(i)).toBeDisabled();
    for (const b of ["Edit", "Add disk", "Add interface", "Save boot order"]) await expect(hw.getByRole("button", { name: b }).first()).toBeHidden();

    await page.locator(".tabs").getByRole("button", { name: "XML" }).click();
    await expect(page.getByLabel("Domain XML")).toHaveAttribute("readonly", "");
    await expect(page.getByRole("button", { name: "Save" })).toBeHidden();

    // a forced request is refused with the server's explanation
    const res = await page.evaluate(async (u) => {
      const r = await fetch(u, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ action: "start" }) });
      return { status: r.status, body: await r.json() };
    }, `/api/nodes/${vm.node}/vms/${vm.uuid}/action`);
    expect(res.status).toBe(403);
    expect(res.body.error).toMatch(/needs the operator role, you are viewer/);
    expect((await (await new Api(ctx.request).get(`/nodes/${vm.node}/vms/${vm.uuid}`)).state)).toBe("shutoff");
    // /users is not reachable for a viewer either
    await page.goto("/users");
    await expect(page.getByText(/nothing at this address/)).toBeVisible();
    await ctx.close();
  });

  test("operator: manages VMs but sees no Users or Audit", async ({ browser, api }) => {
    const { ctx, page } = await signInAs(browser, OP, PASS);
    const nav = page.locator(".topbar");
    await expect(nav.getByRole("link", { name: "Storage" })).toBeVisible();
    await expect(nav.getByRole("link", { name: "Users" })).toHaveCount(0);
    await expect(nav.getByRole("link", { name: "Audit" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "New VM" })).toBeVisible();
    await row(page, VM).getByRole("button", { name: "Start" }).click();
    await expect(row(page, VM).locator(".badge", { hasText: "running" })).toBeVisible();
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    const status = await ctx.request.get("/api/users");
    expect(status.status()).toBe(403);
    await page.goto("/audit");
    await expect(page.getByText(/nothing at this address/)).toBeVisible();
    await ctx.close();
  });

  test("operator changes their own password and back", async ({ browser }) => {
    const { ctx, page } = await signInAs(browser, OP, PASS);
    await page.getByRole("link", { name: OP, exact: true }).click();
    await expect(page.getByRole("heading", { name: "My account" })).toBeVisible();
    const change = async (from: string, to: string) => {
      const f = page.locator("form.card", { hasText: "Change password" });
      await field(f, "Current password").fill(from);
      await field(f, "New password").fill(to);
      await f.getByRole("button", { name: "Change password" }).click();
      await expect(page.getByText("Password changed.")).toBeVisible();
    };
    // a wrong current password is refused
    const f = page.locator("form.card", { hasText: "Change password" });
    await field(f, "Current password").fill("not-my-password");
    await field(f, "New password").fill(PASS2);
    await f.getByRole("button", { name: "Change password" }).click();
    await expect(f.locator(".error")).toBeVisible();

    await change(PASS, PASS2);
    expect(await loginStatus(OP, PASS2)).toBe(200);
    expect(await loginStatus(OP, PASS)).toBe(401);
    await change(PASS2, PASS);
    expect(await loginStatus(OP, PASS)).toBe(200);
    await ctx.close();
  });

  test("API token: create in the UI, use it once, revoke", async ({ page, api }) => {
    autoDialogs(page);
    await page.goto("/account");
    const tokName = `${P}tok`;
    const card = page.locator(".card", { hasText: "API tokens" });
    await field(card, "Name").fill(tokName);
    await field(card, "Role").selectOption("viewer");
    await card.getByRole("button", { name: "Create token" }).click();
    const secret = (await card.locator(".secretbox code").innerText()).trim();
    expect(secret.length).toBeGreaterThan(20);
    await expect(row(card, tokName)).toContainText("viewer");

    const tok = await request.newContext({ baseURL: env.base, extraHTTPHeaders: { Authorization: `Bearer ${secret}` } });
    const list = await tok.get("/api/vms");
    expect(list.status()).toBe(200);
    expect((await list.json()).some((v: any) => v.name === VM)).toBe(true);
    // a viewer token cannot change anything
    const forced = await tok.post(`/api/nodes/${vm.node}/vms/${vm.uuid}/action`, { data: { action: "start" } });
    expect(forced.status()).toBe(403);

    await row(card, tokName).getByRole("button", { name: "Revoke" }).click();
    await expect(row(card, tokName)).toHaveCount(0);
    expect((await tok.get("/api/vms")).status()).toBe(401);
    await tok.dispose();
  });

  test("Audit page lists the actions of this run", async ({ page }) => {
    await page.goto("/audit");
    await expect(page.getByRole("heading", { name: "Audit log" })).toBeVisible();
    await page.getByLabel("Filter by user").fill(env.user);
    await page.getByLabel("Filter by path").fill("/api/users");
    await page.getByRole("button", { name: "Search" }).click();
    await expect(page.locator("tbody tr").filter({ hasText: "POST /api/users" }).first()).toBeVisible();
    await page.getByLabel("Filter by user").fill(OP);
    await page.getByLabel("Filter by path").fill("password");
    await page.getByRole("button", { name: "Search" }).click();
    await expect(page.locator("tbody tr").filter({ hasText: "POST /api/account/password" }).first()).toBeVisible();
    await page.getByLabel("Filter by user").fill(VIEWER);
    await page.getByLabel("Filter by path").fill("");
    await page.getByLabel("Filter by method").selectOption("POST");
    await page.getByRole("button", { name: "Search" }).click();
    // the viewer's refused request is recorded with its 403
    await expect(page.locator("tbody tr").filter({ hasText: "/action" }).filter({ hasText: "403" }).first()).toBeVisible();
  });

  test("admin removes both users on the Users page", async ({ page }) => {
    autoDialogs(page);
    await page.goto("/users");
    for (const name of [OP, VIEWER]) {
      await row(page, name).getByRole("button", { name: "Delete" }).click();
      await expect(row(page, name)).toHaveCount(0);
    }
  });
});
