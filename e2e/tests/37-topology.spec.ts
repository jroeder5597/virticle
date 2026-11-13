// Network topology tab: read-only. Both tabs render, the default NAT network of node 1 shows as a switch with its
// network, nothing scrolls sideways at phone width, and a viewer can open it.
import { Browser, expect as pwExpect } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";

const VIEWER = `${P}topoviewer`;
const PASS = "e2e-topo-pass-123";

const noSideScroll = (page: any) => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth);

test.describe.serial("network topology", () => {
  test.afterAll(async ({ api }) => {
    for (const u of await api.get("/users")) if (u.username === VIEWER) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("the API answers per node and names the default network", async ({ api, n1 }) => {
    const r = await api.get(`/topology?node=${n1}`);
    expect(r.nodes).toHaveLength(1);
    expect(r.nodes[0].error).toBeFalsy();
    const groups = r.nodes[0].switches.flatMap((s: any) => s.portGroups);
    expect(groups.some((g: any) => g.network === "default")).toBe(true);
  });

  test("the node page shows the default network as a switch with its network", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/topology`);
    await expect(page.getByRole("tab", { name: "Topology", exact: true })).toHaveAttribute("aria-selected", "true");
    const sw = page.locator(".toposwitch").filter({ has: page.getByRole("link", { name: "default", exact: true }) });
    await expect(sw).toBeVisible();
    await expect(sw).toContainText("Networks");
    await expect(sw).toContainText("Uplinks");
    await expect(sw).toContainText(/NAT to/);
    await expect(page.locator(".error")).toHaveCount(0);
  });

  test("the datacenter tab lists node 1 and filters by name", async ({ page }) => {
    await page.goto("/datacenter/topology");
    await expect(page.getByRole("tab", { name: "Topology", exact: true })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator(".toponode").filter({ hasText: env.node1 }).first()).toBeVisible();
    await expect(page.locator(".toposwitch").first()).toBeVisible();
    await page.getByLabel("Filter by VM or network name").fill("default");
    await expect(page.getByRole("link", { name: "default", exact: true }).first()).toBeVisible();
    await page.getByLabel("Filter by VM or network name").fill("no-such-name-anywhere");
    await expect(page.getByText("Nothing matches this filter.")).toBeVisible();
  });

  test("no sideways scrolling at phone width, on either tab", async ({ page, n1 }) => {
    await page.setViewportSize({ width: 390, height: 800 });
    for (const path of ["/datacenter/topology", `/node/${n1}/topology`]) {
      await page.goto(path);
      await expect(page.locator(".toposwitch").first()).toBeVisible();
      expect(await noSideScroll(page), path).toBe(true);
    }
  });

  test("a viewer can open the topology", async ({ browser, api }: { browser: Browser } & any) => {
    await api.post("/users", { username: VIEWER, password: PASS, role: "viewer" });
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base, viewport: { width: 1360, height: 860 } });
    const page = await ctx.newPage();
    await page.goto("/");
    await page.getByLabel("Username").fill(VIEWER);
    await page.getByLabel("Password").fill(PASS);
    await page.getByRole("button", { name: "Sign in" }).click();
    await pwExpect(page.getByRole("heading", { name: "Datacenter", level: 1 })).toBeVisible();
    await page.goto("/datacenter/topology");
    await pwExpect(page.locator(".toposwitch").first()).toBeVisible();
    await pwExpect(page.locator(".error")).toHaveCount(0);
    await ctx.close();
  });
});
