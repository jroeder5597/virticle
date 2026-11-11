// Hardware tab: CPU/memory, disks, CD-ROM (with an ISO uploaded through the Storage page), NICs, boot order.
import { cleanVMs, createCirros, expect, openVM, tab, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row, vmAction, pick } from "../lib/ui";

const NAME = `${P}hw1`;
const ISO = `${P}tiny.iso`;

test.describe.serial("hardware editing", () => {
  let vm: { node: number; uuid: string; name: string };
  const card = (page: any, title: string) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: title, exact: true }) });

  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME);
    await api.del(`/nodes/${n1}/pools/default/volumes/${ISO}`).catch(() => {});
  });

  test("upload an ISO-named file on the Storage page", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    await page.goto(`/node/${n1}/storage`); // the node's own storage tab: the unified page lists every node's "default"
    await row(page, /^default/).first().click();
    await expect(page.getByRole("heading", { name: "Volumes in default" })).toBeVisible();
    await page.locator('input[type="file"]').setInputFiles({ name: ISO, mimeType: "application/octet-stream", buffer: Buffer.alloc(64 * 1024, 1) });
    await expect(row(page, ISO)).toBeVisible();
    const vols = await api.get(`/nodes/${n1}/pools/default/volumes`);
    expect(vols.find((v: any) => v.name === ISO)?.capacity).toBe(64 * 1024);
  });

  test("edit vCPUs and memory of the stopped VM", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "CPU and memory");
    await c.getByRole("button", { name: "Edit" }).click();
    await field(c, "Maximum vCPUs").fill("2");
    await field(c, "vCPUs now").fill("2");
    await field(c, "Maximum memory (MiB)").fill("256");
    await field(c, "Memory now (MiB)").fill("192");
    await c.getByRole("button", { name: "Apply" }).click();
    await expect(c.getByRole("button", { name: "Edit" })).toBeVisible();
    await expect(page.getByText("Saved.", { exact: true })).toBeVisible();
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect([d.vcpus, d.maxVcpus, d.currentMemoryKiB, d.memoryKiB]).toEqual([2, 2, 192 * 1024, 256 * 1024]);
    await expect(c).toContainText("192 MiB");
  });

  test("Apply is disabled for an impossible combination", async ({ page }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "CPU and memory");
    await c.getByRole("button", { name: "Edit" }).click();
    await field(c, "vCPUs now").fill("5");
    await expect(c.getByRole("button", { name: "Apply" })).toBeDisabled();
    await c.getByRole("button", { name: "Cancel" }).click();
  });

  test("CD-ROM: add a drive, insert the uploaded ISO, eject it", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "CD-ROM");
    await c.getByRole("button", { name: "Add CD-ROM drive" }).click();
    await expect(c.getByText("(no media)")).toBeVisible();
    await c.getByRole("button", { name: "Insert media" }).click();
    const dlg = dialog(page, /^Media for/);
    await pick(dlg, "ISO image", `default / ${ISO} (64.0 KiB)`);
    await dlg.getByRole("button", { name: "Insert" }).click();
    await expect(dlg).toBeHidden();
    await expect(c).toContainText(ISO);
    let d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.disks.find((k: any) => k.device === "cdrom")?.source).toContain(ISO);
    await c.getByRole("button", { name: "Eject" }).click();
    await expect(c.getByText("(no media)")).toBeVisible();
    d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.disks.find((k: any) => k.device === "cdrom")?.source ?? "").toBe("");
  });

  test("start the VM", async ({ page, api }) => {
    await openVM(page, vm);
    await vmAction(page, "Power", "Power On");
    await expectBadge(page.locator(".head"), "running");
  });

  test("hot-add a new disk volume, resize it, detach and delete it", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Disks");
    await c.getByRole("button", { name: "Add disk" }).click();
    const dlg = dialog(page, "Add disk");
    await field(dlg, "Volume name").fill(`${NAME}-data.qcow2`);
    await field(dlg, "Size (GiB)").fill("1");
    await dlg.getByRole("button", { name: "Add disk" }).click();
    await expect(dlg).toBeHidden();
    await expect(page.getByText("Applied and saved.", { exact: true })).toBeVisible();
    const r = row(c, `${NAME}-data.qcow2`);
    await expect(r).toBeVisible();
    const target = (await r.locator("td.mono").first().innerText()).trim();
    expect(target).toBe("vdb");
    let d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.disks.some((k: any) => k.target === "vdb")).toBe(true);

    // resize by pressing Enter in the field (dialogs without a form still submit)
    await r.getByRole("button", { name: "Resize" }).click();
    const rz = dialog(page, "Resize vdb");
    await expect(field(rz, "New size (GiB)")).toBeFocused();
    await field(rz, "New size (GiB)").fill("2");
    await field(rz, "New size (GiB)").press("Enter");
    await expect(rz).toBeHidden();
    await expect(r).toContainText("2.0 GiB");
    d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.disks.find((k: any) => k.target === "vdb").capacity).toBe(2 * 2 ** 30);

    // The guest (still booting) cannot release a hot-unplugged disk yet: the UI must say so and keep the volume.
    await r.getByRole("button", { name: "Detach" }).click();
    const dt = dialog(page, "Detach vdb?");
    await dt.getByLabel(/Also delete the volume/).check();
    await dt.getByRole("button", { name: "Detach" }).click();
    await expect(dt).toBeHidden();
    await expect(page.getByText(/The guest has not released the disk yet; it disappears once the guest lets go of it, or at the next restart\. The volume was not deleted/)).toBeVisible();
    const vols = await api.get(`/nodes/${vm.node}/pools/default/volumes`);
    expect(vols.some((v: any) => v.name === `${NAME}-data.qcow2`)).toBe(true);
  });

  test("detach on the stopped VM applies at once and deletes the volume", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff");
    await openVM(page, vm, "hardware");
    const c = card(page, "Disks");
    // the pending detach of the previous test took effect with the power-off
    await expect(row(c, `${NAME}-data.qcow2`)).toHaveCount(0);
    await c.getByRole("button", { name: "Add disk" }).click();
    const dlg = dialog(page, "Add disk");
    await field(dlg, "Volume name").fill(`${NAME}-data2.qcow2`);
    await field(dlg, "Size (GiB)").fill("1");
    await dlg.getByRole("button", { name: "Add disk" }).click();
    await expect(dlg).toBeHidden();
    const r = row(c, `${NAME}-data2.qcow2`);
    await expect(r).toBeVisible();
    await r.getByRole("button", { name: "Detach" }).click();
    const dt = dialog(page, /^Detach/);
    await dt.getByLabel(/Also delete the volume/).check();
    await dt.getByRole("button", { name: "Detach" }).click();
    await expect(dt).toBeHidden();
    await expect(r).toHaveCount(0);
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.disks.filter((k: any) => k.device === "disk")).toHaveLength(1);
    const vols = await api.get(`/nodes/${vm.node}/pools/default/volumes`);
    expect(vols.some((v: any) => v.name === `${NAME}-data2.qcow2`)).toBe(false);
    await api.del(`/nodes/${vm.node}/pools/default/volumes/${NAME}-data.qcow2`);
  });

  test("start again for the NIC tests", async ({ page, api }) => {
    await openVM(page, vm);
    await vmAction(page, "Power", "Power On");
    await expectBadge(page.locator(".head"), "running");
  });

  test("hot-add and remove a network interface", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Network interfaces");
    await expect(c.locator("tbody tr")).toHaveCount(1);
    await c.getByRole("button", { name: "Add interface" }).click();
    const dlg = dialog(page, "Add network interface");
    await dlg.getByRole("button", { name: "Add" }).click();
    await expect(dlg).toBeHidden();
    await expect(c.locator("tbody tr")).toHaveCount(2);
    let d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.nics).toHaveLength(2);
    const mac = d.nics[1].mac;
    await row(c, mac).getByRole("button", { name: "Remove" }).click();
    const rm = dialog(page, `Remove ${mac}?`);
    await rm.getByRole("button", { name: "Remove" }).click();
    await expect(rm).toBeHidden();
    // the guest is still booting and cannot release the NIC yet: the UI says so, the removal lands at power-off
    await expect(page.getByText(/The guest has not released the interface yet/)).toBeVisible();
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff");
    await expect(c.locator("tbody tr")).toHaveCount(1);
    d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.nics).toHaveLength(1);
  });

  test("boot order: put the NIC first and save", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Boot order");
    const d0 = await api.vmDetail(vm.node, vm.uuid);
    const mac = d0.nics[0].mac;
    // Legacy lists expand to per-device entries; make sure the NIC is in the order, then move it to the top.
    const nicRow = row(c, mac);
    if (await nicRow.getByRole("button", { name: "Add" }).count()) await nicRow.getByRole("button", { name: "Add" }).click();
    await expect(c.getByRole("button", { name: "Move up" }).first()).toBeVisible();
    const nicInOrder = row(c, mac);
    while (await nicInOrder.getByRole("button", { name: "Move up" }).isEnabled()) await nicInOrder.getByRole("button", { name: "Move up" }).click();
    await c.getByRole("button", { name: "Save boot order" }).click();
    await expect(c.getByRole("button", { name: "Save boot order" })).toBeDisabled();
    await expect.poll(async () => (await api.vmDetail(vm.node, vm.uuid)).nics[0].bootOrder).toBe(1);
  });

});
