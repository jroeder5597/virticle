// Move a disk to another storage pool: live (block copy) and offline (volume copy), through the UI.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, expectBadge, field, confirm, cancelConfirm, pickPool } from "../lib/ui";

const NAME = `${P}mv1`;
const POOL = `${P}mvpool`;

test.describe.serial("disk move", () => {
  let vm: { node: number; uuid: string; name: string };
  const disk = async (api: any) => (await api.vmDetail(vm.node, vm.uuid)).disks.find((k: any) => k.target === "vda");
  const volumes = (api: any, pool: string) => api.get(`/nodes/${vm.node}/pools/${pool}/volumes`) as Promise<any[]>;

  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME);
    const l: any[] = await api.get(`/nodes/${n1}/pools`);
    if (l.some((p) => p.name === POOL)) {
      for (const v of await api.get(`/nodes/${n1}/pools/${POOL}/volumes`).catch(() => [])) await api.del(`/nodes/${n1}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
      await api.post(`/nodes/${n1}/pools/${POOL}/action`, { action: "remove" }).catch(() => {});
    }
  });

  test("set up a second pool and a running VM", async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/pools`, { name: POOL, type: "dir", target: `/var/lib/libvirt/${POOL}` });
    vm = await createCirros(api, n1, NAME, { start: true });
  });

  test("move the disk of the running VM, deleting the old volume", async ({ page, api }) => {
    const before = (await disk(api)).source as string;
    await openVM(page, vm, "hardware");
    await expectBadge(page.locator(".head"), "running");
    await page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Disks", exact: true }) }).getByRole("row", { name: /vda/ }).getByRole("button", { name: "Move", exact: true }).click();
    const dlg = dialog(page, /^Move vda/);
    await pickPool(dlg, "Target pool", POOL);
    await expect(dlg.getByText("Live: the VM keeps running")).toBeVisible();
    await expect(dlg.locator("ul.checks li.error")).toHaveCount(0);
    await dlg.getByRole("button", { name: "Move disk" }).click();
    // "Delete the old volume afterwards" is on: ask first. Cancel changes nothing, confirm starts the move.
    await cancelConfirm(page, /^Move vda and delete the old volume/);
    await expect(dlg.getByText("Done.")).toHaveCount(0);
    await dlg.getByRole("button", { name: "Move disk" }).click();
    await confirm(page, /^Move vda and delete the old volume/);
    await expect(dlg.getByText("Done.")).toBeVisible({ timeout: 120_000 });
    await dlg.locator("footer").getByRole("button", { name: "Close" }).click();
    const after = (await disk(api)).source as string;
    expect(after).toContain(`/${POOL}/`);
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("running");
    const oldName = before.split("/").pop();
    expect((await volumes(api, "default")).some((v) => v.name === oldName)).toBe(false);
  });

  test("move it back while shut off, keeping the old volume", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await openVM(page, vm, "hardware");
    await page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Disks", exact: true }) }).getByRole("row", { name: /vda/ }).getByRole("button", { name: "Move", exact: true }).click();
    const dlg = dialog(page, /^Move vda/);
    await pickPool(dlg, "Target pool", "default");
    await dlg.getByLabel("Delete the old volume").uncheck();
    await expect(dlg.getByText("Offline: the VM is shut off.")).toBeVisible();
    await dlg.getByRole("button", { name: "Move disk" }).click();
    await expect(dlg.getByText("Done.")).toBeVisible({ timeout: 120_000 });
    await dlg.locator("footer").getByRole("button", { name: "Close" }).click();
    expect((await disk(api)).source).toContain("/images/");
    expect((await volumes(api, POOL)).length).toBe(1);
  });

  test("a name that already exists in the target is refused in the dialog", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Disks", exact: true }) }).getByRole("row", { name: /vda/ }).getByRole("button", { name: "Move", exact: true }).click();
    const dlg = dialog(page, /^Move vda/);
    await pickPool(dlg, "Target pool", POOL);
    await expect(dlg.locator("ul.checks li.error")).toContainText("already has a volume");
    await expect(dlg.getByRole("button", { name: "Move disk" })).toBeDisabled();
  });
});
