// Dialog behaviour of the simplified UI that no other spec reaches: the Advanced fold opening itself, "Revert anyway"
// after libvirt asks for force, and the Restore dialog's Replace option with its reasons.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, pick } from "../lib/ui";

const NAME = `${P}sd1`;

test.describe.serial("simplified dialogs", () => {
  let vm: { node: number; uuid: string; name: string };
  const base = () => `/nodes/${vm.node}/vms/${vm.uuid}`;

  test.afterAll(async ({ api }) => {
    for (const b of (await api.get("/backups")) as any[]) if (b.manifest?.vm === NAME) await api.del(`/nodes/${b.node}/backups/${b.pool}/${encodeURIComponent(b.dir)}`);
    await cleanVMs(api, NAME);
  });

  test("Advanced opens by itself when a required field inside is empty", async ({ page }) => {
    await page.goto("/storage");
    await pick(page, page.getByRole("combobox").first(), env.node1);
    await page.getByRole("button", { name: "New pool" }).click();
    const dlg = dialog(page, "New storage pool");
    await field(dlg, "Name").fill(`${P}never`);
    const adv = dlg.getByRole("button", { name: "Advanced", exact: true });
    await adv.click();
    await pick(dlg, "Other storage types", /^iSCSI target/);
    await adv.click(); // fold it away again
    await expect(adv).toHaveAttribute("aria-expanded", "false");
    await dlg.getByRole("button", { name: "Create" }).click();
    // the browser stops the submit at the empty iSCSI server field: Advanced is open and the field has focus
    await expect(adv).toHaveAttribute("aria-expanded", "true");
    await expect(field(dlg, "iSCSI server")).toBeFocused();
    await page.keyboard.press("Escape");
    expect((await api(page)).some((p: any) => p.name === `${P}never`)).toBe(false);
  });
  const api = async (page: any) => (await page.request.get(`/api/nodes/${await nodeId(page)}/pools`)).json();
  const nodeId = async (page: any) => ((await (await page.request.get("/api/nodes")).json()).nodes as any[]).find((n) => n.hostname === env.node1 || n.addr === env.node1).id;

  // The lab cannot make libvirt ask for force (that needs a hostdev or CPU change that cannot be made safely), so the first
  // answer is the one libvirt 11 gives ("revert requires force: ..."), injected at the browser; the second call goes through.
  test("Revert asks for force only after libvirt refuses, then offers Revert anyway", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    vm = await createCirros(api, n1, NAME);
    await api.post(`${base()}/snapshots`, { name: "before" });
    const bodies: string[] = [];
    await page.route(/\/snapshots\/before\/revert$/, async (route) => {
      bodies.push(route.request().postData() ?? "");
      if (bodies.length === 1) return route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "revert requires force: the VM's configuration changed since the snapshot" }) });
      return route.continue();
    });
    await openVM(page, vm, "snapshots");
    await page.getByRole("treeitem", { name: /^before(,|$)/ }).getByRole("button", { name: "Revert to before" }).click();
    const dlg = dialog(page, "Revert to before");
    await expect(dlg.getByRole("button", { name: "Revert anyway" })).toHaveCount(0);
    await expect(dlg.locator("label.check")).toHaveCount(0);
    await dlg.getByRole("button", { name: "Revert", exact: true }).click();
    await expect(dlg.locator(".error")).toContainText("revert requires force");
    const anyway = dlg.getByRole("button", { name: "Revert anyway" });
    await expect(anyway).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Revert", exact: true })).toHaveCount(0);
    await anyway.click();
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    expect(JSON.parse(bodies[0]).force).toBe(false);
    expect(JSON.parse(bodies[1]).force).toBe(true);
    // any other refusal does not offer force
    bodies.length = 0;
    await page.unroute(/\/snapshots\/before\/revert$/);
    await page.route(/\/snapshots\/before\/revert$/, (route) => route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "the VM is busy" }) }));
    await page.getByRole("treeitem", { name: /^before(,|$)/ }).getByRole("button", { name: "Revert to before" }).click();
    await dlg.getByRole("button", { name: "Revert", exact: true }).click();
    await expect(dlg.locator(".error")).toContainText("the VM is busy");
    await expect(dlg.getByRole("button", { name: "Revert anyway" })).toHaveCount(0);
    await page.keyboard.press("Escape");
  });

  test("Restore: Replace is offered for a shut-off VM, refused with a reason while it runs or once it is gone", async ({ page, api }) => {
    test.setTimeout(300_000);
    const t = await api.post(`${base()}/backup`, { pool: "default" });
    await api.waitFor("backup task", async () => (await api.get(`/tasks`)).find((x: any) => x.id === t.task && x.status !== "running" && x.status !== "queued")?.status === "ok", 200_000);
    // running: the Replace entry is disabled and the hint says why
    if ((await api.vmDetail(vm.node, vm.uuid)).state !== "running") await api.post(`${base()}/action`, { action: "start" });
    await api.waitFor("running", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "running");
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Restore" }).click();
    const dlg = dialog(page, `Restore ${NAME}`);
    await dlg.getByRole("combobox", { name: "Restore", exact: true }).click();
    const opt = page.getByRole("listbox").getByRole("option", { name: `Replace ${NAME}` });
    await expect(opt).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("Escape");
    await expect(dlg).toContainText(`Replace is not possible while ${NAME} is running`);
    await page.keyboard.press("Escape");
    // shut off: Replace works, after a confirmation that names the VM
    await api.post(`${base()}/action`, { action: "poweroff" });
    await api.waitFor("off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff");
    await openVM(page, vm, "backups");
    await page.locator("tbody tr").first().getByRole("button", { name: "Restore" }).click();
    await pick(dlg, "Restore", `Replace ${NAME}`);
    await expect(dlg.getByLabel("New VM name")).toHaveCount(0);
    await dlg.getByLabel("Start the VM afterwards").uncheck();
    await dlg.getByRole("button", { name: "Restore", exact: true }).click();
    const ask = dialog(page, `Replace ${NAME}`);
    await expect(ask).toContainText("deleted together with its disks");
    await ask.getByRole("button", { name: "Replace VM" }).click();
    await expect(dlg).toContainText(/ok/i, { timeout: 200_000 });
    const all = (await api.vms()).filter((v: any) => v.name === NAME);
    expect(all).toHaveLength(1);
    expect(all[0].uuid).toBe(vm.uuid); // the same identity comes back
    // gone: the entry is disabled and says so
    await dlg.getByRole("button", { name: "Close dialog" }).click();
    await cleanVMs(api, NAME);
    await page.goto("/backups");
    await page.locator("tbody tr").filter({ hasText: NAME }).first().getByRole("button", { name: "Restore" }).click();
    const d2 = dialog(page, `Restore ${NAME}`);
    await d2.getByRole("combobox", { name: "Restore", exact: true }).click();
    await expect(page.getByRole("listbox").getByRole("option", { name: `Replace ${NAME}` })).toHaveAttribute("aria-disabled", "true");
    await page.keyboard.press("Escape");
    await expect(d2).toContainText(`Replace is not possible: ${NAME} no longer exists.`);
    await page.keyboard.press("Escape");
  });
});
