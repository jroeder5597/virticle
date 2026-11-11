import { expect, test } from "@playwright/test";
import { env } from "../lib/env";

test.use({ storageState: { cookies: [], origins: [] } });

test("wrong password shows an error and stays on the sign-in form", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "virt-proj" })).toBeVisible();
  await page.getByLabel("Username").fill(env.user);
  await page.getByLabel("Password").fill("definitely-not-the-password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.locator(".error")).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeEnabled();
  await expect(page.getByRole("link", { name: "Virtual machines" })).toHaveCount(0);
});

test("sign in with Enter, see the app, sign out", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("Username").fill(env.user);
  await page.getByLabel("Password").fill(env.pass);
  await page.getByLabel("Password").press("Enter");
  await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
  await expect(page.getByRole("link", { name: env.user, exact: true })).toBeVisible();
  // a reload keeps the session
  await page.reload();
  await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  const status = await page.request.get("/api/vms");
  expect(status.status()).toBe(401);
});

test("an unauthenticated deep link asks for sign-in and then keeps working", async ({ page }) => {
  await page.goto("/storage");
  await expect(page.getByRole("button", { name: "Sign in" })).toBeVisible();
  await page.getByLabel("Username").fill(env.user);
  await page.getByLabel("Password").fill(env.pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Storage" })).toBeVisible();
});
