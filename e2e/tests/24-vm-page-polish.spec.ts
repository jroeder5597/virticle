// VM page polish: Edit Settings dialogs that share state with the Hardware tab, the Power submenu order, the
// snapshot type help tip, the aligned Hardware tab with collapsed sections, the pool picker, the autostart box.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, expectBadge, field } from "../lib/ui";
import { poolLevel } from "../../web/src/poolLevel";

const NAME = `${P}pp1`;
const POOL = `${P}pool24`;

const AREAS = ["CPU and memory", "Disks", "Network interfaces", "CD-ROM", "Boot order", "Firmware and TPM", "Advanced", "Passthrough devices"];

test.describe("pool level (pure)", () => {
  test("below warn, between, at and above critical, zero capacity", () => {
    expect(poolLevel(10, 100)).toEqual({ pct: 10, level: "ok" });
    expect(poolLevel(84.9, 100).level).toBe("ok");
    expect(poolLevel(85, 100).level).toBe("warn");
    expect(poolLevel(94, 100).level).toBe("warn");
    expect(poolLevel(95, 100).level).toBe("crit");
    expect(poolLevel(100, 100).level).toBe("crit");
    expect(poolLevel(120, 100).pct).toBe(100);
    expect(poolLevel(50, 100, 40, 60).level).toBe("warn");
    expect(poolLevel(0, 0)).toEqual({ pct: 0, level: "ok" });
    expect(poolLevel(5, 0)).toEqual({ pct: 0, level: "ok" });
  });
});

test.describe.serial("VM page polish", () => {
  let vm: { node: number; uuid: string; name: string };
  const actions = (page: any) => page.locator(".head").getByRole("button", { name: "Actions", exact: true });
  const item = (page: any, name: string) => page.getByRole("menuitem", { name, exact: true });
  const edit = async (page: any, entry: string) => {
    await actions(page).click();
    await item(page, "Edit Settings").click();
    await item(page, entry).click();
  };
  const card = (page: any, title: string) => page.locator("section.card").filter({ has: page.getByRole("heading", { name: title, exact: true }) });

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    await api.post(`/nodes/${n1}/pools`, { name: POOL, type: "dir", target: `/var/lib/libvirt/${POOL}` });
  });
  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME);
    for (const v of await api.get(`/nodes/${n1}/pools/${POOL}/volumes`).catch(() => [])) await api.del(`/nodes/${n1}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
    await api.del(`/nodes/${n1}/pools/default/volumes/${NAME}-d1.qcow2`).catch(() => {});
    await api.post(`/nodes/${n1}/pools/${POOL}/action`, { action: "remove" }).catch(() => {});
  });

  test("each Edit Settings entry opens a dialog over a dimmed backdrop; URL and tab stay", async ({ page }) => {
    await openVM(page, vm);
    const url = page.url();
    for (const area of AREAS) {
      await edit(page, area);
      const dlg = dialog(page, area);
      await expect(dlg).toBeVisible();
      const bg = await page.locator(".backdrop").first().evaluate((e) => getComputedStyle(e).backgroundColor);
      expect(bg).not.toBe("rgba(0, 0, 0, 0)");
      expect(bg).not.toBe("transparent");
      expect(page.url()).toBe(url);
      await expect(page.locator(".tabs .on")).toHaveText("Overview");
      // the dialog fits the window and scrolls inside when tall
      const box = await dlg.boundingBox();
      const vp = page.viewportSize()!;
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
      await page.keyboard.press("Escape");
      await expect(dialog(page)).toHaveCount(0);
    }
  });

  test("memory changed in the dialog shows on the Hardware tab behind it and on the overview, and the other way round", async ({ page, api }) => {
    await openVM(page, vm, "hardware");
    await edit(page, "CPU and memory");
    const dlg = dialog(page, "CPU and memory");
    await field(dlg, "Maximum memory (MiB)").fill("256");
    await field(dlg, "Memory now (MiB)").fill("192");
    await dlg.getByRole("button", { name: "Apply" }).click();
    await expect(dlg).toContainText("Saved.");
    await expect(card(page, "CPU and memory")).toContainText("192 MiB"); // the tab, behind the dialog, without a reload
    await page.keyboard.press("Escape");
    expect((await api.vmDetail(vm.node, vm.uuid)).currentMemoryKiB).toBe(192 * 1024);
    await page.locator(".tabs").getByRole("button", { name: "Overview" }).click();
    const ov = card(page, "Hardware");
    await expect(ov.locator("dt", { hasText: "Memory" }).locator("+ dd")).toHaveText("256 MiB");
    // the other way: change on the tab, reopen the dialog
    await page.locator(".tabs").getByRole("button", { name: "Hardware" }).click();
    const c = card(page, "CPU and memory");
    await c.getByRole("button", { name: "Edit" }).click();
    await field(c, "Memory now (MiB)").fill("224");
    await c.getByRole("button", { name: "Apply" }).click();
    await expect(c).toContainText("224 MiB");
    await edit(page, "CPU and memory");
    await expect(field(dialog(page, "CPU and memory"), "Memory now (MiB)")).toHaveValue("224");
    await page.keyboard.press("Escape");
  });

  test("Disks dialog: nested Add disk, Escape closes only the top dialog, the new disk shows in both", async ({ page, api, n1 }) => {
    await openVM(page, vm, "hardware");
    await edit(page, "Disks");
    const disks = dialog(page, "Disks");
    await disks.getByRole("button", { name: "Add disk" }).click();
    await expect(dialog(page, "Add disk")).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page, "Add disk")).toHaveCount(0);
    await expect(disks).toBeVisible();
    await disks.getByRole("button", { name: "Add disk" }).click();
    const add = dialog(page, "Add disk");
    await field(add, "Volume name").fill(`${NAME}-d1.qcow2`);
    await field(add, "Size (GiB)").fill("1");
    await add.getByRole("button", { name: "Add disk" }).click();
    await expect(add).toBeHidden();
    await expect(disks.locator("tbody tr", { hasText: `${NAME}-d1.qcow2` })).toBeVisible();
    await expect(card(page, "Disks").locator("tbody tr", { hasText: `${NAME}-d1.qcow2` })).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
    expect((await api.vmDetail(vm.node, vm.uuid)).disks.some((k: any) => k.source?.endsWith(`${NAME}-d1.qcow2`))).toBe(true);
  });

  test("pool picker: percent, used of size and a bar per pool; keyboard choice is what the request sends", async ({ page }) => {
    await openVM(page, vm, "hardware");
    await page.getByRole("button", { name: "Add disk" }).click();
    const dlg = dialog(page, "Add disk");
    const combo = dlg.getByRole("combobox", { name: "Storage pool" });
    await expect(combo).toContainText(/\d+% used/);
    await expect(combo).toContainText(/ used of /);
    await expect(combo.locator(".ps-bar")).toBeVisible();
    await combo.click();
    const list = page.getByRole("listbox");
    const opts = list.getByRole("option");
    expect(await opts.count()).toBeGreaterThanOrEqual(2);
    for (let i = 0; i < (await opts.count()); i++) {
      const o = opts.nth(i);
      await expect(o).toContainText(/\d+% used|inactive/);
      await expect(o).toContainText(/ used of /);
      await expect(o.locator(".ps-bar")).toHaveCount(1);
    }
    // the list stays inside the window
    const box = await list.boundingBox();
    const vp = page.viewportSize()!;
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
    expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height);
    // Escape closes the list only
    await page.keyboard.press("Escape");
    await expect(list).toHaveCount(0);
    await expect(dlg).toBeVisible();
    // keyboard: open, type-ahead to the new pool, Enter
    await combo.focus();
    await page.keyboard.press("ArrowDown");
    await expect(page.getByRole("listbox")).toBeVisible();
    await page.keyboard.type(POOL);
    await expect(page.getByRole("option", { name: new RegExp(POOL) })).toHaveClass(/active/);
    await page.keyboard.press("Enter");
    await expect(page.getByRole("listbox")).toHaveCount(0);
    await expect(combo).toContainText(POOL);
    // a size no pool has room for is called out, and the pool stays selectable
    await field(dlg, "Volume name").fill(`${NAME}-d2.qcow2`);
    await field(dlg, "Size (GiB)").fill("60000");
    await expect(combo).toContainText("not enough free space");
    await field(dlg, "Size (GiB)").fill("1");
    await expect(combo).not.toContainText("not enough free space");
    const req = page.waitForRequest((r) => r.method() === "POST" && /\/disks$/.test(r.url()));
    await dlg.getByRole("button", { name: "Add disk" }).click();
    expect((await req).postDataJSON().pool).toBe(POOL);
    await expect(dlg).toBeHidden();
    await expect(card(page, "Disks").locator("tbody tr", { hasText: `${POOL}/${NAME}-d2.qcow2` })).toBeVisible();
  });

  test("the Power submenu has the nine entries and separators in the requested order", async ({ page }) => {
    await openVM(page, vm);
    await actions(page).click();
    const parent = item(page, "Power");
    await parent.click();
    const sub = page.locator(".mi", { has: parent }).getByRole("menu");
    const got = await sub.locator(":scope > *").evaluateAll((els) => els.map((e) => (e.getAttribute("role") === "separator" ? "-" : (e.textContent ?? "").trim())));
    expect(got).toEqual(["Power On", "Reboot (Graceful)", "Shutdown (graceful)", "-", "Pause", "Resume", "-", "Halt (Force Shutdown)", "Reset (Hard Reboot)"]);
    expect(got.join("|")).not.toContain("force off after");
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
  });

  test("autostart: the box follows the click at once, no Yes or No text", async ({ page, api }) => {
    await openVM(page, vm);
    const url = page.url();
    const box = page.getByRole("checkbox", { name: "Start on host boot" });
    await expect(box).not.toBeChecked();
    await box.click();
    await expect(box).toBeChecked();
    await expect.poll(async () => (await api.vmDetail(vm.node, vm.uuid)).autostart).toBe(true);
    expect(page.url()).toBe(url);
    const dd = page.locator("dt", { hasText: "Start on host boot" }).locator("+ dd");
    await expect(dd).not.toContainText(/Yes|No/);
    await box.click();
    await expect(box).not.toBeChecked();
    await expect.poll(async () => (await api.vmDetail(vm.node, vm.uuid)).autostart).toBe(false);
  });

  test("Hardware tab: cards do not overlap, columns line up, gaps are equal, the stacks end level", async ({ page }) => {
    await page.setViewportSize({ width: 1360, height: 900 });
    await openVM(page, vm, "hardware");
    await expect(page.locator("#hw-advanced")).toBeVisible();
    const boxes = await page.locator(".hw .card").evaluateAll((els) => els.map((e) => ({ id: e.id, ...e.getBoundingClientRect().toJSON() })));
    expect(boxes.length).toBe(10);
    for (let i = 0; i < boxes.length; i++)
      for (let j = i + 1; j < boxes.length; j++) {
        const a = boxes[i], b = boxes[j];
        const overlap = Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 && Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
        expect(overlap, `${a.id} overlaps ${b.id}`).toBe(false);
      }
    const lefts = [...new Set(boxes.map((b) => Math.round(b.left)))];
    expect(lefts.length, `left edges ${lefts}`).toBeLessThanOrEqual(2);
    const left = Math.min(...lefts);
    const right = Math.max(...boxes.map((b) => b.right));
    // full-width cards start at the first column and end at the same right edge
    for (const id of ["hw-disks", "hw-nics", "hw-advanced", "hw-passthrough"]) {
      const b = boxes.find((x) => x.id === id)!;
      expect(Math.abs(b.left - left)).toBeLessThan(2);
      expect(Math.abs(b.right - right)).toBeLessThan(2);
    }
    // equal gaps between cards that are above each other
    const gaps: number[] = [];
    const sorted = [...boxes].sort((a, b) => a.top - b.top);
    for (const b of sorted) {
      const above = sorted.filter((a) => a !== b && a.bottom <= b.top + 1 && Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1);
      if (above.length) gaps.push(Math.round(b.top - Math.max(...above.map((a) => a.bottom))));
    }
    expect(gaps.length).toBeGreaterThan(4);
    for (const g of gaps) expect(Math.abs(g - gaps[0]), `gaps ${gaps}`).toBeLessThanOrEqual(1);
    // the two stacks of the top block end level: no hole under the shorter one
    const bottoms = ["hw-serial", "hw-clock"].map((id) => boxes.find((x) => x.id === id)!.bottom);
    expect(Math.abs(bottoms[0] - bottoms[1])).toBeLessThan(2);
    // no card spills out of the page area
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
  });

  test("collapsed sections show a summary, expand, and remember their state", async ({ page }) => {
    await openVM(page, vm, "hardware");
    const adv = page.locator("#hw-advanced");
    const pass = page.locator("#hw-passthrough");
    await expect(adv).toContainText("No pinning, ordinary pages, default NUMA policy");
    await expect(pass).toContainText("No host devices");
    await expect(page.locator("#hw-advanced-body")).toHaveCount(0);
    await adv.getByRole("button").first().click();
    await expect(page.locator("#hw-advanced-body")).toBeVisible();
    await expect(page.locator("#hw-advanced-body")).toContainText("vCPU pinning");
    await page.reload();
    await expect(page.locator("#hw-advanced-body")).toBeVisible();
    await expect(page.locator("#hw-passthrough-body")).toHaveCount(0);
    await adv.getByRole("button").first().click();
    await expect(page.locator("#hw-advanced-body")).toHaveCount(0);
    await page.reload();
    await expect(page.locator("#hw-advanced")).toBeVisible();
    await expect(page.locator("#hw-advanced-body")).toHaveCount(0);
  });

  test("the Advanced and Passthrough dialogs show their content without a collapsed header", async ({ page }) => {
    await openVM(page, vm);
    await edit(page, "Advanced");
    await expect(dialog(page, "Advanced")).toContainText("vCPU pinning");
    await page.keyboard.press("Escape");
    await edit(page, "Passthrough devices");
    await expect(dialog(page, "Passthrough devices")).toContainText("No host devices");
    await page.keyboard.press("Escape");
  });

  test("Add CD-ROM drive stays available up to four drives", async ({ page, api }) => {
    for (let i = 0; i < 2; i++) await api.post(`${"/nodes/" + vm.node}/vms/${vm.uuid}/cdrom`);
    await openVM(page, vm, "hardware");
    const c = card(page, "CD-ROM");
    await expect(c.locator("tbody tr")).toHaveCount(2);
    await c.getByRole("button", { name: "Add CD-ROM drive" }).click();
    await expect(c.locator("tbody tr")).toHaveCount(3);
    await c.getByRole("button", { name: "Add CD-ROM drive" }).click();
    await expect(c.locator("tbody tr")).toHaveCount(4);
    await expect(c.getByRole("button", { name: "Add CD-ROM drive" })).toHaveCount(0);
  });

  test("snapshot help tip opens on hover and on focus and names all three types", async ({ page, api }) => {
    await api.post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: "start" });
    await openVM(page, vm);
    await expectBadge(page.locator(".head"), "running");
    await actions(page).click();
    await item(page, "Snapshots").click();
    await item(page, "Take snapshot").click();
    const dlg = dialog(page, "Take snapshot");
    const help = dlg.getByRole("button", { name: "About snapshot types" });
    await expect(help).toBeVisible();
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await help.hover();
    const tip = page.getByRole("tooltip");
    await expect(tip).toContainText("Automatic");
    await expect(tip).toContainText("Internal");
    await expect(tip).toContainText("External");
    const box = await tip.boundingBox();
    const vp = page.viewportSize()!;
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp.width);
    expect(box!.y + box!.height).toBeLessThanOrEqual(vp.height);
    await page.mouse.move(5, 5);
    await expect(tip).toHaveCount(0);
    await help.focus();
    await expect(page.getByRole("tooltip")).toContainText("External");
    // Escape closes the tip, not the dialog
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);
    await expect(dlg).toBeVisible();
    await help.click();
    await expect(page.getByRole("tooltip")).toBeVisible();
    await page.keyboard.press("Escape");
    await page.keyboard.press("Escape");
    await expect(dialog(page)).toHaveCount(0);
  });

  test("a graphics card cannot be attached while the VM runs, and the reason is shown", async ({ page }) => {
    // the VM is running (the previous test started it); the node has no GPU, so the listing is made up
    await page.route(/\/hostdevs$/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const dev = (id: string, category: string) => ({ kind: "pci", id, nodedev: "pci_" + id, category, vendor: "Vendor", product: category, vendorId: "10de", productId: "1234", iommuGroup: 1, usedBy: [], unsafe: false });
      await route.fulfill({ json: { devices: [dev("0000:0a:00.0", "gpu"), dev("0000:0b:00.0", "storage")], attached: [], iommu: true, factsKnown: true, vm: vm.uuid, warning: "" } });
    });
    await openVM(page, vm, "hardware");
    await page.getByRole("button", { name: /^Passthrough devices/ }).click();
    await page.getByRole("button", { name: "Add host device" }).click();
    const dlg = dialog(page, "Host devices of this node");
    const gpu = dlg.locator("tr", { hasText: "0000:0a:00.0" });
    await expect(gpu.getByRole("button", { name: "Attach" })).toBeDisabled();
    await expect(gpu).toContainText("Shut the VM down to attach a graphics card");
    await expect(gpu.getByRole("button", { name: "Attach" })).toHaveAttribute("title", "Shut the VM down to attach a graphics card");
    await expect(dlg.locator("tr", { hasText: "0000:0b:00.0" }).getByRole("button", { name: "Attach" })).toBeEnabled();
    await page.keyboard.press("Escape");
    await page.evaluate(() => localStorage.removeItem("virticle.hw.open.hw-passthrough"));
  });
});
