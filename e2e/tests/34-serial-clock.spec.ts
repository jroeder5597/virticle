// Serial console (always added at creation, no Hardware card: the Console tab offers to add one when a VM has none)
// and the guest clock (time sync through the guest agent from the Actions menu, automatic sync after resume).
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, pick, vmAction } from "../lib/ui";
import { guestSh, labAvailable } from "../lib/lab";

const SER = `${P}ser1`;
const CLK = `${P}clk1`;
const AGENT_WAIT = 7 * 60_000;
const card = (page: any, title: string) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: title, exact: true }) });

test.describe.serial("serial console", () => {
  let vm: { node: number; uuid: string; name: string };
  const detail = (api: any) => api.vmDetail(vm.node, vm.uuid);

  test.afterAll(async ({ api }) => cleanVMs(api, SER));

  test("New VM form has no Serial console box; the VM gets a pty serial anyway", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await expect(dlg.getByRole("checkbox", { name: "Serial console" })).toHaveCount(0);
    await field(dlg, "Name").fill(SER);
    await pick(dlg, field(dlg, "Node"), env.node1);
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await pick(dlg, "Base image", new RegExp("^" + env.cirros.replace(/\./g, "\\.")));
    await field(dlg, "Disk size (GiB)").fill("1");
    await dlg.getByRole("checkbox", { name: "Start after creating" }).uncheck();
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    await expect(page.getByRole("heading", { level: 1, name: SER })).toBeVisible();
    const got = await api.vmByName(SER);
    vm = { node: got.node, uuid: got.uuid, name: SER };
    const d = await detail(api);
    expect(d.serials).toEqual([{ type: "pty", port: "0" }]);
    expect(d.hasSerial).toBe(true);
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

  test("the Hardware tab and Edit Settings have no Serial console or Clock section", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await expect(card(page, "CPU and memory")).toBeVisible();
    await expect(card(page, "Serial console")).toHaveCount(0);
    await expect(card(page, "Clock")).toHaveCount(0);
    await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
    await page.getByRole("menuitem", { name: "Edit Settings", exact: true }).click();
    await expect(page.getByRole("menuitem", { name: "Serial console", exact: true })).toHaveCount(0);
    await expect(page.getByRole("menuitem", { name: "Clock", exact: true })).toHaveCount(0);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  });

  test("Console tab with no console at all offers Add serial console", async ({ page, api }) => {
    // take the serial console and the VNC display away through the API, so that nothing is left to show
    await api.del(`/nodes/${vm.node}/vms/${vm.uuid}/serial-console`);
    const d = await detail(api);
    const xml = d.xml.replace(/<graphics[^>]*\/>|<graphics[\s\S]*?<\/graphics>/, "");
    expect(xml).not.toContain("<graphics");
    await api.put(`/nodes/${vm.node}/vms/${vm.uuid}/xml`, { xml });
    expect((await detail(api)).serials).toEqual([]);
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
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/serial-console`);
    const d = await detail(api);
    expect(d.serials).toEqual([]);
    expect(d.serialPending).toBe("add");
    expect(d.hasSerial).toBe(false);
    await openVM(page, vm, "console");
    await expect(page.locator(".empty")).toContainText("A serial console is saved. It applies after the VM is powered off and started.");
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

  test("a running VM: removing the serial console is saved and applies after a power cycle", async ({ api }) => {
    test.setTimeout(180_000);
    await api.del(`/nodes/${vm.node}/vms/${vm.uuid}/serial-console`);
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
      cloudInit: { user: "e2e", password: "e2e-guest-pass-1", packages: ["util-linux-extra"] }, // the guest agent comes by default
    });
    vm = { node: n1, uuid: r.uuid, name: CLK };
    await api.waitFor("guest agent", async () => (await api.vmDetail(vm.node, vm.uuid)).agent === "connected", AGENT_WAIT, 5000);
    expect((await api.vmDetail(vm.node, vm.uuid)).xml).toMatch(/<timer name='kvmclock' present='yes'\/>/);
  });

  test("a guest clock in sync reads as in sync", async ({ api }) => {
    test.setTimeout(300_000);
    // hwclock arrives with cloud-init's package list; wait for the guest to be done installing
    await api.waitFor("hwclock", async () => guestSh("n1", CLK, "command -v hwclock").includes("hwclock"), 180_000, 5000);
    await guestSh("n1", CLK, "systemctl start systemd-timesyncd; true");
    expect((await api.get(`${base()}/clock`)).offset).toBe("utc");
    await expect.poll(async () => Math.abs(await skew(api)), { timeout: 60_000 }).toBeLessThan(2000);
  });

  test("the Hardware tab has no Clock card", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await expect(card(page, "CPU and memory")).toBeVisible();
    await expect(card(page, "Clock")).toHaveCount(0);
  });

  test("a wrong guest clock: Actions, Power, Sync guest clock fixes it", async ({ page, api }) => {
    wrong(300);
    expect(await skew(api)).toBeLessThan(-250_000);
    await openVM(page, vm);
    await vmAction(page, "Power", "Sync guest clock");
    await expect.poll(async () => Math.abs(await skew(api)), { timeout: 60_000, intervals: [1500] }).toBeLessThan(2000);
    const tasks: any[] = await api.get("/tasks");
    expect(tasks.some((t) => t.kind === "sync guest clock" && t.target === CLK && t.status === "ok")).toBe(true);
  });

  test("pause and resume sync the clock automatically", async ({ page, api }) => {
    wrong(300);
    expect(await skew(api)).toBeLessThan(-250_000);
    await openVM(page, vm);
    await vmAction(page, "Power", "Pause");
    await api.waitFor("paused", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "paused", 20_000, 500);
    await vmAction(page, "Power", "Resume");
    await expect.poll(async () => Math.abs(await skew(api)), { timeout: 60_000, intervals: [1500] }).toBeLessThan(2000);
  });

  test("a VM that has the switch off (set through the API) keeps it: resume leaves the clock alone", async ({ api }) => {
    await api.put(`${base()}/clock`, { sync: false });
    expect((await api.get(`${base()}/clock`)).sync).toBe(false);
    wrong(300);
    await api.post(`${base()}/action`, { action: "pause" });
    await api.post(`${base()}/action`, { action: "resume" });
    await new Promise((r) => setTimeout(r, 12_000));
    expect(await skew(api)).toBeLessThan(-250_000);
    await api.put(`${base()}/clock`, { sync: true });
    await api.post(`${base()}/clock/sync`);
    expect(Math.abs(await skew(api))).toBeLessThan(2000);
  });

  test("a shut-off VM: the sync action is disabled with its reason", async ({ page, api }) => {
    await api.post(`${base()}/action`, { action: "poweroff" });
    await api.waitFor("vm off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 1000);
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
    await page.getByRole("menuitem", { name: "Power", exact: true }).click();
    const item = page.getByRole("menuitem", { name: "Sync guest clock", exact: true });
    await expect(item).toBeDisabled();
    await expect(item).toHaveAttribute("title", "The VM is shut off");
    await page.keyboard.press("Escape");
  });
});
