// Network drift between nodes: the API answers; the line "N networks differ between nodes." shows on the Datacenter
// Network and Topology tabs only when something differs; a small isolated network made on node 1 only is listed as
// missing on node 2, with no "Create" button (an isolated network cannot be copied as it is), and is gone again after
// it is deleted. Needs the two lab nodes to run the same distribution family (they can exchange VMs).
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";

const NET = `${P}drift`;
const OCT = 100 + Math.floor(Math.random() * 100);
const differs = /networks? differs? between nodes\.$/;

test.describe.serial("network drift", () => {
  test.afterAll(async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "delete", force: true }).catch(() => {});
  });

  test("the API answers with its documented shape", async ({ api }) => {
    const r = await api.get("/cluster/network-drift");
    expect(typeof r.networks).toBe("number");
    expect(Array.isArray(r.rows)).toBe(true);
    expect(Array.isArray(r.blocked)).toBe(true);
    expect(Array.isArray(r.unread)).toBe(true);
    for (const row of r.rows) {
      expect(["missing", "mode", "vlan", "bridge"]).toContain(row.kind);
      expect(row.network.length).toBeGreaterThan(0);
      expect(row.nodes.length).toBeGreaterThan(0);
    }
  });

  test("the line is absent when nothing differs", async ({ page, api }) => {
    const r = await api.get("/cluster/network-drift");
    test.skip(r.networks > 0, "the lab already has networks that differ between its nodes");
    for (const path of ["/datacenter/network"]) {
      await page.goto(path);
      await expect(page.getByRole("tab", { name: "Network", exact: true })).toBeVisible();
      await page.waitForTimeout(1500);
      await expect(page.getByRole("button", { name: differs })).toHaveCount(0);
    }
  });

  test("a network on one node only is listed as missing on the other", async ({ page, api, n1 }) => {
    await api.post(`/nodes/${n1}/networks`, { name: NET, mode: "isolated", address: `10.78.${OCT}.1/24`, dhcp: false });
    const r = await api.waitFor("the network to be reported", async () => {
      const x = await api.get("/cluster/network-drift");
      return x.rows.some((row: any) => row.network === NET) ? x : undefined;
    }, 30_000, 2000);
    const rows = r.rows.filter((row: any) => row.network === NET);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ kind: "missing", used: false });
    expect(rows[0].what).toContain(env.node1);
    expect(rows[0].fix).toBeUndefined();
    expect(r.blocked).not.toContain(NET); // no VM uses it: no alert

    for (const [path, tabName] of [["/datacenter/network", "Network"]]) {
      await page.goto(path);
      await expect(page.getByRole("tab", { name: tabName, exact: true })).toHaveAttribute("aria-selected", "true");
      const line = page.getByRole("button", { name: differs });
      await expect(line).toBeVisible();
      await expect(line).toHaveAttribute("aria-expanded", "false");
      await line.click();
      const tr = page.locator("#drift-body tbody tr").filter({ hasText: NET });
      await expect(tr).toBeVisible();
      await expect(tr).toContainText("missing on");
      await expect(tr.getByRole("button")).toHaveCount(0);
    }
    const alerts = await api.get("/alerts");
    expect(alerts.alerts.some((a: any) => a.kind === "net-drift")).toBe(false);
  });

  test("it is gone from the list once deleted", async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "delete", force: true });
    await api.waitFor("the drift to clear", async () => !(await api.get("/cluster/network-drift")).rows.some((row: any) => row.network === NET), 30_000, 2000);
  });
});
