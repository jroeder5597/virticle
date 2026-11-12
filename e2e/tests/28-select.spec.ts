// The themed dropdowns (Select) and the editable combo (ComboInput): keyboard, label click, Escape inside a
// modal, a disabled option, the search box of a long list, and the free-text MAC field with suggestions.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, pick, row } from "../lib/ui";

const VM = `${P}sel1`;
const NET = `${P}selnet`;
const OCT = 100 + Math.floor(Math.random() * 100);

const list = (page: Page) => page.getByRole("listbox");

test.describe.serial("themed select", () => {
  let vm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, VM);
    await api.post(`/nodes/${n1}/networks`, { name: NET, mode: "nat", address: `10.79.${OCT}.1/24`, dhcp: true });
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "stop" });
  });
  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, VM);
    await api.post(`/nodes/${n1}/networks/${NET}/action`, { action: "delete" }).catch(() => {});
  });

  test("no native select or datalist on the pages used here", async ({ page }) => {
    for (const p of ["/users", "/audit", "/networks", "/storage", "/account"]) {
      await page.goto(p);
      await expect(page.getByRole("heading").first()).toBeVisible();
      await expect(page.locator("select, datalist")).toHaveCount(0);
    }
  });

  test("keyboard: open, arrows, type-ahead, Enter; Escape closes only the list inside a modal", async ({ page }) => {
    await page.goto("/users");
    await page.getByRole("button", { name: "Add user" }).click();
    const dlg = dialog(page, "Add user");
    const role = dlg.getByRole("combobox", { name: "Role", exact: true });
    await role.focus();
    await page.keyboard.press("ArrowDown");
    await expect(list(page)).toBeVisible();
    await expect(list(page).getByRole("option")).toHaveCount(3);
    const active = list(page).locator(".cb-opt.active");
    await expect(active).toHaveCount(1);
    await page.keyboard.press("Home");
    await expect(active).toHaveText("admin");
    await page.keyboard.press("ArrowDown");
    await expect(active).toHaveText("operator");
    await page.keyboard.press("ArrowUp");
    await expect(active).toHaveText("admin");
    await page.keyboard.type("v"); // type-ahead
    await expect(active).toHaveText("viewer");
    await page.keyboard.press("Enter");
    await expect(list(page)).toHaveCount(0);
    await expect(role).toContainText("viewer");
    await expect(role).toBeFocused();
    await expect(dlg).toBeVisible(); // Enter chose the option; it did not submit the form

    await role.press("Enter");
    await expect(list(page)).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(list(page)).toHaveCount(0);
    await expect(dlg).toBeVisible();
    await expect(role).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("clicking the field label opens the list; the label names the control; a click chooses", async ({ page }) => {
    await page.goto("/users");
    await page.getByRole("button", { name: "Add user" }).click();
    const dlg = dialog(page, "Add user");
    await dlg.getByText("Role", { exact: true }).click();
    await expect(list(page)).toBeVisible();
    await expect(list(page)).toHaveAccessibleName("Role list");
    await list(page).getByRole("option", { name: "operator" }).click();
    await expect(dlg.getByRole("combobox", { name: "Role", exact: true })).toContainText("operator");
    await pick(dlg, "Role", "admin");
    await expect(field(dlg, "Role")).toContainText("admin");
    // the list sits above the dialog, inside the window
    await dlg.getByRole("combobox", { name: "Role", exact: true }).click();
    const box = (await list(page).boundingBox())!;
    const vp = page.viewportSize()!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(vp.width);
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("a disabled option is shown but cannot be chosen", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await page.getByRole("button", { name: "Add interface" }).click();
    const dlg = dialog(page, "Add network interface");
    const net = dlg.getByRole("combobox", { name: "Network", exact: true });
    await expect(net).not.toHaveText(""); // the networks have loaded
    const before = await net.innerText();
    await net.click();
    const off = list(page).getByRole("option", { name: new RegExp(`^${NET} \\(inactive\\)`) });
    await expect(off).toHaveAttribute("aria-disabled", "true");
    await off.click({ force: true });
    await expect(list(page)).toBeVisible(); // still open: nothing was chosen
    await page.keyboard.press("Escape");
    await expect(net).toHaveText(before);
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("a long list has a search box that filters; Enter takes the active match", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await page.getByRole("button", { name: /^Advanced/ }).first().click();
    await pick(page, "CPU mode", "custom model");
    const model = page.getByRole("combobox", { name: "Network card", exact: true });
    await model.click();
    const search = page.getByRole("searchbox", { name: "Search model" });
    await expect(search).toBeFocused();
    const all = await list(page).getByRole("option").count();
    expect(all).toBeGreaterThan(8);
    await search.fill("qemu");
    const n = await list(page).getByRole("option").count();
    expect(n).toBeGreaterThan(0);
    expect(n).toBeLessThan(all);
    for (const t of await list(page).getByRole("option").allInnerTexts()) expect(t.toLowerCase()).toContain("qemu");
    await search.fill("no-such-cpu-model");
    await expect(list(page).getByRole("option")).toHaveCount(0);
    await expect(list(page)).toContainText("Nothing matches");
    await search.fill("qemu");
    const first = (await list(page).getByRole("option").first().innerText()).trim();
    await page.keyboard.press("Enter");
    await expect(list(page)).toHaveCount(0);
    await expect(model).toContainText(first);
  });

  test("editable combo: MAC suggestions with the VM name, filtering, free text, Escape", async ({ page, api, n1 }) => {
    const mac = ((await api.vmDetail(vm.node, vm.uuid)).nics[0].mac as string).toLowerCase();
    await page.goto("/networks");
    await pick(page, page.getByRole("combobox").first(), env.node1);
    await row(page, "default").first().click();
    const card = page.locator(".card", { hasText: "DHCP reservations" });
    const input = field(card, "MAC address");
    await input.click();
    const opt = list(page).getByRole("option", { name: new RegExp(mac) });
    await expect(opt).toContainText(VM);
    // filter as you type: by MAC fragment and by VM name
    await input.fill(mac.slice(-8));
    await expect(opt).toBeVisible();
    await input.fill(VM);
    await expect(opt).toBeVisible();
    await input.fill("zz:zz");
    await expect(list(page)).toHaveCount(0); // free text is allowed; nothing to suggest
    await expect(input).toHaveValue("zz:zz");
    // arrows and Enter take a suggestion
    await input.fill(VM);
    await input.press("ArrowDown");
    await expect(list(page).locator(".cb-opt.active")).toContainText(mac);
    await input.press("Enter");
    await expect(input).toHaveValue(mac);
    await expect(list(page)).toHaveCount(0);
    // Escape closes the list and leaves the text
    await input.fill(mac.slice(0, 8));
    await expect(list(page)).toBeVisible();
    await input.press("Escape");
    await expect(list(page)).toHaveCount(0);
    await expect(input).toHaveValue(mac.slice(0, 8));
    expect((await api.get(`/nodes/${n1}/networks/default`)).hosts.some((h: any) => h.mac === mac)).toBe(false); // nothing was saved
  });
});
