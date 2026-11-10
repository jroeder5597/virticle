// A real distro image: cloud-init create, guest agent, guest card, charts, seal into a template, linked clone that boots.
import { cleanVMs, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, expectBadge, field } from "../lib/ui";

const NAME = `${P}deb1`;
const CLONE = `${P}deb1-clone`;
const USER = "e2e";
const PASS = "e2e-guest-pass-1";
const AGENT_WAIT = 6 * 60_000; // first boot installs the agent with apt on a nested 512 MB guest: about 4 minutes

test.describe.serial("debian cloud image, guest agent, template and linked clone", () => {
  let vm: { node: number; uuid: string; name: string };
  let clone: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, CLONE, NAME));

  test("create from the cloud image with cloud-init through the form", async ({ page, api, n1 }) => {
    test.setTimeout(180_000);
    const vols: any[] = await api.get(`/nodes/${n1}/pools/default/volumes`);
    const img = vols.find((v) => v.name === env.debian);
    expect(img, `${env.debian} must exist in pool default on node 1`).toBeTruthy();
    await page.goto("/");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await field(dlg, "Name").fill(NAME);
    await field(dlg, "Node").selectOption({ label: env.node1 });
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("512");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    const opt = dlg.locator("option", { hasText: env.debian });
    await field(dlg, "Base image").selectOption((await opt.first().getAttribute("value"))!);
    await field(dlg, "Disk size (GiB)").fill(String(Math.ceil(img.capacity / 2 ** 30)));
    await dlg.locator("summary", { hasText: "Cloud-init" }).click();
    await field(dlg, "User").fill(USER);
    await field(dlg, "Password").fill(PASS);
    await field(dlg, "Packages").fill("qemu-guest-agent");
    await expect(dlg.locator("summary")).toContainText("configured");
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden({ timeout: 120_000 });
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();
    const got = await api.vmByName(NAME);
    vm = { node: got.node, uuid: got.uuid, name: NAME };
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.state).toBe("running");
    expect(d.memoryKiB).toBe(512 * 1024);
    expect(d.disks.some((k: any) => k.device === "cdrom" && /seed/.test(k.source ?? ""))).toBe(true);
  });

  test("the guest agent connects and the Guest card shows OS and filesystems", async ({ page, api }) => {
    test.setTimeout(AGENT_WAIT + 60_000);
    await openVM(page, vm);
    const agentCard = page.locator("section.card", { hasText: "Guest agent" });
    await expect(agentCard.locator(".badge")).toHaveText("connected", { timeout: AGENT_WAIT });
    expect((await api.vmDetail(vm.node, vm.uuid)).agent).toBe("connected");
    const guest = page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Guest", exact: true }) });
    await expect(guest).toContainText(/Debian/i, { timeout: 60_000 });
    await expect(guest.getByText("Filesystems")).toBeVisible();
    await expect(guest.locator(".fsbar .mono").first()).toHaveText("/");
    await expect(guest).toContainText(/Host name/);
    // the NIC row shows the address the agent reported
    await expect(page.locator("section.card", { hasText: "Network interfaces" })).toContainText(/\d+\.\d+\.\d+\.\d+/, { timeout: 60_000 });
  });

  test("usage charts render with data", async ({ page, api }) => {
    await openVM(page, vm);
    const charts = page.locator(".charts .chart");
    await expect(charts).toHaveCount(4);
    for (const t of ["CPU", "Memory", "Disk I/O", "Network"]) await expect(charts.filter({ hasText: t }).first()).toBeVisible();
    const mem = charts.filter({ hasText: "Memory" }).locator(".chart-val");
    await expect(mem).toContainText(/\d/, { timeout: 30_000 });
    const text = await mem.innerText();
    expect(text).toMatch(/\d+ MiB of (4[5-9]\d|5[01]\d) MiB/); // guest-reported total is a bit under 512
    expect(text).not.toMatch(/^0 /);
    await expect.poll(async () => charts.first().locator("svg path[d]").count(), { timeout: 30_000 }).toBeGreaterThan(0);
    const stats = await api.get(`/nodes/${vm.node}/vms/${vm.uuid}/stats`);
    expect(stats.points.length).toBeGreaterThan(1);
    expect(stats.points.some((p: any) => p.memUsedKiB > 0)).toBe(true);
    expect(stats.points.some((p: any) => p.cpu > 0)).toBe(true);
  });

  test("seal and convert to a template through the dialog", async ({ page, api }) => {
    test.setTimeout(240_000);
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Seal and convert to template" }).click();
    const dlg = dialog(page, `Seal ${NAME}`);
    await expect(dlg).toContainText("machine-id");
    await dlg.getByRole("button", { name: "Seal and convert" }).click();
    await expect(dlg).toBeHidden({ timeout: 180_000 });
    await expectBadge(page.locator(".head"), "shutoff");
    await expect(page.locator(".head .badge", { hasText: "Template" })).toBeVisible();
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.template).toBe(true);
    expect(d.sealed).toBe("linux");
    expect(d.state).toBe("shutoff");
    // a template cannot be started
    await expect(page.locator(".head").getByRole("button", { name: "Start" })).toHaveCount(0);
  });

  test("linked clone with credentials through the Clone dialog", async ({ page, api }) => {
    test.setTimeout(120_000);
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Clone", exact: true }).click();
    const dlg = dialog(page, `Clone ${NAME}`);
    await expect(field(dlg, "Name of the clone")).toBeFocused();
    await field(dlg, "Name of the clone").fill(CLONE);
    await dlg.getByLabel(/Linked clone/).check();
    await dlg.locator("summary", { hasText: "Cloud-init" }).click();
    await field(dlg, "User").fill(USER);
    await field(dlg, "Password").fill(PASS);
    await dlg.getByLabel(/Start the clone when it is ready/).check();
    await dlg.getByRole("button", { name: "Clone", exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: CLONE })).toBeVisible({ timeout: 90_000 });
    const got = await api.vmByName(CLONE);
    clone = { node: got.node, uuid: got.uuid, name: CLONE };
    const d = await api.vmDetail(clone.node, clone.uuid);
    expect(d.template).toBeFalsy();
    // the disk is an overlay on the template's disk
    const xml: string = d.xml;
    expect(xml).toContain("backingStore");
    expect(d.disks.some((k: any) => k.device === "cdrom" && /seed/.test(k.source ?? ""))).toBe(true);
  });

  test("the clone boots and its guest agent connects", async ({ page, api }) => {
    test.setTimeout(AGENT_WAIT + 60_000);
    await openVM(page, clone);
    await expectBadge(page.locator(".head"), "running");
    const agentCard = page.locator("section.card", { hasText: "Guest agent" });
    await expect(agentCard.locator(".badge")).toHaveText("connected", { timeout: AGENT_WAIT });
    await expect(page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Guest", exact: true }) })).toContainText(/Debian/i, { timeout: 60_000 });
  });

  test("delete the clone and its disks, then the template", async ({ page, api }) => {
    // stop the clone, delete it with its disks, then the template
    await api.post(`/nodes/${clone.node}/vms/${clone.uuid}/action`, { action: "poweroff" });
    await api.waitFor("clone off", async () => (await api.vmDetail(clone.node, clone.uuid)).state === "shutoff");
    await openVM(page, clone);
    await page.locator(".head").getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, `Delete ${CLONE}?`);
    await dlg.getByLabel(/Also delete its disk/).check();
    await dlg.getByRole("button", { name: "Delete" }).click();
    await expect(page).toHaveURL(/\/$/);
    expect(await api.vmByName(CLONE)).toBeUndefined();
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Convert back to VM" }).click();
    await expect(page.locator(".head").getByRole("button", { name: "Delete" })).toBeVisible();
    await page.locator(".head").getByRole("button", { name: "Delete" }).click();
    const d2 = dialog(page, `Delete ${NAME}?`);
    await d2.getByLabel(/Also delete its disk/).check();
    await d2.getByRole("button", { name: "Delete" }).click();
    await expect(page).toHaveURL(/\/$/);
    expect(await api.vmByName(NAME)).toBeUndefined();
  });
});
