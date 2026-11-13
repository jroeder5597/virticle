// Incremental backups: the first backup of a running VM is full, the next one copies only the changes and says so,
// a restore of the incremental one works, and deleting the full one takes the later ones with it.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog } from "../lib/ui";

const NAME = `${P}incr1`;
const RESTORED = `${P}incr1-restored`;

test.describe.serial("incremental backups", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, RESTORED, NAME));

  async function backupNow(page: import("@playwright/test").Page, expected: RegExp) {
    await page.getByRole("button", { name: "Back up now" }).first().click();
    const dlg = dialog(page, `Back up ${NAME}`);
    await expect(dlg.getByLabel("Target pool")).toContainText("default");
    await dlg.getByRole("button", { name: "Back up now" }).click();
    await expect(dlg).toContainText(expected, { timeout: 30_000 });
    await expect(dlg).toContainText(/ok/i, { timeout: 200_000 });
    await dlg.getByRole("button", { name: "Close dialog" }).click();
  }

  test("the first backup of a running VM is full, the second is incremental and small", async ({ page, api, n1 }) => {
    test.setTimeout(400_000);
    vm = await createCirros(api, n1, NAME, { start: true });
    await openVM(page, vm, "backups");
    await backupNow(page, /Full backup/);
    await backupNow(page, /Incremental backup/);
    const list = (await api.get(`/backups?uuid=${vm.uuid}`)).sort((a: any, b: any) => a.manifest.created.localeCompare(b.manifest.created));
    expect(list).toHaveLength(2);
    expect(list[0].manifest.kind ?? "full").toBe("full");
    expect(list[1].manifest.kind).toBe("incremental");
    expect(list[1].chain).toBe(2);
    expect(list[1].size).toBeLessThan(list[0].size);
    // the list says which is which, each with its own size
    const rows = page.locator("tbody tr");
    await expect(rows.filter({ hasText: "Incremental" })).toHaveCount(1);
    await expect(rows.filter({ hasText: /\bFull\b/ })).toHaveCount(1);
  });

  test("restoring the incremental backup gives a VM", async ({ page, api }) => {
    test.setTimeout(300_000);
    await openVM(page, vm, "backups");
    await page.locator("tbody tr", { hasText: "Incremental" }).getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await dlg.getByLabel("New VM name").fill(RESTORED);
    await dlg.getByRole("button", { name: "Restore", exact: true }).click();
    await expect(dlg).toContainText(/ok/i, { timeout: 250_000 });
    const all = await api.vms();
    expect(all.some((v: any) => v.name === RESTORED)).toBe(true);
  });

  test("deleting the full backup says it takes the later one along", async ({ page, api }) => {
    await openVM(page, vm, "backups");
    await page.locator("tbody tr", { hasText: /\bFull\b/ }).getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, "Delete backup");
    await expect(dlg).toContainText("1 later incremental backup builds on this one");
    await dlg.getByRole("button", { name: "Delete" }).click();
    await expect.poll(async () => (await api.get(`/backups?uuid=${vm.uuid}`)).length, { timeout: 60_000 }).toBe(0);
  });
});
