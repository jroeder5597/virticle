// Virtual switches with several uplinks, the node's address on a VLAN, and the MTU: dialog-only checks. The dialogs
// open with their fields, Review shows a plan and changes nothing, Cancel closes them. This spec NEVER presses Apply.
// Tests that need a node with two free network cards (or a managed VLAN-aware switch) are skipped with a message
// when the lab has none.
import { expect, test } from "../lib/fixtures";

interface Free {
  id: number;
  cards: string[];
}

async function nodeWithTwoFreeCards(api: any): Promise<Free | undefined> {
  for (const n of await api.nodes()) {
    if (n.state !== "online") continue;
    const st = await api.get(`/nodes/${n.id}/hostnet`).catch(() => undefined);
    if (!st || !st.supported || st.status?.pending) continue;
    const cards = (st.interfaces as any[]).filter((i) => i.type === "physical" && !i.master).map((i) => i.name as string);
    if (cards.length >= 2) return { id: n.id, cards };
  }
  return undefined;
}

async function managedVlanSwitch(api: any): Promise<{ id: number; name: string } | undefined> {
  for (const n of await api.nodes()) {
    if (n.state !== "online") continue;
    const st = await api.get(`/nodes/${n.id}/hostnet`).catch(() => undefined);
    const b = st?.bridges?.find((x: any) => x.managed && x.vlanFiltering && x.uplink);
    if (b && !st.status?.pending) return { id: n.id, name: b.name };
  }
  return undefined;
}

test.describe.serial("virtual switch with several uplinks", () => {
  test("the Mode select appears with two cards ticked, and Review changes nothing", async ({ page, api }) => {
    const n = await nodeWithTwoFreeCards(api);
    test.skip(!n, "no node with two free network cards: nothing to review");
    const before = await api.get(`/nodes/${n!.id}/hostnet`);
    await page.goto(`/node/${n!.id}/topology`);
    await page.getByRole("button", { name: "Add virtual switch" }).click();
    const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
    const group = dlg.getByRole("group", { name: "Uplinks" });
    const boxes = group.getByRole("checkbox");
    // untick everything, then tick one: no Mode
    for (let i = 0; i < (await boxes.count()); i++) await boxes.nth(i).uncheck();
    await group.getByRole("checkbox", { name: new RegExp("^" + n!.cards[0] + " ") }).check();
    await expect(dlg.getByRole("combobox", { name: "Mode" })).toHaveCount(0);
    // tick a second card: Mode appears with the two choices and one line of explanation
    await group.getByRole("checkbox", { name: new RegExp("^" + n!.cards[1] + " ") }).check();
    const mode = dlg.getByRole("combobox", { name: "Mode" });
    await expect(mode).toContainText("Failover (works with any switch)");
    await expect(dlg.getByText("One card carries the traffic. If its link fails, another card takes over.")).toBeVisible();
    await mode.click();
    await expect(page.getByRole("option", { name: "Load balance, needs LACP on the switch" })).toBeVisible();
    await expect(page.getByRole("option")).toHaveCount(2);
    await page.getByRole("option", { name: "Load balance, needs LACP on the switch" }).click();
    await expect(dlg.getByText("The ports of the physical switch must be set up for LACP.")).toBeVisible();
    // the MTU lives in the one Advanced section, with the confirm time
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveCount(1);
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await expect(dlg.getByLabel("MTU")).toBeVisible();
    await expect(dlg.getByLabel("Confirm time (seconds)")).toHaveValue("90");
    // Review is read-only; the plan names the bond
    await dlg.getByRole("button", { name: "Review" }).click();
    await expect(dlg.getByText("This will happen on the node:")).toBeVisible();
    await expect(dlg.getByText(/members of the new bond/)).toBeVisible();
    await dlg.getByRole("button", { name: "Back" }).click();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
    const after = await api.get(`/nodes/${n!.id}/hostnet`);
    expect(after.bridges.map((b: any) => b.name)).toEqual(before.bridges.map((b: any) => b.name));
    expect(after.status.pending).toBeFalsy();
  });

  test("Add virtual switch ticks only a card without an address, and the note names the address that moves", async ({ page, api }) => {
    let found = false;
    for (const n of await api.nodes()) {
      if (n.state !== "online") continue;
      const st = await api.get(`/nodes/${n.id}/hostnet`).catch(() => undefined);
      if (!st || !st.supported || st.status?.pending) continue;
      const free = (st.interfaces as any[]).filter((i) => i.type === "physical" && !i.master);
      if (free.length === 0) continue;
      found = true;
      const hasAddr = (i: any) => (i.addresses as string[]).some((a) => !/^fe80:/i.test(a));
      const bare = free.filter((i) => !hasAddr(i));
      await page.goto(`/node/${n.id}/topology`);
      await page.getByRole("button", { name: "Add virtual switch" }).click();
      const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
      const boxes = dlg.getByRole("group", { name: "Uplinks" }).getByRole("checkbox");
      await expect(boxes).toHaveCount(free.length);
      const ticked: string[] = [];
      for (let i = 0; i < free.length; i++) if (await boxes.nth(i).isChecked()) ticked.push(((await boxes.nth(i).evaluate((e) => e.parentElement!.textContent)) ?? "").split(" ")[0]);
      // a card without an address when there is one, else nothing
      expect(ticked).toEqual(bare.length > 0 ? [bare[0].name] : []);
      // a card with an address that is not the controller's: the note says which address moves and that it is not the controller's
      const other = free.find((i) => hasAddr(i) && i.name !== st.managementInterface);
      if (other) {
        for (let i = 0; i < free.length; i++) await boxes.nth(i).uncheck();
        await dlg.getByRole("group", { name: "Uplinks" }).getByRole("checkbox", { name: new RegExp("^" + other.name + " ") }).check();
        const ip = (other.addresses as string[]).find((a) => !/^fe80:/i.test(a))!.split("/")[0];
        await expect(dlg.getByText(`The address ${ip} of ${other.name} moves to the virtual switch and stays the same; it is not the address the controller uses.`)).toBeVisible();
      }
      await dlg.getByRole("button", { name: "Cancel" }).click();
      await expect(dlg).toBeHidden();
    }
    test.skip(!found, "no node with a free network card");
  });

  test("Edit uplinks, Edit MTU and Add this node's address open with their fields", async ({ page, api }) => {
    const sw = await managedVlanSwitch(api);
    test.skip(!sw, "no managed VLAN-aware virtual switch with an uplink in this lab");
    await page.goto(`/node/${sw!.id}/topology`);
    const acts = page.getByRole("button", { name: `Actions for ${sw!.name}` });
    await acts.click();
    await page.getByRole("menuitem", { name: "Edit uplinks" }).click();
    let dlg = page.getByRole("dialog", { name: `Edit uplinks of ${sw!.name}` });
    await expect(dlg.getByRole("group", { name: "Uplinks" })).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Review" })).toBeVisible();
    await dlg.getByRole("button", { name: "Cancel" }).click();

    await acts.click();
    await page.getByRole("menuitem", { name: "Edit MTU" }).click();
    dlg = page.getByRole("dialog", { name: `Edit MTU of ${sw!.name}` });
    await expect(dlg.getByLabel("MTU")).toBeVisible();
    await expect(dlg.getByText("The ports of the physical switch and the other nodes must use the same MTU.")).toBeVisible();
    await dlg.getByRole("button", { name: "Cancel" }).click();

    await acts.click();
    await page.getByRole("menuitem", { name: "Add this node's address" }).click();
    dlg = page.getByRole("dialog", { name: `Add this node's address on ${sw!.name}` });
    await expect(dlg.getByLabel("VLAN id")).toBeVisible();
    await expect(dlg.getByRole("combobox", { name: "Address" })).toContainText("From DHCP");
    // static: the address field appears; the default route lives under Advanced
    await dlg.getByRole("combobox", { name: "Address" }).click();
    await page.getByRole("option", { name: "Enter an address" }).click();
    await expect(dlg.getByLabel("IP address with prefix")).toBeVisible();
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveCount(1);
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await expect(dlg.getByRole("checkbox", { name: "Use this VLAN for the default route of this node" })).not.toBeChecked();
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await expect(dlg).toBeHidden();
    const st = await api.get(`/nodes/${sw!.id}/hostnet`);
    expect(st.status.pending).toBeFalsy();
  });

  test("the dialog with two cards fits at phone width", async ({ page, api }) => {
    const n = await nodeWithTwoFreeCards(api);
    test.skip(!n, "no node with two free network cards");
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto(`/node/${n!.id}/topology`);
    await page.getByRole("button", { name: "Add virtual switch" }).click();
    const dlg = page.getByRole("dialog", { name: "Add virtual switch" });
    const group = dlg.getByRole("group", { name: "Uplinks" });
    for (const c of n!.cards.slice(0, 2)) await group.getByRole("checkbox", { name: new RegExp("^" + c + " ") }).check();
    await expect(dlg.getByRole("combobox", { name: "Mode" })).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    const box = await dlg.boundingBox();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  });
});
