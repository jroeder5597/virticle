// Networks page: NAT network, DHCP reservations (add, edit, remove), stop/start, delete.
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row, confirm } from "../lib/ui";

const NET = `${P}net`;
const OCT = 100 + Math.floor(Math.random() * 100);
const SUBNET = `10.79.${OCT}`;

test.describe.serial("networks", () => {
  test.afterAll(async ({ api, n1 }) => {
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "delete" }).catch(() => {});
  });

  const detail = (api: any, n: number) => api.get(`/nodes/${n}/networks/${NET}`);

  test("create a NAT network", async ({ page, api, n1 }) => {
    await page.goto("/networks");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await page.getByRole("button", { name: "New network" }).click();
    const dlg = dialog(page, "New virtual network");
    await expect(field(dlg, "Name")).toBeFocused();
    await field(dlg, "Name").fill(NET);
    await field(dlg, "Address of the node on this network").fill(`${SUBNET}.1/24`);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, NET);
    await expect(r).toBeVisible();
    await expectBadge(r, "active");
    await expect(r).toContainText("NAT");
    await expect(r).toContainText(`${SUBNET}.1/24`);
    // the new network is selected and its details show below
    await expect(page.getByRole("heading", { name: NET, exact: true })).toBeVisible();
    const n = await detail(api, n1);
    expect(n).toMatchObject({ active: true, forward: "nat", dhcp: true });
  });

  test("an overlapping or invalid network is refused with a message", async ({ page }) => {
    await page.goto("/networks");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await page.getByRole("button", { name: "New network" }).click();
    const dlg = dialog(page, "New virtual network");
    await field(dlg, "Name").fill(`${NET}-dup`);
    await field(dlg, "Address of the node on this network").fill(`${SUBNET}.1/24`);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg.locator(".error")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("DHCP reservation: add, edit, remove", async ({ page, api, n1 }) => {
    autoDialogs(page);
    await page.goto("/networks");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, NET).click();
    const card = page.locator(".card", { hasText: "DHCP reservations" });
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
    await expect(page.getByRole("row", { name: new RegExp(`${NET}.*1 reserved`) })).toBeVisible();

    // an address outside the subnet is refused
    await field(card, "MAC address").fill("52:54:00:79:00:02");
    await field(card, "IP address").fill("192.0.2.5");
    await card.getByRole("button", { name: "Add reservation" }).click();
    await expect(card.locator(".error")).toBeVisible();

    await row(card, mac).getByRole("button", { name: "Remove" }).click();
    await confirm(page, "Remove DHCP reservation");
    await expect(row(card, mac)).toHaveCount(0);
    expect((await detail(api, n1)).hosts).toHaveLength(0);
  });

  test("stop, start, autostart", async ({ page, api, n1 }) => {
    autoDialogs(page);
    await page.goto("/networks");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    const r = row(page, NET);
    await r.getByRole("button", { name: "Stop" }).click();
    await confirm(page, `Stop network ${NET}`);
    await expectBadge(r, "inactive");
    expect((await detail(api, n1)).active).toBe(false);
    await r.getByRole("button", { name: "Start", exact: true }).click();
    await expectBadge(r, "active");
    // networks made here start with the host; toggle it off and on
    expect((await detail(api, n1)).autostart).toBe(true);
    await r.getByRole("button", { name: "No autostart" }).click();
    await expect(r.getByRole("button", { name: "Autostart", exact: true })).toBeVisible();
    expect((await detail(api, n1)).autostart).toBe(false);
    await r.getByRole("button", { name: "Autostart", exact: true }).click();
    await expect(r.getByRole("button", { name: "No autostart" })).toBeVisible();
  });

  test("delete the network", async ({ page, api, n1 }) => {
    await page.goto("/networks");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, NET).getByRole("button", { name: "Delete" }).click();
    await confirm(page, `Delete network ${NET}`);
    await expect(row(page, NET)).toHaveCount(0);
    expect((await api.get(`/nodes/${n1}/networks`)).some((x: any) => x.name === NET)).toBe(false);
  });
});
