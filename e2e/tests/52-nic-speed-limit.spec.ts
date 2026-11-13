// Speed limit of a VM network card, on a shut-off VM (nothing needs to run): set 50 Mbit/s with the one field,
// see it in the table and in the XML, split it into download and upload under Advanced, remove it.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, field, row } from "../lib/ui";

const NAME = `${P}nicbw`;

test.describe.serial("network card speed limit", () => {
  let vm: { node: number; uuid: string; name: string };
  const card = (page: any) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Network interfaces", exact: true }) });

  test.afterAll(async ({ api }) => {
    await cleanVMs(api, NAME);
  });

  test("a new card has no limit and the table says nothing about it", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    await openVM(page, vm, "hardware");
    await expect(card(page).locator("tbody tr")).toHaveCount(1);
    await expect(card(page)).not.toContainText("limited to");
    expect((await api.vmDetail(vm.node, vm.uuid)).xml).not.toContain("<bandwidth");
  });

  test("50 Mbit/s with the one field", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page);
    await c.locator("tbody tr").first().getByRole("button", { name: "Edit" }).click();
    const dlg = dialog(page);
    await field(dlg, "Speed limit (Mbit/s)").fill("50");
    await dlg.getByRole("button", { name: "Apply" }).click();
    await expect(dlg).toBeHidden();
    await expect(row(c, /./).first()).toContainText("limited to 50 Mbit/s");
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.nics[0]).toMatchObject({ limitDown: 50, limitUp: 50 });
    // 50 Mbit/s = 6250 kilobytes per second, both directions, no peak
    expect(d.xml).toMatch(/<inbound average='6250'[^>]*\/>/);
    expect(d.xml).toMatch(/<outbound average='6250'[^>]*\/>/);
    expect(d.xml).not.toContain("peak=");
  });

  test("download and upload apart, under Advanced", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page);
    await c.locator("tbody tr").first().getByRole("button", { name: "Edit" }).click();
    const dlg = dialog(page);
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await field(dlg, "Download limit (Mbit/s)").fill("20");
    await field(dlg, "Upload limit (Mbit/s)").fill("5");
    await dlg.getByRole("button", { name: "Apply" }).click();
    await expect(dlg).toBeHidden();
    await expect(row(c, /./).first()).toContainText("limited to 20 Mbit/s down, 5 Mbit/s up");
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.xml).toMatch(/<inbound average='2500'/);
    expect(d.xml).toMatch(/<outbound average='625'/);
  });

  test("an invalid limit is refused before anything is sent", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page);
    await c.locator("tbody tr").first().getByRole("button", { name: "Edit" }).click();
    const dlg = dialog(page);
    await field(dlg, "Speed limit (Mbit/s)").fill("-3");
    await expect(dlg.getByRole("button", { name: "Apply" })).toBeDisabled();
    await expect(dlg).toContainText("whole number of Mbit/s");
    await dlg.getByRole("button", { name: "Cancel" }).click();
    // the server refuses it as well
    const mac = (await api.vmDetail(vm.node, vm.uuid)).nics[0].mac;
    const r = await api.call("PUT", `/nodes/${vm.node}/vms/${vm.uuid}/nics/${mac}`, { limit: { down: 100001, up: 0 } }, [400]);
    expect(JSON.stringify(r)).toContain("speed limit");
  });

  test("an empty field removes the limit and the element", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page);
    await c.locator("tbody tr").first().getByRole("button", { name: "Edit" }).click();
    const dlg = dialog(page);
    await field(dlg, "Speed limit (Mbit/s)").fill("");
    await dlg.getByRole("button", { name: "Apply" }).click();
    await expect(dlg).toBeHidden();
    await expect(c).not.toContainText("limited to");
    expect((await api.vmDetail(vm.node, vm.uuid)).xml).not.toContain("<bandwidth");
  });

  test("a card can be added with a limit under Advanced", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    const c = card(page);
    await c.getByRole("button", { name: "Add interface" }).click();
    const dlg = dialog(page, "Add network interface");
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await field(dlg, "Speed limit (Mbit/s)").fill("100");
    await dlg.getByRole("button", { name: "Add" }).click();
    await expect(dlg).toBeHidden();
    await expect(c.locator("tbody tr")).toHaveCount(2);
    await expect(c.locator("tbody tr").nth(1)).toContainText("limited to 100 Mbit/s");
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.xml).toMatch(/<inbound average='12500'/);
  });
});
