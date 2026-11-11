// Sidebar resource tree: nodes with their VMs, quick actions, filter, phone drawer.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { confirm, cancelConfirm } from "../lib/ui";

const NAME = `${P}tree1`;

test.describe.serial("resource tree", () => {
  let vm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
  });
  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  const tree = (page: Page) => page.getByRole("tree", { name: "Resources" });
  const vmRow = (page: Page) => tree(page).getByRole("treeitem", { name: new RegExp(`^${NAME}, `) });

  test("shows both nodes and the VM under its node", async ({ page }) => {
    await page.goto("/");
    await expect(tree(page)).toBeVisible();
    for (const n of [env.node1, env.node2]) await expect(tree(page).getByRole("treeitem", { name: new RegExp(`^${n}, `) })).toBeVisible();
    const row = vmRow(page);
    await expect(row).toBeVisible();
    await expect(row).toHaveAttribute("aria-level", "3");
    await expect(row).toHaveAttribute("data-state", "shutoff");
    // it sits after its node row
    const keys = await tree(page).locator("[data-key]").evaluateAll((els) => els.map((e) => (e as HTMLElement).dataset.key!));
    expect(keys.indexOf(`v:${vm.node}/${vm.uuid}`)).toBeGreaterThan(keys.indexOf(`n:${vm.node}`));
  });

  test("the quick action starts the VM and the state dot follows; force off from the context menu", async ({ page, api }) => {
    await page.goto("/");
    const row = vmRow(page);
    await row.hover();
    await row.locator(`button[aria-label="Power On ${NAME}"]`).click();
    await expect(row).toHaveAttribute("data-state", "running");
    await expect(row.locator(".dot.v-running")).toBeVisible();
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("running");
    await row.click({ button: "right" });
    await page.getByRole("menuitem", { name: "Halt (Force Shutdown)" }).click();
    await cancelConfirm(page, "Halt (Force Shutdown)");
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("running");
    await row.click({ button: "right" });
    await page.getByRole("menuitem", { name: "Halt (Force Shutdown)" }).click();
    await confirm(page, "Halt (Force Shutdown)");
    await expect(row).toHaveAttribute("data-state", "shutoff");
  });

  test("keyboard: the menu opens with Shift+F10, Escape closes it, Enter opens the VM", async ({ page }) => {
    await page.goto("/");
    const row = vmRow(page);
    await row.focus();
    await page.keyboard.press("Shift+F10");
    await expect(page.getByRole("menu")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(0);
    await expect(row).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/vm/${vm.node}/${vm.uuid}`));
    await expect(vmRow(page)).toHaveAttribute("aria-selected", "true");
  });

  test("the storage group loads its pools when opened", async ({ page }) => {
    await page.goto("/");
    const g = tree(page).locator(`[data-key="s:${vm.node}"]`);
    await expect(g).toHaveAttribute("aria-expanded", "false");
    await g.locator(`button[aria-label="Expand"]`).click();
    await expect(tree(page).getByRole("treeitem", { name: /^default/ }).first()).toBeVisible();
  });

  test("the filter narrows the tree and keeps the node", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel("Filter tree").fill(NAME);
    await expect(vmRow(page)).toBeVisible();
    await expect(tree(page).getByRole("treeitem", { name: new RegExp(`^${env.node1}, `) })).toBeVisible();
    await expect(tree(page).locator('[data-kind="vm"]')).toHaveCount(1);
    await page.getByLabel("Filter tree").fill("zz-no-such-thing");
    await expect(tree(page).locator('[data-kind="vm"]')).toHaveCount(0);
  });

  test("phone width: the sidebar is a drawer and nothing scrolls sideways", async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: ".auth/admin.json", viewport: { width: 390, height: 800 } });
    const page = await ctx.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
    await expect(page.getByRole("tree")).toBeHidden();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(1);
    await page.getByRole("button", { name: "Open navigation" }).click();
    await expect(tree(page)).toBeVisible();
    await expect(vmRow(page)).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(1);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tree")).toBeHidden();
    await ctx.close();
  });
});
