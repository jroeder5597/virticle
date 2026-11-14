// Network tab of a node: Add network (the three answers), a NAT network, its detail with DHCP reservations
// (add, edit, remove), stop/start/autostart from the menu of the network, delete.
import { Page } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { autoDialogs, dialog, field, row, confirm } from "../lib/ui";

const NET = `${P}net`;
const OCT = 100 + Math.floor(Math.random() * 100);
const SUBNET = `10.79.${OCT}`;

/** The box of a network in the topology of the node page. */
const box = (page: Page, name: string) => page.locator(`[data-topo-id="net:${name}"]`);
const menu = (page: Page, name: string) => box(page, name).getByRole("button", { name: `Actions for network ${name}` });

/** Add network, "A private network made by libvirt", Next: the dialog of the private network. */
async function openPrivate(page: Page) {
  await page.locator('[data-vsw="add-net"]').click();
  const ask = dialog(page, "Add network");
  await ask.getByRole("radio", { name: "A private network made by libvirt" }).check();
  await ask.getByRole("button", { name: "Next" }).click();
  return dialog(page, "New private network");
}

test.describe.serial("networks", () => {
  test.afterAll(async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "delete" }).catch(() => {});
  });

  const detail = (api: any, n: number) => api.get(`/nodes/${n}/networks/${NET}`);

  test("Add network asks what the VMs connect to", async ({ page, api, n1 }) => {
    const topo = await api.get(`/topology?node=${n1}`);
    const bridges = topo.nodes[0].switches.filter((s: any) => s.kind === "linux-bridge" || s.kind === "openvswitch");
    await page.goto(`/node/${n1}/network`);
    await page.locator('[data-vsw="add-net"]').click();
    const ask = dialog(page, "Add network");
    const existing = ask.getByRole("radio", { name: "An existing virtual switch" });
    await expect(ask.getByRole("radio", { name: "A new virtual switch on a network card" })).toBeVisible();
    await expect(ask.getByRole("radio", { name: "A private network made by libvirt" })).toBeVisible();
    if (bridges.length === 0) {
      // only libvirt's own bridges here: the first answer is not offered, and says why
      await expect(existing).toBeDisabled();
      await expect(ask.getByText("This node has no virtual switch yet.")).toBeVisible();
    } else {
      // an existing switch leads into Add network on that switch
      await expect(existing).toBeChecked();
      await ask.getByRole("button", { name: "Next" }).click();
      await expect(dialog(page, /^Add network on /)).toBeVisible();
    }
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
  });

  test("create a NAT network", async ({ page, api, n1 }) => {
    await page.goto(`/node/${n1}/network`);
    const dlg = await openPrivate(page);
    await expect(field(dlg, "Name")).toBeFocused();
    await field(dlg, "Name").fill(NET);
    await field(dlg, "Address of the node on this network").fill(`${SUBNET}.1/24`);
    await dlg.getByRole("button", { name: "Create network" }).click();
    await expect(dlg).toBeHidden();
    const b = box(page, NET);
    await expect(b).toBeVisible();
    await expect(b).toContainText("NAT");
    await expect(b).not.toContainText("Stopped");
    const n = await detail(api, n1);
    expect(n).toMatchObject({ active: true, forward: "nat", dhcp: true });
  });

  test("an overlapping or invalid network is refused with a message", async ({ page, n1 }) => {
    await page.goto(`/node/${n1}/network`);
    const dlg = await openPrivate(page);
    await field(dlg, "Name").fill(`${NET}-dup`);
    await field(dlg, "Address of the node on this network").fill(`${SUBNET}.1/24`);
    await dlg.getByRole("button", { name: "Create network" }).click();
    await expect(dlg.locator(".error")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("DHCP reservation: add, edit, remove", async ({ page, api, n1 }) => {
    autoDialogs(page);
    await page.goto(`/node/${n1}/network`);
    await box(page, NET).getByRole("button", { name: NET, exact: true }).click();
    const dlg = dialog(page, `Network ${NET}`);
    await expect(dlg).toContainText("DHCP leases");
    const card = dlg.locator(".card", { hasText: "DHCP reservations" });
    const mac = "52:54:00:79:00:01";
    await field(card, "MAC address").fill(mac);
    await field(card, "IP address").fill(`${SUBNET}.50`);
    await field(card, "Host name (optional)").fill("res1");
    await card.getByRole("button", { name: "Add reservation" }).click();
    const r = row(card, mac);
    await expect(r).toContainText(`${SUBNET}.50`);
    await expect(r).toContainText("res1");
    expect((await detail(api, n1)).hosts).toEqual([expect.objectContaining({ mac, ip: `${SUBNET}.50`, name: "res1" })]);
    // the form is cleared after saving
    await expect(field(card, "MAC address")).toHaveValue("");

    await r.getByRole("button", { name: "Edit" }).click();
    await expect(field(card, "MAC address")).toHaveValue(mac);
    await field(card, "IP address").fill(`${SUBNET}.51`);
    await card.getByRole("button", { name: "Save reservation" }).click();
    await expect(row(card, mac)).toContainText(`${SUBNET}.51`);
    expect((await detail(api, n1)).hosts[0].ip).toBe(`${SUBNET}.51`);

    // an address outside the subnet is refused
    await field(card, "MAC address").fill("52:54:00:79:00:02");
    await field(card, "IP address").fill("192.0.2.5");
    await card.getByRole("button", { name: "Add reservation" }).click();
    await expect(card.locator(".error")).toBeVisible();

    await row(card, mac).getByRole("button", { name: "Remove" }).click();
    await confirm(page, "Remove DHCP reservation");
    await expect(row(card, mac)).toHaveCount(0);
    expect((await detail(api, n1)).hosts).toHaveLength(0);
    // the definition is in the same dialog
    await expect(dlg.getByText("Definition (XML)")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("stop, start, autostart", async ({ page, api, n1 }) => {
    autoDialogs(page);
    await page.goto(`/node/${n1}/network`);
    const b = box(page, NET);
    await menu(page, NET).click();
    await page.getByRole("menuitem", { name: "Stop", exact: true }).click();
    await confirm(page, `Stop network ${NET}`);
    await expect(b).toContainText("Stopped");
    expect((await detail(api, n1)).active).toBe(false);
    // a stopped network stays in the topology and can be started there
    await menu(page, NET).click();
    await page.getByRole("menuitem", { name: "Start", exact: true }).click();
    await expect(b).not.toContainText("Stopped");
    expect((await detail(api, n1)).active).toBe(true);
    // networks made here start with the host; toggle it off and on
    expect((await detail(api, n1)).autostart).toBe(true);
    await menu(page, NET).click();
    await page.getByRole("menuitem", { name: "No autostart" }).click();
    await expect.poll(async () => (await detail(api, n1)).autostart).toBe(false);
    await menu(page, NET).click();
    await expect(page.getByRole("menuitem", { name: "Autostart", exact: true })).toBeVisible();
    await page.getByRole("menuitem", { name: "Autostart", exact: true }).click();
    await expect.poll(async () => (await detail(api, n1)).autostart).toBe(true);
  });

  test("delete the network", async ({ page, api, n1 }) => {
    await page.goto(`/node/${n1}/network`);
    await menu(page, NET).click();
    await page.getByRole("menuitem", { name: "Delete" }).click();
    await confirm(page, `Delete network ${NET}`);
    await expect(box(page, NET)).toHaveCount(0);
    expect((await api.get(`/nodes/${n1}/networks`)).some((x: any) => x.name === NET)).toBe(false);
  });
});
