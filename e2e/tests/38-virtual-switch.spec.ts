// Virtual switches on the topology: the dialog opens with its fields, Review shows the plan in plain words and
// changes nothing, Cancel closes it. This spec NEVER presses Apply, so it is safe on any lab. It needs a node with
// a free network card (physical, not part of a bridge, and a node network system the feature supports); without
// one the tests that need it are skipped with a message. A viewer sees no buttons.
import { Browser, expect as pwExpect } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";

const VIEWER = `${P}vswviewer`;
const PASS = "e2e-vsw-pass-123";

/** The first online node that can host a new virtual switch: supported, nothing pending, a free card. */
async function nodeWithFreeCard(api: any): Promise<{ id: number; card: string; vlanCapable: boolean } | undefined> {
  for (const n of await api.nodes()) {
    if (n.state !== "online") continue;
    const st = await api.get(`/nodes/${n.id}/hostnet`).catch(() => undefined);
    if (!st || !st.supported || st.status?.pending) continue;
    const card = (st.interfaces as any[]).find((i) => i.type === "physical" && !i.master);
    if (card) return { id: n.id, card: card.name, vlanCapable: !!st.vlanCapable };
  }
  return undefined;
}

test.describe.serial("virtual switch dialog", () => {
  test.afterAll(async ({ api }) => {
    for (const u of await api.get("/users")) if (u.username === VIEWER) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("Add virtual switch shows its fields and Review changes nothing", async ({ page, api }) => {
    const n = await nodeWithFreeCard(api);
    test.skip(!n, "no node with a free network card and a supported network system: nothing to review");
    const before = await api.get(`/nodes/${n!.id}/hostnet`);
    await page.goto(`/node/${n!.id}/topology`);
    await page.getByRole("button", { name: "Add virtual switch" }).click();
    const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
    await expect(dlg.getByLabel("Name")).toHaveValue(/^vsw\d+$/);
    await expect(dlg.getByRole("combobox", { name: "Uplink" })).toBeVisible();
    if (n!.vlanCapable) await expect(dlg.getByRole("checkbox", { name: "VLANs" })).toBeChecked();
    else await expect(dlg.getByText("VLANs need libvirt 11 or later on this node.")).toBeVisible();
    // one Advanced section, closed: the confirm time is inside it
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveCount(1);
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await expect(dlg.getByLabel("Confirm time (seconds)")).toHaveValue("90");
    // choose the free card: when it has an address, the dialog says what happens to it
    await dlg.getByRole("combobox", { name: "Uplink" }).click();
    await page.getByRole("listbox").last().getByRole("option").filter({ hasText: n!.card }).first().click();
    // Review is read-only
    await dlg.getByRole("button", { name: "Review" }).click();
    await expect(dlg.getByText("This will happen on the node:")).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Details" })).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Apply" })).toBeVisible();
    // go back and cancel; never Apply
    await dlg.getByRole("button", { name: "Back" }).click();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
    const after = await api.get(`/nodes/${n!.id}/hostnet`);
    expect(after.bridges.map((b: any) => b.name)).toEqual(before.bridges.map((b: any) => b.name));
    expect(after.status.pending).toBeFalsy();
  });

  test("an unused card offers Use for a new virtual switch, preselected", async ({ page, api }) => {
    const n = await nodeWithFreeCard(api);
    test.skip(!n, "no node with a free network card and a supported network system");
    await page.goto(`/node/${n!.id}/topology`);
    const unused = page.getByRole("region", { name: "Unused network cards" });
    test.skip((await unused.count()) === 0, "this node has no card outside a bridge");
    await unused.getByRole("button", { name: "Use for a new virtual switch" }).first().click();
    const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
    await expect(dlg.getByRole("combobox", { name: "Uplink" })).not.toContainText("None (internal only)");
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
  });

  test("the dialog fits at phone width", async ({ page, api }) => {
    const n = await nodeWithFreeCard(api);
    test.skip(!n, "no node with a free network card and a supported network system");
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto(`/node/${n!.id}/topology`);
    await page.getByRole("button", { name: "Add virtual switch" }).click();
    const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
    await expect(dlg).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    const box = await dlg.boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  });

  test("a viewer sees the topology without any virtual switch button", async ({ browser, api }: { browser: Browser } & any) => {
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
    await pwExpect(page.getByRole("button", { name: "Add virtual switch" })).toHaveCount(0);
    await pwExpect(page.getByRole("button", { name: "Add network" })).toHaveCount(0);
    await pwExpect(page.getByRole("button", { name: "Use for a new virtual switch" })).toHaveCount(0);
    await ctx.close();
  });
});
