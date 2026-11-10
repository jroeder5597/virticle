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

export async function expectBadge(scope: Locator | Page, state: string) {
  await expect(scope.locator(".badge", { hasText: new RegExp(`^${state}$`) }).first()).toBeVisible();
}
