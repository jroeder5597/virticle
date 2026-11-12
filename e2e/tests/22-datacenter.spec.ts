// Datacenter page: summary with totals, unified storage and network tabs across nodes, old addresses redirect,
// and an unreachable node is named instead of breaking the lists.
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { confirm, dialog, expectBadge, field, row, pick } from "../lib/ui";

const POOL = `${P}dcpool`;
const PATH = `/var/lib/libvirt/${P}dcpool`;
const NET = `${P}dcnet`;
const OCT = 100 + Math.floor(Math.random() * 100);
const BOGUS = "192.0.2.77"; // TEST-NET-1: never answers

test.describe.serial("datacenter", () => {
  let bogusId = 0;
  test.afterAll(async ({ api, n2 }) => {
    if (bogusId) await api.del(`/nodes/${bogusId}`).catch(() => {});
    for (const n of (await api.nodes()).filter((x) => x.addr === BOGUS)) await api.del(`/nodes/${n.id}`).catch(() => {});
    const pools: any[] = await api.get(`/nodes/${n2}/pools`);
    if (pools.some((p) => p.name === POOL)) {
      for (const v of await api.get(`/nodes/${n2}/pools/${POOL}/volumes`).catch(() => [])) await api.del(`/nodes/${n2}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
      await api.post(`/nodes/${n2}/pools/${POOL}/action`, { action: "remove" }).catch(() => {});
    }
    await api.post(`/nodes/${n2}/networks/${NET}/action`, { action: "delete" }).catch(() => {});
  });

  test("summary shows totals and every node", async ({ page, api }) => {
    const nodes = await api.nodes();
    await page.goto("/datacenter");
    await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Summary", exact: true })).toHaveAttribute("aria-selected", "true");
    const tabs = await page.getByRole("tab").allTextContents();
    expect(tabs).toEqual(["Summary", "Virtual machines", "Storage", "Network", "Backups", "Nodes", "Tasks"]);
    await expect(page.locator(".dctile", { hasText: "Nodes online" })).toContainText(new RegExp(`${nodes.filter((n) => n.state === "online").length} of ${nodes.length}`));
    await expect(page.locator(".dctile", { hasText: "Virtual machines running" })).toContainText(/\d+ of \d+/);
    await expect(page.locator(".dctile", { hasText: "Storage in use" })).toHaveAttribute("title", /counted as one filesystem/);
    await expect(page.locator(".dctile", { hasText: "Storage in use" })).toContainText(/[KMGT]iB of [\d.]+ [KMGT]iB/);
    await expect(page.locator(".dctile", { hasText: "vCPUs" })).toContainText(/\d+ of \d+/, { timeout: 30_000 });
    for (const n of nodes) await expect(row(page.locator(".dcnodes"), n.hostname ?? n.addr).first()).toBeVisible();
    await expect(page.locator(".dcnodes").getByRole("link", { name: env.node1 })).toHaveAttribute("href", /\/node\/\d+$/);
    await expect(page.locator(".error")).toHaveCount(0);
  });

  test("the top bar opens the matching tab and follows it", async ({ page }) => {
    await page.goto("/datacenter");
    const top = page.getByRole("navigation", { name: "Main", exact: true });
    for (const [link, slug, tab] of [
      ["Virtual machines", "vms", "Virtual machines"],
      ["Storage", "storage", "Storage"],
      ["Networks", "network", "Network"],
      ["Backups", "backups", "Backups"],
      ["Nodes", "nodes", "Nodes"],
      ["Tasks", "tasks", "Tasks"],
    ]) {
      await top.getByRole("link", { name: link, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`/datacenter/${slug}$`));
      await expect(page.getByRole("tab", { name: tab, exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(top.getByRole("link", { name: link, exact: true })).toHaveClass(/active/);
    }
    await page.getByRole("tab", { name: "Summary", exact: true }).click();
    await expect(page).toHaveURL(/\/datacenter$/);
    await page.locator(".trow", { hasText: "Datacenter" }).first().getByRole("link").click();
    await expect(page).toHaveURL(/\/datacenter$/);
  });

  test("storage tab lists the pools of two nodes with a Node column", async ({ page }) => {
    await page.goto("/datacenter/storage");
    await expect(page.getByRole("columnheader", { name: "Node", exact: true })).toBeVisible();
    await expect(row(page, "default").filter({ hasText: env.node1 })).toBeVisible();
    await expect(row(page, "default").filter({ hasText: env.node2 })).toBeVisible();
    // the node filter narrows the list, the text filter works across nodes
    await pick(page, "Filter by node", env.node2);
    await expect(row(page, "default").filter({ hasText: env.node1 })).toHaveCount(0);
    await expect(row(page, "default").filter({ hasText: env.node2 })).toBeVisible();
    await pick(page, "Filter by node", "All nodes");
    await page.getByLabel("Search pools by name, type or state").fill("zzz-no-such-pool");
    await expect(page.getByText("No pool matches the filter.")).toBeVisible();
    await page.getByLabel("Search pools by name, type or state").fill("");
    await expect(row(page, "default").first()).toBeVisible();
  });

  test("create a pool on node 2, manage a volume in it, remove the pool", async ({ page, api, n2 }) => {
    await page.goto("/datacenter/storage");
    await page.getByRole("button", { name: "New pool" }).click();
    const dlg = dialog(page, "New storage pool");
    await pick(dlg, field(dlg, "Node"), env.node2);
    await field(dlg, "Name").fill(POOL);
    await field(dlg, "Directory on the node").fill(PATH);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, POOL);
    await expect(r).toBeVisible();
    await expect(r).toContainText(env.node2);
    await expectBadge(r, "running");
    expect((await api.get(`/nodes/${n2}/pools`)).some((p: any) => p.name === POOL)).toBe(true);

    await r.click();
    await expect(page.getByRole("heading", { name: `Volumes in ${POOL} on ${env.node2}` })).toBeVisible();
    await expect(page.getByText("This pool is empty.")).toBeVisible();
    await page.getByRole("button", { name: "New volume" }).click();
    const vd = dialog(page, `New volume in ${POOL}`);
    await field(vd, "Name").fill(`${P}dcvol`);
    await field(vd, "Size (GiB)").fill("1");
    await vd.getByRole("button", { name: "Create" }).click();
    await expect(vd).toBeHidden();
    const v = row(page, `${P}dcvol.qcow2`);
    await expect(v).toBeVisible();
    expect((await api.get(`/nodes/${n2}/pools/${POOL}/volumes`)).map((x: any) => x.name)).toContain(`${P}dcvol.qcow2`);
    await v.getByRole("button", { name: "Delete" }).click();
    await confirm(page, /^Delete volume/);
    await expect(v).toHaveCount(0);
    expect(await api.get(`/nodes/${n2}/pools/${POOL}/volumes`)).toEqual([]);

    await r.getByRole("button", { name: "Remove" }).click();
    await expect(page.getByRole("dialog", { name: /^Remove pool/ })).toContainText(env.node2);
    await confirm(page, /^Remove pool/);
    await expect(r).toHaveCount(0);
    expect((await api.get(`/nodes/${n2}/pools`)).some((p: any) => p.name === POOL)).toBe(false);
  });

  test("network tab: create and delete a network on node 2, host interfaces load only when opened", async ({ page, api, n2 }) => {
    const calls: string[] = [];
    page.on("request", (q) => /\/api\/nodes\/\d+\/interfaces/.test(q.url()) && calls.push(q.url()));
    await page.goto("/datacenter/network");
    await expect(page.getByRole("main").getByRole("columnheader", { name: "Node", exact: true })).toBeVisible();
    await expect(row(page, "default").filter({ hasText: env.node1 })).toBeVisible();
    await expect(row(page, "default").filter({ hasText: env.node2 })).toBeVisible();
    await page.getByRole("button", { name: "New network" }).click();
    const dlg = dialog(page, "New virtual network");
    await pick(dlg, field(dlg, "Node"), env.node2);
    await field(dlg, "Name").fill(NET);
    await field(dlg, "Address of the node on this network").fill(`10.79.${OCT}.1/24`);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, NET);
    await expect(r).toContainText(env.node2);
    await expectBadge(r, "active");
    await expect(page.getByRole("heading", { name: NET, exact: true })).toBeVisible();
    expect((await api.get(`/nodes/${n2}/networks`)).some((n: any) => n.name === NET)).toBe(true);

    expect(calls).toEqual([]); // collapsed: nothing was asked
    await page.getByRole("button", { name: new RegExp("^" + env.node1) }).click();
    await expect(page.locator(".nicbox tbody tr").first()).toBeVisible({ timeout: 30_000 });
    expect(calls.length).toBe(1);

    await r.getByRole("button", { name: "Delete" }).click();
    await confirm(page, `Delete network ${NET}`);
    await expect(r).toHaveCount(0);
    expect((await api.get(`/nodes/${n2}/networks`)).some((n: any) => n.name === NET)).toBe(false);
  });

  test("old addresses redirect and keep their query strings", async ({ page, n1 }) => {
    for (const [from, to] of [
      ["/", /\/datacenter$/],
      ["/storage", /\/datacenter\/storage$/],
      ["/networks", /\/datacenter\/network$/],
      ["/backups", /\/datacenter\/backups$/],
      ["/nodes", /\/datacenter\/nodes$/],
      ["/tasks", /\/datacenter\/tasks$/],
    ] as const) {
      await page.goto(from);
      await expect(page).toHaveURL(to);
      await expect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
    }
    // Add VM from the tree: ?new=<node> opens the New VM dialog on the VM tab
    await page.goto(`/?new=${n1}`);
    await expect(page.getByRole("dialog", { name: /New virtual machine|New VM/ })).toBeVisible();
    await expect(page).toHaveURL(/\/datacenter\/vms/);
    await page.keyboard.press("Escape");
    // a link from the New VM dialog: pool and catalogue open on the storage tab
    await page.goto("/storage?pool=default&catalogue=1");
    await expect(page).toHaveURL(/\/datacenter\/storage/);
    await expect(page.getByRole("dialog", { name: "Image catalogue" })).toBeVisible();
    await page.keyboard.press("Escape");
  });

  test("an unreachable node is named; the lists still render", async ({ page, api }) => {
    const res = await api.post("/nodes", { addr: BOGUS, user: "root" });
    bogusId = res.id;
    // the connection attempt has to fail first (or still be pending)
    await api.waitFor("bogus node not online", async () => (await api.nodes()).find((n) => n.id === bogusId)?.state !== "online", 20_000, 500);
    for (const [path, what] of [
      ["/datacenter/storage", "default"],
      ["/datacenter/network", "default"],
    ]) {
      await page.goto(path);
      const err = page.locator(".nodeerrs");
      await expect(err).toBeVisible();
      await expect(err).toContainText(BOGUS);
      await expect(row(page, what).filter({ hasText: env.node1 })).toBeVisible();
      await expect(row(page, what).filter({ hasText: env.node2 })).toBeVisible();
    }
    await page.goto("/datacenter");
    await expect(page.locator(".nodeerrs")).toContainText(BOGUS);
    await expect(page.locator(".dctile", { hasText: "Storage in use" })).toContainText(/GiB of/);
    await expect(row(page.locator(".dcnodes"), BOGUS)).toContainText(/offline|connecting/i);
    await api.del(`/nodes/${bogusId}`);
    bogusId = 0;
  });
});
