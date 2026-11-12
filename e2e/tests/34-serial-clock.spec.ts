// Serial console as a hardware item (New VM, Hardware tab, Edit Settings, Console tab) and the guest clock
// (offset, time sync through the guest agent, automatic sync after resume).
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { cancelConfirm, confirm, dialog, field, pick, vmAction } from "../lib/ui";
import { guestSh, labAvailable } from "../lib/lab";

const SER = `${P}ser1`;
const CLK = `${P}clk1`;
const AGENT_WAIT = 7 * 60_000;
const card = (page: any, title: string) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: title, exact: true }) });

test.describe.serial("serial console as hardware", () => {
  let vm: { node: number; uuid: string; name: string };
  const detail = (api: any) => api.vmDetail(vm.node, vm.uuid);

  test.afterAll(async ({ api }) => cleanVMs(api, SER));

  test("New VM form: Serial console is ticked by default; unticked, the VM has none", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    const box = dlg.getByRole("checkbox", { name: "Serial console" });
    await expect(box).toBeChecked();
    await field(dlg, "Name").fill(SER);
    await pick(dlg, field(dlg, "Node"), env.node1);
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await pick(dlg, "Base image", new RegExp("^" + env.cirros.replace(/\./g, "\\.")));
    await field(dlg, "Disk size (GiB)").fill("1");
    await dlg.getByRole("checkbox", { name: "Start after creating" }).uncheck();
    await box.uncheck();
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    await expect(page.getByRole("heading", { level: 1, name: SER })).toBeVisible();
    const got = await api.vmByName(SER);
    vm = { node: got.node, uuid: got.uuid, name: SER };
    const d = await detail(api);
    expect(d.serials).toEqual([]);
    expect(d.hasSerial).toBe(false);
    expect(d.xml).not.toContain("<serial");
    expect(n1).toBe(vm.node);
  });

  test("a default VM made through the API has a pty serial and the explicit KVM timers", async ({ api, n1 }) => {
    const other = await createCirros(api, n1, `${P}ser2`);
    try {
      const d = await api.vmDetail(other.node, other.uuid);
      expect(d.serials).toEqual([{ type: "pty", port: "0" }]);
      expect(d.hasSerial).toBe(true);
      expect(d.xml).toMatch(/<timer name='rtc' tickpolicy='catchup'\/>/);
      expect(d.xml).toMatch(/<timer name='pit' tickpolicy='delay'\/>/);
      expect(d.xml).toMatch(/<timer name='hpet' present='no'\/>/);
      expect(d.xml).toMatch(/<timer name='kvmclock' present='yes'\/>/);
    } finally {
      await cleanVMs(api, `${P}ser2`);
    }
  });

  test("Hardware tab: add a serial console to the stopped VM", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Serial console");
    await expect(c).toContainText("None");
    await c.getByRole("button", { name: "Add serial console" }).click();
    await expect(page.getByText("Saved.", { exact: true })).toBeVisible();
    await expect(c).toContainText("pty (used by the Console tab), port 0");
    await expect(c.getByRole("button", { name: "Add serial console" })).toBeDisabled();
    const d = await detail(api);
    expect(d.serials).toEqual([{ type: "pty", port: "0" }]);
    expect(d.serialPending).toBeUndefined();
    expect(d.xml).toMatch(/<console type='pty'>\s*<target type='serial' port='0'\/>/);
  });

  test("Edit Settings shows the same section; Remove asks first", async ({ page, api }) => {
    await openVM(page, vm);
    await vmAction(page, "Edit Settings", "Serial console");
    const dlg = dialog(page, "Serial console");
    await expect(dlg).toContainText("pty (used by the Console tab), port 0");
    await dlg.getByRole("button", { name: "Remove", exact: true }).click();
    await cancelConfirm(page, "Remove serial console");
    expect((await detail(api)).serials).toHaveLength(1);
    await dlg.getByRole("button", { name: "Remove", exact: true }).click();
    await confirm(page, "Remove serial console");
    await expect(dlg).toContainText("None");
    expect((await detail(api)).serials).toEqual([]);
    expect((await detail(api)).xml).not.toContain("<console");
  });

  test("Console tab with no console at all offers Add serial console", async ({ page, api }) => {
    // take the VNC display away through the XML tab's API, so that nothing is left to show
    const d = await detail(api);
    const xml = d.xml.replace(/<graphics[^>]*\/>|<graphics[\s\S]*?<\/graphics>/, "");
    expect(xml).not.toContain("<graphics");
    await api.put(`/nodes/${vm.node}/vms/${vm.uuid}/xml`, { xml });
    await openVM(page, vm, "console");
    await expect(page.locator(".empty")).toContainText("neither a VNC display nor a serial console");
    await page.getByRole("button", { name: "Add serial console" }).click();
    await expect(page.locator(".empty")).toContainText("Start the VM to open its console.");
    expect((await detail(api)).serials).toEqual([{ type: "pty", port: "0" }]);
  });

  test("a running VM: libvirt cannot hot-plug the q35 ISA serial, so the change waits for a power cycle", async ({ page, api }) => {
    test.setTimeout(240_000);
    // remove it while stopped, start, then add it to the running VM
    await api.del(`/nodes/${vm.node}/vms/${vm.uuid}/serial-console`);
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    await openVM(page, vm, "hardware");
    const c = card(page, "Serial console");
    await expect(c).toContainText("None");
    await c.getByRole("button", { name: "Add serial console" }).click();
    await expect(page.getByRole("status").filter({ hasText: /Takes effect after a restart/ })).toContainText(/powered off and started/);
    await expect(c).toContainText("Saved: a serial console is added after the VM is powered off and started.");
    const d = await detail(api);
    expect(d.serials).toEqual([]);
    expect(d.serialPending).toBe("add");
    expect(d.hasSerial).toBe(false);
    // power cycle: now it is there and the Console tab reaches it
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("vm off", async () => (await detail(api)).state === "shutoff", 30_000, 1000);
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    await api.waitFor("serial live", async () => (await detail(api)).serials.length === 1, 30_000, 1000);
    expect((await detail(api)).serialPending).toBeUndefined();
    await openVM(page, vm, "console");
    await expect(page.locator(".console .toolbar")).toContainText("Connected", { timeout: 30_000 });
    await expect(page.locator(".xterm")).toBeVisible();
  });

  test("a running VM: Remove is saved and applies after a power cycle", async ({ page, api }) => {
    test.setTimeout(180_000);
    await openVM(page, vm, "hardware");
    const c = card(page, "Serial console");
    await c.getByRole("button", { name: "Remove", exact: true }).click();
    await confirm(page, "Remove serial console");
    await expect(c).toContainText("Saved: the serial console is removed after the VM is powered off and started.");
    const d = await detail(api);
    expect(d.serialPending).toBe("remove");
    expect(d.serials).toHaveLength(1); // still running with it
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("vm off", async () => (await detail(api)).state === "shutoff", 30_000, 1000);
    expect((await detail(api)).serials).toEqual([]);
  });
});

test.describe.serial("guest clock", () => {
  let vm: { node: number; uuid: string; name: string };
  const base = () => `/nodes/${vm.node}/vms/${vm.uuid}`;
  const wrong = (secs: number) => guestSh("n1", CLK, `systemctl stop systemd-timesyncd; date -s @$(( $(date +%s) - ${secs} )) >/dev/null`);
  const skew = async (api: any) => (await api.get(`${base()}/clock`)).skewMs as number;

  test.beforeEach(async () => {
    test.skip(!labAvailable(), "needs the lab ssh access (SSHPASS) to change the guest's clock");
  });
  test.afterAll(async ({ api }) => cleanVMs(api, CLK));

  test("create a Debian VM with the guest agent", async ({ api, n1 }) => {
    test.setTimeout(AGENT_WAIT + 120_000);
    const r = await api.post(`/nodes/${n1}/vms`, {
      name: CLK,
      vcpus: 1,
      memoryMiB: 512,
      firmware: "bios",
      pool: "default",
      diskGiB: 3,
      baseVolume: env.debian,
      network: "default",
      start: true,
      cloudInit: { user: "e2e", password: "e2e-guest-pass-1", packages: ["qemu-guest-agent", "util-linux-extra"] },
    });
    vm = { node: n1, uuid: r.uuid, name: CLK };
    await api.waitFor("guest agent", async () => (await api.vmDetail(vm.node, vm.uuid)).agent === "connected", AGENT_WAIT, 5000);
    expect((await api.vmDetail(vm.node, vm.uuid)).xml).toMatch(/<timer name='kvmclock' present='yes'\/>/);
  });

  test("Clock area: UTC, and a guest clock in sync shows a green dot", async ({ page, api }) => {
    test.setTimeout(300_000);
    // hwclock arrives with cloud-init's package list; wait for the guest to be done installing
    await api.waitFor("hwclock", async () => guestSh("n1", CLK, "command -v hwclock").includes("hwclock"), 180_000, 5000);
    await guestSh("n1", CLK, "systemctl start systemd-timesyncd; true");
    await openVM(page, vm, "hardware");
    const c = card(page, "Clock");
    await expect(c.getByRole("combobox", { name: "Clock offset" })).toContainText("UTC");
    await expect(c.getByText(/^Guest clock:/)).toBeVisible();
    await expect.poll(async () => Math.abs(await skew(api)), { timeout: 60_000 }).toBeLessThan(2000);
    await c.getByRole("button", { name: "Read clock" }).click();
    await expect(c.locator(".statusline").filter({ hasText: "Guest clock" }).locator(".cdot")).toHaveClass(/c-ok/);
  });

  test("a wrong guest clock shows red; Sync guest clock turns it green", async ({ page, api }) => {
    wrong(300);
    await openVM(page, vm, "hardware");
    const c = card(page, "Clock");
    const line = c.locator(".statusline").filter({ hasText: "Guest clock" });
    await expect(line).toContainText("5 min behind");
    await expect(line.locator(".cdot")).toHaveClass(/c-error/);
    await c.getByRole("button", { name: "Sync guest clock" }).click();
    await expect(line.locator(".cdot")).toHaveClass(/c-ok/);
    await expect(line).toContainText(/in sync|s (behind|ahead)/);
    expect(Math.abs(await skew(api))).toBeLessThan(2000);
  });

  test("40 s off is yellow; Actions, Guest, Sync guest clock fixes it", async ({ page, api }) => {
    wrong(40);
    await openVM(page, vm, "hardware");
    const c = card(page, "Clock");
    const line = c.locator(".statusline").filter({ hasText: "Guest clock" });
    await expect(line.locator(".cdot")).toHaveClass(/c-warning/);
    await vmAction(page, "Guest", "Sync guest clock");
    await c.getByRole("button", { name: "Read clock" }).click();
    await expect(line.locator(".cdot")).toHaveClass(/c-ok/);
    expect(Math.abs(await skew(api))).toBeLessThan(2000);
  });

  test("pause and resume sync the clock automatically", async ({ page, api }) => {
    wrong(300);
    expect(await skew(api)).toBeLessThan(-250_000);
    await openVM(page, vm);
    await vmAction(page, "Power", "Pause");
    await api.waitFor("paused", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "paused", 20_000, 500);
    await vmAction(page, "Power", "Resume");
    await expect.poll(async () => Math.abs(await skew(api)), { timeout: 60_000, intervals: [1500] }).toBeLessThan(2000);
    const tasks: any[] = await api.get("/tasks");
    expect(tasks.some((t) => t.kind === "sync guest clock" && t.target === CLK && t.status === "ok")).toBe(true);
  });

  test("with the switch off, resume leaves the clock alone", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Clock");
    const sw = c.getByRole("checkbox", { name: "Sync clock after resume, revert and migration" });
    await expect(sw).toBeChecked();
    await sw.uncheck();
    await expect.poll(async () => (await api.get(`${base()}/clock`)).sync).toBe(false);
    wrong(300);
    await api.post(`${base()}/action`, { action: "pause" });
    await api.post(`${base()}/action`, { action: "resume" });
    await new Promise((r) => setTimeout(r, 12_000));
    expect(await skew(api)).toBeLessThan(-250_000);
    await sw.check();
    await expect.poll(async () => (await api.get(`${base()}/clock`)).sync).toBe(true);
    await api.post(`${base()}/clock/sync`);
    expect(Math.abs(await skew(api))).toBeLessThan(2000);
  });

  test("the offset is saved and applies at the next cold start", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page, "Clock");
    await expect(c.getByRole("button", { name: "Save" })).toBeDisabled();
    await pick(c, "Clock offset", "Local time");
    await c.getByRole("button", { name: "Save" }).click();
    await expect(page.getByRole("status").filter({ hasText: /Takes effect after a restart/ })).toContainText(/offset applies after the VM is powered off and started/);
    const info = await api.get(`${base()}/clock`);
    expect([info.saved, info.offset]).toEqual(["localtime", "utc"]);
    await expect(c).toContainText("Saved offset is Local time; the running VM still uses UTC.");
    await pick(c, "Clock offset", "UTC");
    await c.getByRole("button", { name: "Save" }).click();
    await expect.poll(async () => (await api.get(`${base()}/clock`)).saved).toBe("utc");
  });

  test("a shut-off VM: the sync action is disabled with its reason", async ({ page, api }) => {
    await api.post(`${base()}/action`, { action: "poweroff" });
    await api.waitFor("vm off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 1000);
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
    await page.getByRole("menuitem", { name: "Guest", exact: true }).click();
    const item = page.getByRole("menuitem", { name: "Sync guest clock", exact: true });
    await expect(item).toBeDisabled();
    await expect(item).toHaveAttribute("title", "The VM is shut off");
    await page.keyboard.press("Escape");
  });
});
