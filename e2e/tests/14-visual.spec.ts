// One visual pass: both themes (dark is the default; light is chosen through the app's own theme
// setting, not the OS preference) at desktop and phone width over every page and tab.
// Screenshots go to test-results/visual/ for a human to look at; the automatic checks catch
// unreadable text (contrast) and pages that scroll sideways.
import { Page } from "@playwright/test";
import { mkdirSync } from "fs";
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { P } from "../lib/env";

const NAME = `${P}vis1`;
const OUT = "test-results/visual";

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
  return page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
}

/** Desktop only: elements whose content is wider than their box (cut off or scrolling inside), or that end past the window. */
async function clipped(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    const vw = window.innerWidth;
    for (const el of Array.from(document.querySelectorAll<HTMLElement>("body *"))) {
      if (el.closest("pre, textarea, code, canvas, svg, .xterm, .console, select, input")) continue;
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

const modes = [
  { name: "dark", scheme: "dark" as const, viewport: { width: 1280, height: 800 } },
  { name: "light", scheme: "light" as const, viewport: { width: 1440, height: 900 } },
  { name: "narrow", scheme: "light" as const, viewport: { width: 420, height: 860 } },
  { name: "narrow-dark", scheme: "dark" as const, viewport: { width: 420, height: 860 } },
];

test.describe.serial("visual pass", () => {
  let vm: { node: number; uuid: string; name: string };
  test.beforeAll(async ({ api, n1 }) => {
    mkdirSync(OUT, { recursive: true });
    vm = await createCirros(api, n1, NAME, { start: true });
  });
  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  for (const m of modes) {
    test(`every page and tab: ${m.name}`, async ({ browser }) => {
      test.setTimeout(240_000);
      const ctx = await browser.newContext({ storageState: ".auth/admin.json", viewport: m.viewport });
      // The theme no longer follows the OS: pick it the way the toggle does (localStorage, read before first paint).
      await ctx.addInitScript((t) => localStorage.setItem("vp-theme", t), m.scheme);
      const page = await ctx.newPage();
      const problems: string[] = [];
      const shot = async (name: string, ready: () => Promise<unknown>, opts: { contrast?: boolean; wide?: boolean } = {}) => {
        await ready();
        await page.waitForTimeout(400);
        await page.screenshot({ path: `${OUT}/${m.name}-${name}.png`, fullPage: true });
        const issues = await lowContrast(page);
        for (const i of issues) problems.push(`${m.name}/${name}: low contrast: ${i}`);
        const over = await sideways(page);
        if (over > 1 && !opts.wide) problems.push(`${m.name}/${name}: page scrolls sideways by ${over}px`);
        if (m.viewport.width >= 1000 && !opts.wide) for (const c of await clipped(page)) problems.push(`${m.name}/${name}: ${c}`);
      };
      const vmUrl = `/vm/${vm.node}/${vm.uuid}`;
      const heading = (n: string | RegExp) => page.getByRole("heading", { name: n }).first();

      await page.goto("/");
      expect(await page.evaluate(() => document.documentElement.getAttribute("data-theme"))).toBe(m.scheme);
      await shot("list", () => expect(page.locator("tbody tr", { hasText: NAME })).toBeVisible());
      await page.getByRole("button", { name: "New VM" }).click();
      await shot("new-vm-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: /^Alerts/ }).click();
      await shot("alerts", () => expect(page.getByRole("dialog", { name: "Alerts" })).toBeVisible());
      await page.keyboard.press("Escape");

      await page.goto(vmUrl);
      await shot("vm-overview", () => expect(page.locator(".charts")).toBeVisible({ timeout: 30_000 }));
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
      await page.evaluate(() => localStorage.setItem("vp-console-click", "inline"));
      await page.goto(vmUrl);
      await page.locator(".cc-open").click();
      await shot("vm-console-inline", () => expect(page.locator(".cc .console.inline").getByText("Connected")).toBeVisible({ timeout: 30_000 }));
      await page.evaluate(() => localStorage.removeItem("vp-console-click"));
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
      // modals that carry the most layout
      await page.goto("/storage");
      await page.getByRole("button", { name: "New pool" }).click();
      await shot("new-pool-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.keyboard.press("Escape");
      await page.goto(vmUrl + "?tab=hardware");
      await page.getByRole("button", { name: "Add disk" }).click();
      await shot("add-disk-dialog", () => expect(page.getByRole("dialog")).toBeVisible());
      await page.keyboard.press("Escape");
      await page.goto(vmUrl);
      await page.locator(".head").getByRole("button", { name: "Migrate" }).click();
      await shot("migrate-dialog", () => expect(page.getByRole("dialog").locator("ul.checks li").first()).toBeVisible({ timeout: 60_000 }));
      await page.keyboard.press("Escape");

      await ctx.close();
      expect(problems, problems.join("\n")).toEqual([]);
    });
  }
});
