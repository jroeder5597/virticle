// Creating a VM through the form, its consoles, and every lifecycle button.
import { cleanVMs, expect, openVM, stateOf, tab, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row, vmAction, confirm, cancelConfirm, pick } from "../lib/ui";

const NAME = `${P}life1`;

test.describe.serial("create from a disk image and drive the lifecycle", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  test("New VM form: disk image, BIOS, start", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await expect(dlg).toBeVisible();
    // initial focus is in the name field
    await expect(field(dlg, "Name")).toBeFocused();
    await field(dlg, "Name").fill(NAME);
    await pick(dlg, field(dlg, "Node"), env.node1);
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await pick(dlg, "Base image", new RegExp("^" + env.cirros.replace(/\./g, "\\.")));
    await field(dlg, "Disk size (GiB)").fill("1");
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await expect(field(dlg, "Firmware")).toContainText("BIOS");
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    await expect(page).toHaveURL(/\/vm\/\d+\/[0-9a-f-]+\?tab=console/);
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();

    const got = await api.vmByName(NAME);
    expect(got).toBeTruthy();
    vm = { node: got.node, uuid: got.uuid, name: NAME };
    expect(got.node).toBe(n1);
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.firmware).toBe("bios");
    expect(d.vcpus).toBe(1);
    expect(d.state).toBe("running");
    expect(d.memoryKiB).toBe(128 * 1024);
  });

  test("the VM is in the list and its detail page shows the specs", async ({ page }) => {
    await page.goto("/datacenter/vms");
    const r = row(page, NAME);
    await expect(r).toBeVisible();
    await expectBadge(r, "running");
    await expect(r.getByRole("cell", { name: env.node1 })).toBeVisible();
    await r.getByRole("link", { name: NAME }).click();
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();
    const summary = page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Hardware", exact: true }) });
    await expect(summary).toContainText("BIOS");
    await expect(summary).toContainText("128 MiB");
    await expect(summary).toContainText(vm.uuid);
    await tab(page, "Hardware");
    await expect(page.locator(".card", { hasText: "Disks" }).first()).toContainText(`${NAME}.qcow2`);
  });

  test("VNC console renders a canvas", async ({ page }) => {
    await openVM(page, vm, "console");
    const canvas = page.locator(".console canvas").first();
    await expect(canvas).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(".console .toolbar")).toContainText("Connected");
    const box = (await canvas.boundingBox())!;
    expect(box.width).toBeGreaterThan(100);
    expect(box.height).toBeGreaterThan(100);
  });

  test("serial console connects", async ({ page }) => {
    await openVM(page, vm, "console");
    await page.getByRole("button", { name: "Serial console" }).click();
    await expect(page.locator(".console .toolbar")).toContainText("Connected", { timeout: 20_000 });
    await expect(page.locator(".xterm")).toBeVisible();
  });

  test("pause, resume, reboot, reset", async ({ page, api }) => {
    await openVM(page, vm);
    const head = page.locator(".head");
    await vmAction(page, "Power", "Pause");
    await expectBadge(head, "paused");
    expect(await stateOf(api, vm)).toBe("paused");
    await vmAction(page, "Power", "Resume");
    await expectBadge(head, "running");
    await vmAction(page, "Power", "Reboot (Graceful)");
    await confirm(page, "Reboot (Graceful)");
    await vmAction(page, "Power", "Reset (Hard Reboot)");
    await confirm(page, "Reset (Hard Reboot)");
    await expect(page.locator(".error")).toHaveCount(0);
    await expectBadge(head, "running");
    expect(await stateOf(api, vm)).toBe("running");
  });

  test("power on from the detail page, then halt with confirmation", async ({ page, api }) => {
    // the previous tests left the VM running
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("off", async () => (await stateOf(api, vm)) === "shutoff");
    await openVM(page, vm);
    await vmAction(page, "Power", "Power On");
    await expectBadge(page.locator(".head"), "running");
    await vmAction(page, "Power", "Halt (Force Shutdown)");
    // Cancel does nothing
    await cancelConfirm(page, "Halt (Force Shutdown)");
    await page.waitForTimeout(1500);
    expect(await stateOf(api, vm)).toBe("running");
    await vmAction(page, "Power", "Halt (Force Shutdown)");
    await confirm(page, "Halt (Force Shutdown)");
    await expectBadge(page.locator(".head"), "shutoff");
    expect(await stateOf(api, vm)).toBe("shutoff");
  });

  test("list buttons: Power On and Shutdown (graceful) on the row", async ({ page, api }) => {
    await page.goto("/datacenter/vms");
    const r = row(page, NAME);
    await r.getByRole("button", { name: "Power On" }).click();
    await expectBadge(r, "running");
    await expect(r.getByRole("link", { name: "Console" })).toBeVisible();
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await expectBadge(r, "shutoff");
  });

  test("delete from the detail page removes the VM and its disk", async ({ page, api, n1 }) => {
    await openVM(page, vm);
    await vmAction(page, "More", "Delete VM");
    const dlg = dialog(page, `Delete ${NAME}?`);
    await expect(dlg.getByLabel(/Also delete its disk/)).toBeChecked(); // ticked by default
    await dlg.getByRole("button", { name: "Delete VM" }).click();
    await expect(page).toHaveURL(/\/datacenter\/vms$/);
    await expect(row(page, NAME)).toHaveCount(0);
    expect(await api.vmByName(NAME)).toBeUndefined();
    const vols = await api.get(`/nodes/${n1}/pools/default/volumes`);
    expect(vols.some((v: any) => v.name.startsWith(NAME))).toBe(false);
  });
});
