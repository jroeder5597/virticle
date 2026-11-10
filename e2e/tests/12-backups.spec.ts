// Backups tab: back up now, restore as a new VM, export download, delete; global Backups page.
import { cleanVMs, createCirros, expect, openVM, tab, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, expectBadge, row } from "../lib/ui";
import { readFileSync, statSync } from "fs";

const NAME = `${P}bak1`;
const RESTORED = `${P}bak1-restored`;

test.describe.serial("backups", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, RESTORED, NAME));

  test("back up now (VM shut off) and see it in the list", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    vm = await createCirros(api, n1, NAME);
    await openVM(page, vm, "backups");
    await expect(page.getByText("No backups of this VM yet.")).toBeVisible();
    await page.getByRole("button", { name: "Back up now" }).first().click();
    const dlg = dialog(page, `Back up ${NAME}`);
    await expect(dlg.getByLabel("Target pool")).toHaveValue("default");
    await dlg.getByRole("button", { name: "Back up now" }).click();
    await expect(dlg.getByRole("progressbar")).toBeVisible();
    await expect(dlg).toContainText("ok", { timeout: 200_000 });
    // a finished task shows a full bar that is not alarming red
    const color = await dlg.getByRole("progressbar").locator("div").evaluate((e) => getComputedStyle(e).backgroundColor);
    const accent = await page.evaluate(() => {
      const el = document.createElement("div");
      el.style.background = "var(--danger)";
      document.body.appendChild(el);
      const c = getComputedStyle(el).backgroundColor;
      el.remove();
      return c;
    });
    expect(color).not.toBe(accent);
    await dlg.getByRole("button", { name: "Close dialog" }).click();
    const r = page.locator("tbody tr").first();
    await expect(r).toContainText("default");
    await expect(r).toContainText("vda");
    const list = await api.get(`/backups?uuid=${vm.uuid}`);
    expect(list).toHaveLength(1);
    expect(list[0].manifest.vm).toBe(NAME);
    expect(list[0].size).toBeGreaterThan(1024 * 1024);
  });

  test("the global Backups page lists it too", async ({ page }) => {
    await page.goto("/backups");
    await expect(page.getByRole("heading", { name: "Backups" }).first()).toBeVisible();
    await expect(row(page, NAME).first()).toBeVisible({ timeout: 60_000 });
  });

  test("export downloads a tar file", async ({ page }) => {
    await openVM(page, vm, "backups");
    const dl = page.waitForEvent("download");
    await page.getByRole("link", { name: "Export" }).click();
    const d = await dl;
    expect(d.suggestedFilename()).toMatch(/\.tar$/);
    const path = await d.path();
    expect(statSync(path!).size).toBeGreaterThan(1024 * 1024);
    // a tar archive carries "ustar" at offset 257 of its first header
    expect(readFileSync(path!).subarray(257, 262).toString()).toBe("ustar");
  });

  test("restore as a new VM", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await expect(dlg.getByLabel("New VM name")).toHaveValue(RESTORED);
    await dlg.getByRole("button", { name: "Restore", exact: true }).click();
    await expect(dlg.getByRole("progressbar")).toBeVisible();
    await expect(dlg).toContainText("ok", { timeout: 200_000 });
    await dlg.getByRole("button", { name: "Close dialog" }).click();
    const got = await api.vmByName(RESTORED);
    expect(got).toBeTruthy();
    expect(got.uuid).not.toBe(vm.uuid);
    const d = await api.vmDetail(got.node, got.uuid);
    expect(d.nics[0].mac).not.toBe((await api.vmDetail(vm.node, vm.uuid)).nics[0].mac);
    await page.goto("/");
    await expectBadge(row(page, RESTORED), "shutoff");
  });

  test("delete the backup", async ({ page, api }) => {
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, "Delete backup");
    await dlg.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(dlg).toBeHidden();
    await expect(page.getByText("No backups of this VM yet.")).toBeVisible();
    expect(await api.get(`/backups?uuid=${vm.uuid}`)).toHaveLength(0);
  });
});
