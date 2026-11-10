// Creating a VM through the form, its consoles, and every lifecycle button.
import { cleanVMs, expect, openVM, stateOf, tab, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row } from "../lib/ui";

const NAME = `${P}life1`;

test.describe.serial("create from a disk image and drive the lifecycle", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  test("New VM form: disk image, BIOS, start", async ({ page, api, n1 }) => {
    await page.goto("/");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await expect(dlg).toBeVisible();
    // initial focus is in the name field
    await expect(field(dlg, "Name")).toBeFocused();
    await field(dlg, "Name").fill(NAME);
    await field(dlg, "Node").selectOption({ label: env.node1 });
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await field(dlg, "Base image").selectOption({ label: new RegExp(env.cirros.replace(".", "\\.")) as any }).catch(async () => {
      // selectOption does not take a regex: pick the option by its text
      const opt = dlg.locator("option", { hasText: env.cirros });
      await field(dlg, "Base image").selectOption(await opt.first().getAttribute("value"));
    });
    await field(dlg, "Disk size (GiB)").fill("1");
    await expect(field(dlg, "Firmware")).toHaveValue("bios");
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
    await page.goto("/");
    const r = row(page, NAME);
    await expect(r).toBeVisible();
    await expectBadge(r, "running");
    await expect(r.getByRole("cell", { name: env.node1 })).toBeVisible();
    await r.getByRole("link", { name: NAME }).click();
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();
    const summary = page.locator(".card", { hasText: "Summary" }).first();
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
    autoDialogs(page);
    await openVM(page, vm);
    const head = page.locator(".head");
    await head.getByRole("button", { name: "Pause" }).click();
    await expectBadge(head, "paused");
    expect(await stateOf(api, vm)).toBe("paused");
    await head.getByRole("button", { name: "Resume" }).click();
    await expectBadge(head, "running");
    await head.getByRole("button", { name: "Reboot" }).click();
    await head.getByRole("button", { name: "Reset" }).click();
    await expect(page.locator(".error")).toHaveCount(0);
    await expectBadge(head, "running");
    expect(await stateOf(api, vm)).toBe("running");
  });

  test("shutdown menu: force off after a timeout when the guest ignores ACPI", async ({ page, api }) => {
    await openVM(page, vm);
    // The menu offers 60 s; shorten it so the test does not wait a minute.
    let body: any;
    await page.route("**/api/nodes/*/vms/*/action", async (route) => {
      const req = route.request().postDataJSON();
      if (req.action === "shutdown" && req.force) {
        body = req;
        req.timeoutSec = 6;
        await route.continue({ postData: JSON.stringify(req) });
      } else await route.continue();
    });
    await page.getByRole("button", { name: "More shutdown options" }).click();
    await expect(page.getByRole("menu")).toBeVisible();
    await page.getByRole("menuitem", { name: /force off after 60 s/ }).click();
    expect(await expect.poll(() => body).toMatchObject({ action: "shutdown", force: true }));
    await expectBadge(page.locator(".head"), "shutoff");
    expect(await stateOf(api, vm)).toBe("shutoff");
    await page.unroute("**/api/nodes/*/vms/*/action");
  });

  test("start from the detail page, then force off with confirmation", async ({ page, api }) => {
    const seen = autoDialogs(page);
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Start" }).click();
    await expectBadge(page.locator(".head"), "running");
    await page.locator(".head").getByRole("button", { name: "Force off" }).click();
    await expectBadge(page.locator(".head"), "shutoff");
    expect(seen.some((m) => /Force .* off/.test(m))).toBe(true);
    expect(await stateOf(api, vm)).toBe("shutoff");
  });

  test("list buttons: Start and Shut down on the row", async ({ page, api }) => {
    await page.goto("/");
    const r = row(page, NAME);
    await r.getByRole("button", { name: "Start" }).click();
    await expectBadge(r, "running");
    await expect(r.getByRole("link", { name: "Console" })).toBeVisible();
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await expectBadge(r, "shutoff");
  });

  test("delete from the detail page removes the VM and its disk", async ({ page, api, n1 }) => {
    await openVM(page, vm);
    await page.locator(".head").getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, `Delete ${NAME}?`);
    await dlg.getByLabel(/Also delete its disk/).check();
    await dlg.getByRole("button", { name: "Delete" }).click();
    await expect(page).toHaveURL(/\/$/);
    await expect(row(page, NAME)).toHaveCount(0);
    expect(await api.vmByName(NAME)).toBeUndefined();
    const vols = await api.get(`/nodes/${n1}/pools/default/volumes`);
    expect(vols.some((v: any) => v.name.startsWith(NAME))).toBe(false);
  });
});
