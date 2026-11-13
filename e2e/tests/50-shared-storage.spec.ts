// Shared storage over NFS: Datacenter, Storage, "Add shared storage" mounts one NFS share on every node under the
// same name; the table shows it once; the migration pre-flight says no disk is copied; removing is refused while a
// VM has a disk on it; removing works afterwards.
//
// Needs an NFS server the nodes can reach, so it runs only when E2E_NFS_SERVER (name or address) and E2E_NFS_EXPORT
// (exported path) are set, and the export lets root of the nodes write (no_root_squash). Otherwise it skips.
// The share is named after the suite prefix and is removed at the end, files on the server are left alone.
import { cleanVMs, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { confirm, dialog, field, row, vmAction } from "../lib/ui";

const SERVER = process.env.E2E_NFS_SERVER ?? "";
const EXPORT = process.env.E2E_NFS_EXPORT ?? "";
const SHARE = `${P}share`.replace(/[^A-Za-z0-9_-]/g, "-");
const VM = `${P}nfsvm`;

test.describe.serial("shared storage (NFS)", () => {
  test.skip(!SERVER || !EXPORT, "needs an NFS server: set E2E_NFS_SERVER and E2E_NFS_EXPORT");
  let node1 = 0;

  async function removeShare(api: any) {
    await api.del(`/storage/shared/${SHARE}`).catch(() => {});
  }

  test.beforeAll(async ({ api, n1 }) => {
    node1 = n1;
    await cleanVMs(api, VM);
    await removeShare(api);
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
    await removeShare(api);
  });

  test("Add shared storage mounts the share on every node", async ({ page, api }) => {
    await page.goto("/datacenter/storage");
    await page.getByRole("button", { name: "Add shared storage" }).click();
    const dlg = dialog(page, "Add shared storage");
    await field(dlg, "Name").fill(SHARE);
    await field(dlg, "NFS server").fill(SERVER);
    await field(dlg, "Export path").fill(EXPORT);
    // the node choice hides in Advanced, closed by default
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveAttribute("aria-expanded", "false");
    await dlg.getByRole("button", { name: "Add shared storage" }).click();
    await expect(dlg).toBeHidden({ timeout: 120_000 });
    const r = await api.get("/storage/shared");
    const g = r.shared.find((s: any) => s.name === SHARE);
    expect(g, "the share is listed").toBeTruthy();
    expect(g.nodes.length).toBeGreaterThan(1);
    expect(g.missing ?? []).toHaveLength(0);
    expect(g.state).toBe("running");
  });

  test("the table shows the share once, with its nodes and free space", async ({ page }) => {
    await page.goto("/datacenter/storage");
    const section = page.getByRole("region", { name: "Shared storage" });
    const tr = row(section, SHARE);
    await expect(tr).toHaveCount(1);
    await expect(tr).toContainText(`${SERVER}:${EXPORT.replace(/\/$/, "")}`);
    await expect(tr).toContainText(env.node1);
    await expect(tr).toContainText(env.node2);
    await expect(tr).toContainText(/running/i);
    await expect(tr).toContainText(/free of/);
    await expect(tr.getByRole("button", { name: /^Add to / })).toHaveCount(0);
    // the share is a pool on each node, so the pool table lists it per node as NFS
    await expect(page.locator("tbody tr").filter({ hasText: SHARE }).filter({ hasText: "NFS" })).toHaveCount(2);
  });

  test("the pool dialog of a node no longer offers NFS, and the page keeps its width at 390 px", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/storage`);
    await page.getByRole("button", { name: "New pool" }).click();
    const dlg = dialog(page, /New (storage )?pool/);
    await expect(dlg).toBeVisible();
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await dlg.getByRole("combobox", { name: "Other storage types" }).click();
    await expect(page.getByRole("listbox").last()).not.toContainText("NFS");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto("/datacenter/storage");
    await expect(page.getByRole("region", { name: "Shared storage" })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });

  test("migration pre-flight: a VM whose disk is on the share is not copied", async ({ page, api, n1, n2 }) => {
    const r = await api.post(`/nodes/${n1}/vms`, { name: VM, vcpus: 1, memoryMiB: 128, firmware: "bios", pool: SHARE, diskGiB: 1, network: "default", start: false });
    const vm = { node: n1, uuid: r.uuid as string, name: VM };
    await openVM(page, vm);
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${VM}`);
    await expect(dlg).toContainText("Offline: the VM is shut off", { timeout: 60_000 });
    await expect(dlg).toContainText("no disk is copied", { timeout: 60_000 });
    await expect(dlg).toContainText(`vda on ${SHARE}`);
    await expect(dlg.getByRole("img", { name: "failed" })).toHaveCount(0);
    await expect(dlg.getByRole("button", { name: "Migrate", exact: true })).toBeEnabled();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
    void n2;
  });

  test("Remove is refused while a VM has a disk on the share", async ({ page }) => {
    await page.goto("/datacenter/storage");
    const tr = row(page.getByRole("region", { name: "Shared storage" }), SHARE);
    await tr.getByRole("button", { name: "Remove", exact: true }).click();
    await confirm(page, `Remove ${SHARE}`);
    await expect(page.locator(".error")).toContainText(new RegExp(`cannot remove ${SHARE}.*${VM}`));
    await expect(row(page.getByRole("region", { name: "Shared storage" }), SHARE)).toHaveCount(1);
  });

  test("with the VM gone, Remove unmounts the share on every node", async ({ page, api }) => {
    await cleanVMs(api, VM);
    await page.goto("/datacenter/storage");
    const tr = row(page.getByRole("region", { name: "Shared storage" }), SHARE);
    await tr.getByRole("button", { name: "Remove", exact: true }).click();
    await confirm(page, `Remove ${SHARE}`);
    await expect(row(page.locator("body"), SHARE)).toHaveCount(0, { timeout: 60_000 });
    const r = await api.get("/storage/shared");
    expect(r.shared.find((s: any) => s.name === SHARE)).toBeUndefined();
    for (const n of await api.get("/nodes").then((x: any) => x.nodes ?? x)) {
      const pools = await api.get(`/nodes/${n.id}/pools`);
      expect(pools.find((p: any) => p.name === SHARE), `pool gone on node ${n.id}`).toBeUndefined();
    }
    void node1;
  });
});
