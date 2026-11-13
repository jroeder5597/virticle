// VM overview (summary and utilization cards), the Actions menu with its submenus, keyboard use,
// confirmation of destructive power actions, and the viewer role.
import { Browser } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { cancelConfirm, confirm, dialog, expectBadge } from "../lib/ui";

const NAME = `${P}ov1`;
const VIEWER = `${P}ovviewer`;
const PASS = "e2e-user-pass-123";

// Clone and Convert to template are single entries; the others open a submenu.
const TOP = ["Power", "Edit Settings", "Snapshots", "Clone", "Convert to template", "Migrate", "More"];
const SUBMENUS: Record<string, string[]> = {
  Power: ["Power On", "Reboot (Graceful)", "Shutdown (graceful)", "Pause", "Resume", "Halt (Force Shutdown)", "Reset (Hard Reboot)", "Sync guest clock"],
  "Edit Settings": ["CPU and memory", "Disks", "Network interfaces", "CD-ROM", "Boot order", "Firmware and TPM", "Passthrough devices"],
  Snapshots: ["Take snapshot", "Manage snapshots", "Schedule"],
  Migrate: ["Migrate to another node", "Move a disk to another pool"],
  More: ["Edit XML", "Delete VM"],
};

async function signInAs(browser: Browser, user: string, pass: string) {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base, viewport: { width: 1360, height: 860 } });
  const page = await ctx.newPage();
  await page.goto("/datacenter/vms");
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
  return { ctx, page };
}

test.describe.serial("overview and actions", () => {
  let vm: { node: number; uuid: string; name: string };
  const actions = (page: any) => page.locator(".head").getByRole("button", { name: "Actions", exact: true });
  const item = (page: any, name: string) => page.getByRole("menuitem", { name, exact: true });

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME, { start: true });
  });
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, NAME);
    for (const u of await api.get("/users")) if (u.username === VIEWER) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("summary and utilization cards: address, name, CPU, memory, storage", async ({ page }) => {
    await openVM(page, vm);
    const sum = page.locator("section#summary");
    await expect(sum).toContainText("Power State");
    await expect(sum.locator(".badge", { hasText: "running" })).toBeVisible();
    await expect(sum).toContainText("Guest OS");
    await expect(sum).toContainText("QEMU Guest Agent");
    // rows in the order requested
    const labels = await sum.locator("dt").allTextContents();
    expect(labels).toEqual(["Power State", "Guest OS", "QEMU Guest Agent", "DNS Name", "IP Addresses", "Passthrough"]);
    // Secure Boot and vTPM rows show only when enabled; this is a BIOS guest without host devices
    await expect(sum.locator("dd").nth(5)).toHaveText("None");
    // a cirros guest has no agent: the fallback text says so plainly
    await expect(sum.locator("dd").nth(3)).toHaveText("Not reported");
    await expect(sum.locator("dd").nth(3)).toHaveAttribute("title", "Needs the guest agent");
    await expect(sum.locator("dd").nth(1)).toContainText("Not reported");
    // the DHCP lease gives an address
    await expect(sum.locator("dd").nth(4)).toContainText(/\d+\.\d+\.\d+\.\d+/, { timeout: 90_000 });
    await expect(sum.locator("dd[title*='DHCP leases']")).toHaveCount(1); // the source is in the tooltip, not in the cell
    await expect(sum).not.toContainText("DHCP");

    const util = page.locator("section#utilization");
    await expect(util.locator(".st-val").nth(0)).toContainText(/\d%/, { timeout: 30_000 });
    await expect(util.locator(".st-val").nth(1)).toContainText(/\d%/);
    await expect(util).toContainText(/MiB of \d/);
    await expect(util.locator("svg.plot")).toHaveCount(2);
    await expect(util).toContainText("Disk read");
    await expect(util).toContainText("Network out");
    // storage on the host: allocation of the 1 GiB capacity
    const disk = util.locator(".fsbar").first();
    await expect(disk).toContainText("vda");
    await expect(disk).toContainText(/of 1\.0 GiB/);
    await expect(util.locator("span[title='Needs the guest agent']")).toHaveText("Not reported"); // guest filesystems
    // the old four-chart card is gone, and the console is a small preview in the same band
    await expect(page.locator(".charts")).toHaveCount(0);
    const band = await page.locator(".ov-band").boundingBox();
    const cc = await page.locator(".cc-box").boundingBox();
    expect(cc!.y).toBeLessThan(band!.y + 120);
    expect(cc!.width).toBeLessThan(400);
    // the host disk allocation is in the API too
    const d = await (await page.request.get(`/api/nodes/${vm.node}/vms/${vm.uuid}`)).json();
    expect(d.disks[0].allocation).toBeGreaterThan(0);
    expect(d.ipSource).toBe("lease");
  });

  test("every Actions submenu opens and lists the exact labels", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).click();
    await expect(actions(page)).toHaveAttribute("aria-expanded", "true");
    const top = await page.getByRole("menu").first().locator(":scope > .mi > button").allTextContents();
    expect(top).toEqual(TOP);
    for (const [group, labels] of Object.entries(SUBMENUS)) {
      const parent = item(page, group);
      await expect(parent).toHaveAttribute("aria-haspopup", "menu");
      await parent.click();
      await expect(parent).toHaveAttribute("aria-expanded", "true");
      const sub = page.locator(".mi", { has: parent }).getByRole("menu");
      await expect(sub.getByRole("menuitem")).toHaveCount(labels.length);
      expect(await sub.getByRole("menuitem").allTextContents()).toEqual(labels);
      // the submenu stays inside the window
      const box = await sub.boundingBox();
      const vp = page.viewportSize()!;
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
      expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height);
    }
    // a running VM: entries that do not apply are disabled with a reason, not hidden
    await item(page, "Power").click();
    await expect(item(page, "Power On")).toBeDisabled();
    await expect(item(page, "Resume")).toBeDisabled();
    await expect(item(page, "Halt (Force Shutdown)")).toBeEnabled();
    await item(page, "More").click();
    await expect(item(page, "Delete VM")).toBeDisabled();
    await expect(item(page, "Delete VM")).toHaveAttribute("title", /Shut the VM down/);
    await expect(item(page, "Clone")).toBeDisabled();
    await expect(item(page, "Clone")).toHaveAttribute("title", /Shut the VM down to clone/);
    // a running VM without the guest agent cannot reset its identity, so it cannot be converted either
    await expect(item(page, "Convert to template")).toBeDisabled();
    // Escape steps back out of a submenu (focus is on its first enabled entry), a second one closes the menu
    await expect(item(page, "Edit XML")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(0);
  });

  test("Edit Settings entries open a dialog on the current tab; Snapshots entries open the tab", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).click();
    await item(page, "Edit Settings").click();
    await item(page, "Disks").click();
    await expect(dialog(page, "Disks")).toBeVisible();
    await expect(page).not.toHaveURL(/tab=/);
    await page.keyboard.press("Escape");
    await vmActionOnPage(page, "Edit Settings", "Boot order");
    await expect(dialog(page, "Boot order")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
    await vmActionOnPage(page, "Snapshots", "Schedule");
    await expect(page).toHaveURL(/tab=snapshots/);
    await expect(page.locator("#snap-schedule")).toBeInViewport();
    await vmActionOnPage(page, "Snapshots", "Take snapshot");
    await expect(dialog(page)).toBeVisible();
    await page.keyboard.press("Escape");
    await vmActionOnPage(page, "Snapshots", "Manage snapshots");
    await expect(page).toHaveURL(/tab=snapshots/);
  });

  test("the XML editor is not a tab; Actions, More, Edit XML opens it and the old link works", async ({ page }) => {
    await openVM(page, vm);
    await expect(page.locator(".tabs").getByRole("button", { name: "XML" })).toHaveCount(0);
    await vmActionOnPage(page, "More", "Edit XML");
    await expect(page).toHaveURL(/tab=xml/);
    await expect(page.getByLabel("Domain XML")).toBeVisible();
    await page.goto(`/vm/${vm.node}/${vm.uuid}?tab=xml`);
    await expect(page.getByLabel("Domain XML")).toBeVisible();
  });

  test("keyboard: Enter opens, arrows move, Right opens a submenu, Left and Escape close, focus returns", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByRole("menu")).toBeVisible();
    await expect(item(page, "Power")).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await expect(item(page, "Edit Settings")).toBeFocused();
    await page.keyboard.press("ArrowUp");
    await expect(item(page, "Power")).toBeFocused();
    await page.keyboard.press("End");
    await expect(item(page, "More")).toBeFocused();
    await page.keyboard.press("Home");
    await page.keyboard.press("ArrowRight");
    await expect(item(page, "Power")).toHaveAttribute("aria-expanded", "true");
    // Power On is disabled on a running VM: focus lands on the first entry that can be used
    await expect(item(page, "Reboot (Graceful)")).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await expect(item(page, "Shutdown (graceful)")).toBeFocused();
    await page.keyboard.press("ArrowLeft");
    await expect(item(page, "Power")).toBeFocused();
    await expect(item(page, "Power")).toHaveAttribute("aria-expanded", "false");
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("Escape"); // closes the submenu only
    await expect(page.getByRole("menu")).toBeVisible();
    await expect(item(page, "Power")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("menu")).toHaveCount(0);
    await expect(actions(page)).toBeFocused();
    await expect(actions(page)).toHaveAttribute("aria-expanded", "false");
    // Space on a submenu parent opens it too
    await page.keyboard.press("ArrowDown");
    await item(page, "Power").press("Space");
    await expect(item(page, "Power")).toHaveAttribute("aria-expanded", "true");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  });

  test("a destructive power action asks first: Cancel does nothing, Confirm does it", async ({ page, api }) => {
    await openVM(page, vm);
    await actions(page).click();
    await item(page, "Power").click();
    await item(page, "Halt (Force Shutdown)").click();
    const dlg = dialog(page, "Halt (Force Shutdown)");
    await expect(dlg).toBeVisible();
    await expect(dlg).toContainText(NAME);
    await expect(dlg.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
    await expect(dlg.getByRole("button", { name: "Halt", exact: true })).toHaveClass(/danger/);
    // Enter must not confirm: with Cancel focused it cancels
    await page.keyboard.press("Enter");
    await expect(dlg).toBeHidden();
    await page.waitForTimeout(2000);
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("running");
    // Escape cancels
    await actions(page).click();
    await item(page, "Power").click();
    await item(page, "Reboot (Graceful)").click();
    await expect(dialog(page, "Reboot (Graceful)")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
    // Cancel button
    await actions(page).click();
    await item(page, "Power").click();
    await item(page, "Reset (Hard Reboot)").click();
    await cancelConfirm(page, "Reset (Hard Reboot)");
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("running");
    // Confirm does it
    await actions(page).click();
    await item(page, "Power").click();
    await item(page, "Halt (Force Shutdown)").click();
    await confirm(page, "Halt (Force Shutdown)");
    await expectBadge(page.locator(".head"), "shutoff");
    expect((await api.vmDetail(vm.node, vm.uuid)).state).toBe("shutoff");
    // shut off: the menu keeps its shape, with other entries enabled
    await actions(page).click();
    await item(page, "Power").click();
    await expect(item(page, "Power On")).toBeEnabled();
    await expect(item(page, "Halt (Force Shutdown)")).toBeDisabled();
    await item(page, "More").click();
    await expect(item(page, "Delete VM")).toBeEnabled();
    await expect(item(page, "Clone")).toBeEnabled();
    await expect(item(page, "Convert to template")).toBeEnabled();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    // the overview of a shut-off VM keeps its band
    await expect(page.locator("#utilization .st-val").first()).toHaveText("–");
    await expect(page.locator(".cc-box")).toBeVisible();
  });

  test("viewer: the Actions menu has no enabled entry that changes anything", async ({ browser, api }) => {
    await api.post("/users", { username: VIEWER, password: PASS, role: "viewer" });
    const { ctx, page } = await signInAs(browser, VIEWER, PASS);
    await page.goto(`/vm/${vm.node}/${vm.uuid}`);
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();
    await actions(page).click();
    for (const g of ["Power", "Edit Settings", "Clone", "Convert to template", "Migrate"]) await expect(item(page, g)).toBeDisabled();
    await item(page, "More").click();
    await expect(item(page, "Edit XML")).toBeEnabled();
    await expect(item(page, "Delete VM")).toBeDisabled();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await actions(page).click();
    await item(page, "Snapshots").click();
    await expect(item(page, "Take snapshot")).toBeDisabled();
    await expect(item(page, "Manage snapshots")).toBeEnabled();
    await expect(item(page, "Schedule")).toBeEnabled();
    await ctx.close();
  });
});

async function vmActionOnPage(page: any, group: string, entry: string) {
  const btn = page.locator(".head").getByRole("button", { name: "Actions", exact: true });
  if ((await btn.getAttribute("aria-expanded")) !== "true") await btn.click();
  await page.getByRole("menuitem", { name: group, exact: true }).click();
  await page.getByRole("menuitem", { name: entry, exact: true }).click();
}
