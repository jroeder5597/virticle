// Two-step sign-in with an authenticator app, end to end, on any controller: a throw-away user turns it on with the
// secret shown on the page (the code is computed here), signs in with it, signs in once with a recovery code, is reset
// by an administrator, and is deleted. No identity provider or directory is needed. The "Require two-step sign-in"
// setting is not touched, so the controller's other users are not affected.
import { createHmac } from "node:crypto";
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";

const USER = `${P}twostep`;
const PASS = "e2e-two-step-pass-1";

function b32(s: string): Buffer {
  const a = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const c of s.replace(/\s/g, "").toUpperCase()) bits += a.indexOf(c).toString(2).padStart(5, "0");
  const out: number[] = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) out.push(parseInt(bits.slice(i, i + 8), 2));
  return Buffer.from(out);
}
// A code for the step `ahead` steps from now; the server accepts the one before and the one after the current step.
function code(secret: string, ahead = 0): string {
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000) + ahead));
  const h = createHmac("sha1", b32(secret)).update(msg).digest();
  const o = h[h.length - 1] & 15;
  return String((h.readUInt32BE(o) & 0x7fffffff) % 1_000_000).padStart(6, "0");
}

test.describe.serial("two-step sign-in", () => {
  let secret = "";
  let recovery: string[] = [];

  test.beforeAll(async ({ api }) => {
    await api.post("/users", { username: USER, password: PASS, role: "viewer" });
  });
  test.afterAll(async ({ api }) => {
    for (const u of (await api.get("/users")) as any[]) if (u.username === USER) await api.del(`/users/${u.id}`);
  });

  const signIn = async (page: any) => {
    await page.goto("/");
    await page.getByLabel("Username").fill(USER);
    await page.getByLabel("Password").fill(PASS);
    await page.getByRole("button", { name: "Sign in" }).click();
  };

  test("turn it on with the secret shown, and get recovery codes", async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base });
    const page = await ctx.newPage();
    await signIn(page);
    await expect(page.getByRole("link", { name: USER })).toBeVisible(); // signed in before the next page is asked for
    await page.goto("/account");
    await page.getByRole("button", { name: "Turn on two-step sign-in" }).click();
    await expect(page.getByText("Works with Google Authenticator, Microsoft Authenticator, Duo Mobile").first()).toBeVisible();
    await expect(page.getByRole("img", { name: /QR code/ })).toBeVisible();
    secret = (await page.locator(".ts-secret").innerText()).replace(/\s/g, "");
    expect(secret).toHaveLength(32);
    await page.getByLabel("Code from the app").fill("000000");
    await page.getByRole("button", { name: "Turn on", exact: true }).click();
    await expect(page.getByText("that code is not right")).toBeVisible();
    await page.getByLabel("Code from the app").fill(code(secret));
    await page.getByRole("button", { name: "Turn on", exact: true }).click();
    await expect(page.locator(".ts-codes code")).toHaveCount(10);
    recovery = (await page.locator(".ts-codes code").allInnerTexts()).map((c) => c.trim());
    await page.getByRole("button", { name: "I have saved them" }).click();
    await expect(page.getByText("10 recovery codes left")).toBeVisible();
    await ctx.close();
  });

  test("sign in needs the second step; a wrong code fails; the right one works", async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base });
    const page = await ctx.newPage();
    await signIn(page);
    await expect(page.getByText("Two-step sign-in. Enter the 6-digit code")).toBeVisible();
    await page.getByLabel("Code").fill("123456");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByText("that code is not right")).toBeVisible();
    // The code that confirmed the set-up is spent; the next step's code is accepted (clock drift window).
    await page.getByLabel("Code").fill(code(secret, 1));
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("link", { name: USER })).toBeVisible();
    await ctx.close();
  });

  test("a recovery code signs in once", async ({ browser }) => {
    for (const expectOk of [true, false]) {
      const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base });
      const page = await ctx.newPage();
      await signIn(page);
      await page.getByLabel("Code").fill(recovery[0]);
      await page.getByRole("button", { name: "Sign in" }).click();
      if (expectOk) await expect(page.getByRole("link", { name: USER })).toBeVisible();
      else await expect(page.getByText("that code is not right")).toBeVisible();
      await ctx.close();
    }
  });

  test("an administrator resets it and the user signs in with the password alone", async ({ page, api, browser }) => {
    await page.goto("/users");
    const row = page.getByRole("row", { name: new RegExp(USER) });
    await expect(row.getByText("On", { exact: true })).toBeVisible();
    await row.getByRole("button", { name: "Reset two-step sign-in" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Reset two-step sign-in" }).click();
    await expect(row.getByText("Off", { exact: true })).toBeVisible();
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base });
    const p = await ctx.newPage();
    await signIn(p);
    await expect(p.getByRole("link", { name: USER })).toBeVisible();
    await ctx.close();
    const audit = (await api.get("/audit?limit=200")).entries.map((e: any) => e.detail);
    expect(audit).toContain("two-step sign-in turned on");
    expect(audit).toContain(`two-step sign-in reset for ${USER}`);
  });
});
