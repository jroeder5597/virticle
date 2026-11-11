// One visual pass: both themes (dark is the default; light is chosen through the app's own theme
// setting, not the OS preference) at desktop and phone width over every page and tab.
// Screenshots go to test-results/visual/ for a human to look at; the automatic checks catch
// unreadable text (contrast) and pages that scroll sideways.
import { Page } from "@playwright/test";
import { mkdirSync } from "fs";
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { field, pick, row } from "../lib/ui";

const NAME = `${P}vis1`;
const OUT = "test-results/visual";
// Coloured tags on the screenshot VM: a very light and a very dark colour must both stay readable on both themes.
const T_LIGHT = `${P}vis-light`;
const T_DARK = `${P}vis-dark`;
const T_LONG = `${P}vis-a-rather-long-tag-name`;
const T_PLAIN = `${P}vis-plain`;

/** Finds visible text whose colour is too close to the background it sits on. */
async function lowContrast(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const parse = (c: string) => {
      const m = c.match(/rgba?\(([^)]+)\)/);
      if (!m) return null;
      const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number);
      return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
    };
    const lum = (c: { r: number; g: number; b: number }) => {
      const f = (v: number) => {
        v /= 255;
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
      };
      return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
    };
    const bgOf = (el: Element) => {
      let layers: { r: number; g: number; b: number; a: number }[] = [];
      for (let e: Element | null = el; e; e = e.parentElement) {
        const c = parse(getComputedStyle(e).backgroundColor);
        if (c && c.a > 0) {
          layers.push(c);
          if (c.a >= 1) break;
        }
      }
      let base = { r: 255, g: 255, b: 255 };
      const bodyBg = parse(getComputedStyle(document.body).backgroundColor);
      if (bodyBg && bodyBg.a > 0) base = bodyBg;
      for (const l of layers.reverse()) base = { r: l.r * l.a + base.r * (1 - l.a), g: l.g * l.a + base.g * (1 - l.a), b: l.b * l.a + base.b * (1 - l.a) };
      return base;
    };
    const bad = new Map<string, number>();
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const t = (n.textContent ?? "").trim();
      const el = n.parentElement;
      if (!t || !el) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === "hidden" || cs.display === "none" || Number(cs.opacity) === 0) continue;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      if (el.closest("canvas, .xterm, .console, svg, [disabled], :disabled")) continue;
      const fg = parse(cs.color);
      if (!fg) continue;
      const bg = bgOf(el);
      const eff = { r: fg.r * fg.a + bg.r * (1 - fg.a), g: fg.g * fg.a + bg.g * (1 - fg.a), b: fg.b * fg.a + bg.b * (1 - fg.a) };
      const L1 = lum(eff), L2 = lum(bg);
      const ratio = (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05);
      if (ratio < 3) bad.set(`${t.slice(0, 40)} [${el.tagName.toLowerCase()}.${el.className}] ratio ${ratio.toFixed(2)}`, ratio);
    }
    return [...bad.keys()].slice(0, 20);
  });
}

async function sideways(page: Page): Promise<number> {
  // The main area scrolls inside the app shell, so sideways overflow shows there, not on the document.
  return page.evaluate(() => {
    const c = document.querySelector(".content-scroll");
    return Math.max(document.documentElement.scrollWidth - window.innerWidth, c ? c.scrollWidth - c.clientWidth : 0);
  });
}

/** Opens or collapses the Tasks dock (the choice is stored for the user). */
async function dock(page: Page, open: boolean) {
  const b = page.getByRole("button", { name: open ? "Show tasks" : "Hide tasks" });
  if (await b.count()) await b.click();
  await expect(page.getByRole("button", { name: open ? "Hide tasks" : "Show tasks" })).toHaveAttribute("aria-expanded", String(open));
}

/** Desktop only: elements whose content is wider than their box (cut off or scrolling inside), or that end past the window. */
async function clipped(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    const vw = window.innerWidth;
    for (const el of Array.from(document.querySelectorAll<HTMLElement>("body *"))) {
      if (el.closest("pre, textarea, code, canvas, svg, .xterm, .console, select, input, .tgbox")) continue;
      const cs = getComputedStyle(el);
      if (cs.display === "none" || cs.visibility === "hidden") continue;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      const id = `${el.tagName.toLowerCase()}.${String(el.className).slice(0, 40)} "${(el.textContent ?? "").trim().slice(0, 30)}"`;
      // cut off by an ancestor that hides or scrolls its overflow (a table with rounded corners, a card)
      let cut = "";
      for (let a = el.parentElement; a && a !== document.body; a = a.parentElement) {
        if (getComputedStyle(a).overflowX === "visible") continue;
        const ar = a.getBoundingClientRect();
        if (r.right > ar.right + 1 && r.width > 0) cut = `${a.tagName.toLowerCase()}.${String(a.className).slice(0, 30)}`;
        break;
      }
      // spilling out of its card or dialog onto whatever is next to it
      const box = el.closest(".modal") ?? (el.closest(".backdrop") ? null : el.closest(".card")); // dialogs render inside the card that opened them
      if (!cut && box && box !== el && r.right > box.getBoundingClientRect().right + 1) cut = "its card";
      if (cut && cs.textOverflow !== "ellipsis") out.push(`cut off by ${cut}: ${id}`);
      else if (r.right > vw + 1) out.push(`ends ${Math.round(r.right - vw)}px past the window: ${id}`);
      else if (el.scrollWidth > el.clientWidth + 1 && cs.overflowX !== "visible" && cs.textOverflow !== "ellipsis") out.push(`content ${el.scrollWidth - el.clientWidth}px wider than its box: ${id}`);
    }
    return [...new Set(out)].slice(0, 8);
  });
}

/** The user's rule: no ellipsis in labels, placeholders or status text. */
async function ellipses(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const bad = new Set<string>();
    const has = (t: string) => t.includes("\u2026") || t.includes("...");
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      const t = (n.textContent ?? "").trim();
      if (t && has(t) && !n.parentElement?.closest(".xterm, pre, code, textarea, script, style, .tmore")) bad.add(t.slice(0, 60));
    }
    for (const el of Array.from(document.querySelectorAll("[placeholder], [title], [aria-label]")))
      for (const a of ["placeholder", "title", "aria-label"]) {
        const t = el.getAttribute(a) ?? "";
        if (has(t)) bad.add(`${a}: ${t.slice(0, 60)}`);
      }
    return [...bad];
  });
}

const modes = [
  { name: "dark", scheme: "dark" as const, viewport: { width: 1280, height: 800 } },
  { name: "light", scheme: "light" as const, viewport: { width: 1440, height: 900 } },
  { name: "narrow", scheme: "light" as const, viewport: { width: 420, height: 860 } },
  { name: "narrow-dark", scheme: "dark" as const, viewport: { width: 420, height: 860 } },
];

test.describe.serial("visual pass", () => {
  let vm: { node: number; uuid: string; name: string };
  let snapVm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    mkdirSync(OUT, { recursive: true });
    vm = await createCirros(api, n1, NAME);
    await api.put(`/nodes/${vm.node}/vms/${vm.uuid}/tags`, { tags: [T_LIGHT, T_DARK, T_PLAIN, T_LONG] });
    await api.put(`/tags/${encodeURIComponent(T_LIGHT)}/color`, { color: "#fafafa" });
    await api.put(`/tags/${encodeURIComponent(T_DARK)}/color`, { color: "#0a0f14" });
    // a shut-off VM with a branched snapshot tree: a -> b -> c, back to a, then d
    snapVm = await createCirros(api, n1, `${P}vsnap1`);
    const sb = `/nodes/${n1}/vms/${snapVm.uuid}`;
    for (const n of ["a", "b", "c"]) await api.post(`${sb}/snapshots`, { name: n, description: `snapshot ${n}` });
    await api.post(`${sb}/snapshots/a/revert`, {});
    await api.post(`${sb}/snapshots`, { name: "scheduled-2026-11-11T03-00-00Z-daily", description: "A long description to see how long user text is treated in the tree and in the dialog" });
    // two disks and two NICs, so the Hardware tab is shown with its tables filled
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/disks`, { pool: "default", bus: "virtio", newVolume: { name: `${NAME}-data.qcow2`, sizeGiB: 1, format: "qcow2" } });
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/nics`, { network: "default", model: "virtio" });
    // both are added while the VM is off (a running guest has no free PCI slot for the second), then it starts
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/action`, { action: "start" });
    await api.post("/cloudinit-profiles", { name: `${P}visprof`, description: "visual pass", user: "admin", password: "e2e-vis-pass-123", packages: ["qemu-guest-agent"], default: true });
  });
  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME, `${P}vsnap1`);
    await api.del(`/nodes/${n1}/pools/default/volumes/${NAME}-data.qcow2`).catch(() => {});
    for (const t of [T_LIGHT, T_DARK]) await api.del(`/tags/${encodeURIComponent(t)}/color`).catch(() => {});
    for (const p of (await api.get("/cloudinit-profiles")).profiles) if (p.name === `${P}visprof`) await api.del(`/cloudinit-profiles/${p.id}`).catch(() => {});
  });

  for (const m of modes) {
    test(`every page and tab: ${m.name}`, async ({ browser }) => {
      test.setTimeout(240_000);
      const ctx = await browser.newContext({ storageState: ".auth/admin.json", viewport: m.viewport });
      // The theme no longer follows the OS: pick it the way the toggle does (localStorage, read before first paint).
      await ctx.addInitScript((t) => localStorage.setItem("virticle-theme", t), m.scheme);
      const page = await ctx.newPage();
      const problems: string[] = [];
      const shot = async (name: string, ready: () => Promise<unknown>, opts: { contrast?: boolean; wide?: boolean; viewportOnly?: boolean } = {}) => {
        await ready();
        await page.waitForTimeout(400);
        await page.screenshot({ path: `${OUT}/${m.name}-${name}.png`, fullPage: !opts.viewportOnly });
        const issues = await lowContrast(page);
        for (const i of issues) problems.push(`${m.name}/${name}: low contrast: ${i}`);
        for (const e of await ellipses(page)) problems.push(`${m.name}/${name}: ellipsis in "${e}"`);
        // every dropdown is a themed one: no browser-native select or datalist anywhere
        const native = await page.locator("select, datalist").count();
        if (native) problems.push(`${m.name}/${name}: ${native} native select/datalist element(s)`);
        const over = await sideways(page);
        if (over > 1 && !opts.wide) problems.push(`${m.name}/${name}: page scrolls sideways by ${over}px`);
        if (m.viewport.width >= 1000 && !opts.wide) for (const c of await clipped(page)) problems.push(`${m.name}/${name}: ${c}`);
      };
      const vmUrl = `/vm/${vm.node}/${vm.uuid}`;
      const heading = (n: string | RegExp) => page.getByRole("heading", { name: n }).first();

      await page.goto("/datacenter/vms");
      expect(await page.evaluate(() => document.documentElement.getAttribute("data-theme"))).toBe(m.scheme);
      await shot("list", () => expect(page.locator("tbody tr", { hasText: NAME })).toBeVisible());
      await dock(page, true);
      await shot("dock-open", () => expect(page.locator(".dk-rows .dk-row").first()).toBeVisible({ timeout: 30_000 }), { viewportOnly: true });
      await dock(page, false);
      await shot("dock-collapsed", () => expect(page.locator(".dock-table")).toHaveCount(0), { viewportOnly: true });
      await dock(page, true);
      await page.getByRole("button", { name: "New VM" }).click();
      await shot("new-vm-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.getByRole("dialog").getByRole("button", { name: "Disk image" }).click();
      await shot("new-vm-cloudinit-profile", () => expect(page.getByRole("dialog").getByText("Profile", { exact: true })).toBeVisible());
      // a themed Select open inside the dialog; Escape closes the list and leaves the dialog
      await page.getByRole("dialog").getByRole("combobox", { name: "Network", exact: true }).click();
      await shot("new-vm-network-select", () => expect(page.getByRole("listbox").getByRole("option").first()).toBeVisible(), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await expect(page.getByRole("listbox")).toHaveCount(0);
      await expect(page.getByRole("dialog")).toBeVisible();
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: /^Alerts/ }).click();
      await shot("alerts", () => expect(page.getByRole("dialog", { name: "Alerts" })).toBeVisible());
      await page.keyboard.press("Escape");

      await page.goto(vmUrl);
      await shot("vm-overview", () => expect(page.locator("#utilization .st-val").first()).toContainText("%", { timeout: 30_000 }));
      // the colour picker, the add-tag input with its suggestions, and the tree's list of all tags
      await page.getByRole("button", { name: `Colour of tag ${T_LIGHT}` }).click();
      await shot("vm-tag-picker", () => expect(page.getByRole("group", { name: /^Colour for tag/ })).toBeVisible(), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "+ Add Tag", exact: true }).click();
      await page.keyboard.type("e");
      await shot("vm-tag-add", () => expect(page.getByRole("combobox", { name: "Tag name" })).toBeVisible(), { viewportOnly: true });
      await page.keyboard.press("Escape");
      if (m.viewport.width >= 900) {
        await page.locator(`.trow[data-key="v:${vm.node}/${vm.uuid}"] .tgbox`).hover();
        await shot("tree-tags-all", () => expect(page.locator(".tagpop")).toBeVisible(), { viewportOnly: true });
        await page.mouse.move(700, 600);
      }
      // the Actions menu with its Power submenu open, and a confirmation dialog
      await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Power", exact: true }).click();
      await shot("actions-menu-power", () => expect(page.getByRole("menuitem", { name: "Reset (Hard Reboot)", exact: true })).toBeVisible(), { viewportOnly: true });
      await page.getByRole("menuitem", { name: "Halt (Force Shutdown)", exact: true }).click();
      await shot("confirm-dialog", () => expect(page.getByRole("dialog", { name: "Halt (Force Shutdown)" })).toBeVisible());
      await page.getByRole("button", { name: "Cancel", exact: true }).click();
      for (const [tab, check] of [
        ["hardware", "Boot order"],
        ["console", "Connected"],
        ["snapshots", "Scheduled snapshots"],
        ["backups", "Backups"],
        ["xml", "Revert"],
      ] as const) {
        await page.goto(`${vmUrl}?tab=${tab}`);
        await shot("vm-" + tab, () => expect(page.locator("main").getByText(check).first()).toBeVisible({ timeout: 30_000 }), { wide: tab === "xml" });
      }
      // console preview card, inline console, standalone console page, paste dialog
      await page.goto(vmUrl);
      await shot("vm-console-card", async () => {
        await page.locator(".cc-box").scrollIntoViewIfNeeded(); // the preview only loads while the card is on screen
        await expect(page.locator(".cc-open img")).toBeVisible({ timeout: 30_000 });
      });
      await page.request.put("/api/me/prefs", { data: { consoleClick: "inline" } });
      await page.goto(vmUrl);
      await page.locator(".cc-open").click();
      await shot("vm-console-inline", () => expect(page.locator(".cc .console.inline").getByText("Connected")).toBeVisible({ timeout: 30_000 }));
      await page.request.put("/api/me/prefs", { data: { consoleClick: "tab" } });
      await page.goto(`/console/${vm.node}/${vm.uuid}`);
      await shot("console-page", () => expect(page.getByText("Connected")).toBeVisible({ timeout: 30_000 }));
      await page.goto(`${vmUrl}?tab=console`);
      await expect(page.getByText("Connected")).toBeVisible({ timeout: 30_000 });
      await page.getByRole("button", { name: "Paste text" }).click();
      await shot("console-paste-dialog", () => expect(page.getByRole("dialog", { name: "Paste text into the guest" })).toBeVisible());
      await page.keyboard.press("Escape");
      for (const [path, name, h] of [
        ["/storage", "storage", "Storage"],
        ["/backups", "backups", "Backups"],
        ["/networks", "networks", "Networks"],
        ["/nodes", "nodes", "Nodes"],
        ["/tasks", "tasks", "Tasks"],
        ["/users", "users", "Users"],
        ["/audit", "audit", "Audit log"],
        ["/account", "account", "My account"],
        ["/cloudinit-profiles", "cloudinit-profiles", "Cloud-init profiles"],
      ] as const) {
        await page.goto(path);
        await shot(name, () => expect(heading(h)).toBeVisible());
      }
      for (const tab of ["summary", "vms", "storage", "network", "updates", "settings"]) {
        await page.goto(`/node/${vm.node}/${tab}`);
        await shot("node-" + tab, async () => {
          await expect(page.locator("main .tabs")).toBeVisible();
          await page.waitForLoadState("networkidle");
        });
      }
      // Datacenter: every tab, and a host-interfaces section opened
      for (const tab of ["summary", "vms", "storage", "network", "backups", "nodes", "tasks"]) {
        await page.goto(tab === "summary" ? "/datacenter" : `/datacenter/${tab}`);
        await shot("datacenter-" + tab, async () => {
          await expect(page.getByRole("tab", { name: "Summary", exact: true })).toBeVisible();
          await page.waitForLoadState("networkidle");
        });
      }
      await page.goto("/datacenter/network");
      await page.getByRole("button", { name: new RegExp("^" + env.node1) }).click();
      await shot("datacenter-network-interfaces", () => expect(page.locator(".nicbox tbody tr").first()).toBeVisible({ timeout: 30_000 }));
      // the DHCP reservation form: the MAC field is the themed editable combo with the VMs' MACs
      await page.goto(`/node/${vm.node}/network`);
      await row(page, "default").first().click();
      await field(page.locator(".card", { hasText: "DHCP reservations" }), "MAC address").click();
      await shot("network-dhcp-mac-suggestions", () => expect(page.getByRole("listbox").getByRole("option").first()).toBeVisible({ timeout: 30_000 }), { viewportOnly: true });
      await page.keyboard.press("Escape");
      // the Edit connection dialog of the node page's Settings tab, and its Test connection result
      await page.goto(`/node/${vm.node}/settings`);
      await page.getByRole("button", { name: "Edit connection" }).click();
      await shot("node-edit-connection-dialog", () => expect(page.getByRole("dialog", { name: "Edit connection" }).locator(".keybox code")).toContainText("ssh-ed25519"));
      await page.getByRole("dialog", { name: "Edit connection" }).getByRole("button", { name: "Test connection" }).click();
      await shot("node-edit-connection-tested", () => expect(page.getByRole("dialog", { name: "Edit connection" }).locator("ul.checklist")).toContainText("Root access", { timeout: 30_000 }));
      await page.keyboard.press("Escape");
      // modals that carry the most layout
      await page.goto("/storage");
      await page.getByRole("button", { name: "New pool" }).click();
      await shot("new-pool-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "Image catalogue" }).click();
      await shot("image-catalogue-dialog", () => expect(page.getByRole("dialog", { name: "Image catalogue" }).locator("li.catitem").first()).toBeVisible());
      await page.keyboard.press("Escape");
      await page.goto("/cloudinit-profiles");
      await page.getByRole("button", { name: "New profile" }).click();
      await shot("new-profile-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.keyboard.press("Escape");
      await page.goto(vmUrl + "?tab=hardware");
      await page.getByRole("button", { name: "Add disk" }).click();
      await shot("add-disk-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.getByRole("combobox", { name: "Storage pool" }).click();
      await shot("add-disk-pool-picker", () => expect(page.getByRole("listbox").getByRole("option").first()).toBeVisible(), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await page.keyboard.press("Escape");
      // the Hardware tab with its collapsed sections opened, and an Edit Settings dialog
      // snapshot tree, delete dialog, schedule form with the time zone helper
      await page.goto(`/vm/${snapVm.node}/${snapVm.uuid}?tab=snapshots`);
      await shot("snapshot-tree", () => expect(page.getByRole("treeitem", { name: /^scheduled-2026/ })).toBeVisible({ timeout: 30_000 }));
      await page.getByRole("button", { name: "Delete b", exact: true }).click();
      await shot("snapshot-delete-dialog", () => expect(page.getByRole("dialog", { name: "Delete b" }).getByRole("treeitem").first()).toBeVisible());
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "Set up schedule" }).click();
      await shot("snapshot-schedule-form", () => expect(page.getByRole("status", { name: "Time zone conversion" })).toBeVisible());
      // the bridge picker
      await page.goto(vmUrl + "?tab=hardware");
      await page.getByRole("button", { name: "Add interface" }).click();
      await page.getByRole("dialog").getByRole("button", { name: "Host bridge" }).click();
      await page.getByRole("combobox", { name: "Bridge device" }).click();
      await shot("bridge-picker", () => expect(page.getByRole("listbox").getByRole("option").first()).toBeVisible({ timeout: 30_000 }), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await page.keyboard.press("Escape");
      await page.goto(vmUrl + "?tab=hardware");
      await page.getByRole("button", { name: /^Advanced: CPU placement/ }).click();
      await page.getByRole("button", { name: /^Passthrough devices/ }).click();
      await shot("vm-hardware-sections-open", () => expect(page.locator("#hw-advanced-body")).toBeVisible({ timeout: 30_000 }));
      await page.evaluate(() => localStorage.removeItem("virticle.hw.open.hw-advanced"));
      await page.evaluate(() => localStorage.removeItem("virticle.hw.open.hw-passthrough"));
      await page.goto(vmUrl);
      await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Edit Settings", exact: true }).click();
      await page.getByRole("menuitem", { name: "Network interfaces", exact: true }).click();
      await shot("edit-settings-dialog", () => expect(page.getByRole("dialog", { name: "Network interfaces" }).locator("tbody tr").nth(1)).toBeVisible(), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Snapshots", exact: true }).click();
      await page.getByRole("menuitem", { name: "Take snapshot", exact: true }).click();
      await page.getByRole("button", { name: "About snapshot types" }).hover();
      await shot("snapshot-help-tip", () => expect(page.getByRole("tooltip")).toContainText("External"), { viewportOnly: true });
      await page.keyboard.press("Escape");
      await page.keyboard.press("Escape");
      await page.goto(vmUrl + "?tab=hardware");
      await page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Disks", exact: true }) }).getByRole("row", { name: /vda/ }).getByRole("button", { name: "Move", exact: true }).click();
      await shot("disk-move-dialog", () => expect(page.getByRole("dialog").locator("ul.checklist li").first()).toBeVisible({ timeout: 60_000 }));
      await page.keyboard.press("Escape");
      await page.goto(vmUrl);
      await page.locator(".head").getByRole("button", { name: "Actions", exact: true }).click();
      await page.getByRole("menuitem", { name: "Migrate", exact: true }).click();
      await page.getByRole("menuitem", { name: "Migrate to another node", exact: true }).click();
      await shot("migrate-dialog", () => expect(page.getByRole("dialog").locator("ul.checklist li").first()).toBeVisible({ timeout: 60_000 }));
      await page.keyboard.press("Escape");

      await ctx.close();
      expect(problems, problems.join("\n")).toEqual([]);
    });
  }
});
