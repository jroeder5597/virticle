// VM replication to a standby node: the Replication card of a running VM and its setup dialog, the first copy, the
// copy on the other node (state "replica", no normal actions, "Planned failover"), the planned failover itself (the
// VM runs on node 2, the old one on node 1 is a "old copy"), and the cleanup of both sides.
import { mkdirSync } from "fs";
import { Api } from "../lib/api";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field } from "../lib/ui";

const NAME = `${P}repl1`;
const SHOTS = process.env.E2E_SHOTS ?? "test-results/replication";

/** Everything named NAME on any node: stop replication, delete replicas, then delete the VMs. */
async function cleanAll(api: Api) {
  for (let pass = 0; pass < 8; pass++) {
    const mine = (await api.vms()).filter((v) => v.name === NAME);
    if (mine.length === 0) return;
    for (const v of mine) {
      const base = `/nodes/${v.node}/vms/${v.uuid}`;
      if (v.replica) await api.call("DELETE", `${base}/replica`, undefined, [400, 404, 409]).catch(() => {});
      else if (v.replication) await api.call("DELETE", `${base}/replication?deleteReplica=1`, undefined, [400, 404, 409]).catch(() => {});
    }
    await cleanVMs(api, NAME);
    await new Promise((r) => setTimeout(r, 5000));
  }
}

test.describe.serial("replication", () => {
  let vm: { node: number; uuid: string; name: string };
  let n2id = 0;

  test.beforeAll(async ({ api, n2 }) => {
    n2id = n2;
    mkdirSync(SHOTS, { recursive: true });
    await cleanAll(api);
  });
  test.afterAll(async ({ api }) => cleanAll(api));

  test("set up replication on a running VM and wait for the first copy", async ({ page, api }) => {
    test.setTimeout(300_000);
    await page.setViewportSize({ width: 1280, height: 900 });
    vm = await createCirros(api, (await api.nodeId(env.node1)), NAME, { start: true });
    await openVM(page, vm);
    const card = page.locator("section#replication");
    await expect(card).toContainText("Keep a copy of this VM on another node that starts with one click if this node fails.");
    await card.getByRole("button", { name: "Set up replication" }).click();
    const dlg = dialog(page, `Replicate ${NAME}`);
    await expect(field(dlg, "Replicate to")).toContainText(env.node2);
    await expect(field(dlg, "Every")).toContainText("15 minutes");
    await expect(dlg).toContainText("The first copy sends the whole VM, later copies only what changed.");
    // no pool choice, no Advanced section: the app decides
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveCount(0);
    await expect(dlg.getByText("Pool")).toHaveCount(0);
    await dlg.screenshot({ path: `${SHOTS}/setup-dialog.png` });
    await dlg.getByRole("button", { name: "Start replication" }).click();
    await expect(dlg).toContainText("Replication is set up");
    await dlg.getByRole("button", { name: "Close", exact: true }).first().click();
    await expect(dlg).toBeHidden();
    await expect(card).toContainText(env.node2);
    await expect(card).toContainText("Every15 minutes");
    // the first copy runs as a task: the card follows it
    await expect(card).toContainText(/Last copy.*ago/, { timeout: 240_000 });
    await expect(card).toContainText("everything");
    await expect(card.getByRole("button", { name: "Replicate now" })).toBeVisible();
    await expect(card.getByRole("button", { name: "Change" })).toBeVisible();
    await expect(card.getByRole("button", { name: "Stop replication" })).toBeVisible();
    await card.screenshot({ path: `${SHOTS}/source-card.png` });
  });

  test("a move to the standby is not refused: the replica steps aside", async ({ api, n1 }) => {
    const c = await api.call("POST", `/nodes/${n1}/vms/${vm.uuid}/migrate/check`, { target: n2id });
    const items: { check: string; ok: boolean; severity: string; message: string }[] = c.checks;
    expect(items.filter((x) => !x.ok && x.severity === "error").map((x) => x.check + ": " + x.message)).toEqual([]);
    const rep = items.find((x) => x.check === "Replica");
    expect(rep?.message).toContain("steps aside");
    expect(rep?.message).toContain("full copy");
  });

  test("on node 2 the VM is a replica: no actions, Planned failover offered", async ({ page, api }) => {
    const copy = await api.waitFor("the replica on node 2", async () => (await api.vms()).find((v) => v.name === NAME && v.node === n2id && v.state === "replica"), 60_000);
    // the VM list: state word, no selection, no power button
    await page.goto("/datacenter/vms");
    await page.getByLabel("Search VMs").fill(NAME);
    const rows = page.locator("tbody tr").filter({ hasText: NAME });
    const mine = rows.filter({ hasText: /Replica/ });
    await expect(mine).toHaveCount(1);
    await expect(mine.getByRole("checkbox")).toBeDisabled();
    await expect(mine.getByRole("button", { name: "Power On" })).toBeDisabled();
    // its page
    await openVM(page, { node: n2id, uuid: copy.uuid, name: NAME });
    const head = page.locator(".head");
    await expect(head.locator(".badge", { hasText: /^Replica$/ })).toBeVisible();
    await expect(head.getByRole("button", { name: "Actions" })).toHaveCount(0);
    await expect(page.locator(".tabs button")).toHaveCount(1);
    const card = page.locator("section#replica");
    await expect(card).toContainText(`This VM is the replica of ${NAME} on ${env.node1}`);
    await expect(card).toContainText(/\d+ cop(y|ies) kept/);
    await expect(card.getByRole("button", { name: "Planned failover" })).toBeVisible();
    await expect(card.getByRole("button", { name: "Delete replica" })).toBeVisible();
    await card.screenshot({ path: `${SHOTS}/replica-card.png` });
    // the confirmation says what happens; cancel it
    await card.getByRole("button", { name: "Planned failover" }).click();
    const dlg = dialog(page, "Planned failover");
    await expect(dlg).toContainText("Nothing is lost");
    await dlg.screenshot({ path: `${SHOTS}/planned-confirm.png` });
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
    // the backend refuses a normal change
    const r = await api.ctx.fetch(`/api/nodes/${n2id}/vms/${copy.uuid}/action`, { method: "POST", data: { action: "start" } });
    expect(r.status()).toBe(400);
    for (const [method, path, data] of [
      ["PUT", "autostart", { autostart: true }],
      ["PUT", "tags", { tags: ["x"] }],
      ["POST", "snapshots", { name: "s" }],
      ["POST", "disks/vda/resize", { sizeGiB: 99 }],
      ["POST", "clone", { name: `${NAME}-c` }],
      ["POST", "backup", {}],
      ["PUT", "backup-schedule", { enabled: true }],
      ["POST", "migrate", { target: 1 }],
      ["DELETE", "", undefined],
    ] as [string, string, unknown][]) {
      const x = await api.ctx.fetch(`/api/nodes/${n2id}/vms/${copy.uuid}${path ? "/" + path : ""}`, { method, data });
      expect(x.status(), `${method} ${path}`).toBe(400);
    }
  });

  test("planned failover: the VM runs on node 2, the old VM is an old copy", async ({ page, api, n1 }) => {
    test.slow();
    test.setTimeout(420_000);
    const copy = (await api.vms()).find((v) => v.name === NAME && v.node === n2id)!;
    // cirros ignores the power button, which would make Virticle wait three minutes and refuse (it does, safely:
    // the VM keeps running); so the source VM is powered off first and the planned failover takes the shut-off path
    await api.call("POST", `/nodes/${n1}/vms/${copy.uuid}/action`, { action: "poweroff" });
    await openVM(page, { node: n2id, uuid: copy.uuid, name: NAME });
    await page.locator("section#replica").getByRole("button", { name: "Planned failover" }).click();
    const dlg = dialog(page, "Planned failover");
    await expect(dlg).toContainText("Nothing is lost");
    await dlg.getByRole("button", { name: "Planned failover" }).click();
    await expect(dlg).toBeHidden();
    // the page of the replica becomes a normal VM when the task is done
    await api.waitFor("the VM running on node 2", async () => {
      const d = await api.vmDetail(n2id, copy.uuid);
      return d.state === "running";
    }, 360_000, 3000);
    await expect(page.locator(".head .badge", { hasText: /^Running$/ })).toBeVisible({ timeout: 30_000 });
    await expect(page.locator(".head").getByRole("button", { name: "Actions" })).toBeVisible();
    await expect(page.locator("section#replica")).toHaveCount(0);
    // the old VM on node 1 is shut off and marked; once the copy back has arrived it is the replica
    const old = await api.waitFor("the old VM marked on node 1", async () => {
      const d = await api.vmDetail(n1, copy.uuid);
      return d.state === "stale" || d.state === "replica" ? d : undefined;
    }, 60_000, 1000);
    if (old.state === "stale") {
      await openVM(page, { node: n1, uuid: copy.uuid, name: NAME });
      await expect(page.locator(".head .badge", { hasText: /^Old copy$/ })).toBeVisible();
      await expect(page.locator("section#stale")).toContainText(`This is the old copy of ${NAME}`);
      await expect(page.locator("section#stale").getByRole("button", { name: "Replicate back" })).toBeVisible();
      await page.locator("section#stale").screenshot({ path: `${SHOTS}/stale-card.png` });
    }
    // the VM on node 2 now replicates back to node 1 by itself
    const st = await api.waitFor("replication back to node 1", async () => {
      const s = await api.get(`/nodes/${n2id}/vms/${copy.uuid}/replication`);
      return s.configured && s.config.targetNode === n1 ? s : undefined;
    }, 60_000, 2000);
    expect(st.config.everyMinutes).toBeGreaterThan(0);
  });

  test("bulk delete offers to delete the replica too", async ({ page, api }) => {
    const mine = (await api.vms()).find((v) => v.name === NAME && v.node === n2id)!;
    await api.waitFor("the copy back to finish", async () => !(await api.get(`/nodes/${n2id}/vms/${mine.uuid}/replication`)).running, 240_000, 3000);
    await api.post(`/nodes/${n2id}/vms/${mine.uuid}/action`, { action: "poweroff" });
    await api.waitFor("the VM off", async () => (await api.vmDetail(n2id, mine.uuid)).state === "shutoff", 60_000, 1500);
    await page.goto("/datacenter/vms");
    await page.getByLabel("Search VMs").fill(NAME);
    const row = page.locator("tbody tr").filter({ hasText: NAME }).filter({ hasText: env.node2 });
    await row.getByRole("checkbox").check();
    await page.locator(".bulkbar").getByRole("button", { name: "Delete" }).click();
    const dlg = dialog(page, "Delete 1 VM(s)?");
    await expect(dlg.getByLabel(/Also delete the replica of the VM that has one/)).toBeChecked();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
    // a start in the middle of a copy of the shut-off VM discards that copy, and node 1 would keep the old copy
    await api.waitFor("no copy running", async () => !(await api.get(`/nodes/${n2id}/vms/${mine.uuid}/replication`)).running, 240_000, 2000);
    await api.post(`/nodes/${n2id}/vms/${mine.uuid}/action`, { action: "start" });
  });

  test("stop replication with its copy, then delete the VM", async ({ page, api, n1 }) => {
    test.setTimeout(300_000);
    const mine = (await api.vms()).find((v) => v.name === NAME && v.node === n2id)!;
    // a copy that is being written cannot be deleted: let the first copy back finish
    await api.waitFor("the copy back to finish", async () => !(await api.get(`/nodes/${n2id}/vms/${mine.uuid}/replication`)).running, 240_000, 3000);
    await openVM(page, { node: n2id, uuid: mine.uuid, name: NAME });
    const card = page.locator("section#replication");
    await card.getByRole("button", { name: "Stop replication" }).click();
    const dlg = dialog(page, "Stop replication");
    await expect(dlg.getByLabel(/Also delete the copy on/)).toBeChecked();
    await dlg.getByRole("button", { name: "Stop replication" }).click();
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    await expect(card.getByRole("button", { name: "Set up replication" })).toBeVisible({ timeout: 30_000 });
    await expect.poll(async () => (await api.vms()).filter((v) => v.name === NAME).length, { timeout: 60_000 }).toBe(1);
    await cleanAll(api);
    expect((await api.vms()).filter((v) => v.name === NAME)).toHaveLength(0);
    void n1;
  });
});
