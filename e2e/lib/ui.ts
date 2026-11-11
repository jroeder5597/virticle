import { expect, Locator, Page } from "@playwright/test";

/** The open modal dialog (optionally by its title). */
export const dialog = (page: Page, title?: string | RegExp) => (title ? page.getByRole("dialog", { name: title }) : page.getByRole("dialog"));

/** The input, select or textarea of a `<Field label=...>`, found by its exact label text. */
export function field(scope: Locator | Page, label: string): Locator {
  const page = "page" in scope && typeof scope.page === "function" ? (scope as Locator).page() : (scope as Page);
  return scope
    .locator("label.field")
    .filter({ has: page.getByText(label, { exact: true }) })
    .locator("input, select, textarea")
    .first();
}

/** Accept every confirm/prompt/alert; returns the messages seen so far. */
export function autoDialogs(page: Page, promptAnswer?: string) {
  const seen: string[] = [];
  page.on("dialog", (d) => {
    seen.push(d.message());
    d.type() === "prompt" ? d.accept(promptAnswer ?? d.defaultValue()) : d.accept();
  });
  return seen;
}

/** A row of the first table on the page (or inside scope) that contains the text. */
export const row = (scope: Locator | Page, text: string | RegExp) => scope.locator("tbody tr").filter({ hasText: text });

/** Opens the VM page's Actions menu and clicks Group, then Item (a submenu entry). */
export async function vmAction(page: Page, group: string, item: string) {
  const btn = page.locator(".head").getByRole("button", { name: "Actions", exact: true });
  if ((await btn.getAttribute("aria-expanded")) !== "true") await btn.click();
  const parent = page.getByRole("menuitem", { name: group, exact: true });
  if ((await parent.getAttribute("aria-expanded")) !== "true") await parent.click();
  await page.getByRole("menuitem", { name: item, exact: true }).click();
}

/** The dialog on top (the last one in the page). */
export const confirmDialog = (page: Page) => page.getByRole("dialog").last();

/** Confirms a destructive-action dialog with its confirm button (the last one in the footer). */
export async function confirm(page: Page, title?: string | RegExp) {
  const d = title ? page.getByRole("dialog", { name: title }) : confirmDialog(page);
  await expect(d).toBeVisible();
  await d.locator("footer button").last().click();
}

/** Cancels a confirmation dialog. */
export async function cancelConfirm(page: Page, title?: string | RegExp) {
  const d = title ? page.getByRole("dialog", { name: title }) : confirmDialog(page);
  await expect(d).toBeVisible();
  await d.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(d).toBeHidden();
}

export async function expectBadge(scope: Locator | Page, state: string) {
  await expect(scope.locator(".badge", { hasText: new RegExp(`^${state}$`) }).first()).toBeVisible();
}
