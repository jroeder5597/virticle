// Tags: "+ Add Tag" next to the VM name, shown in the VM header, the VM list and the tree; colours chosen with the
// built-in picker (they belong to the tag name, so they follow it to every VM); suggestions from existing tags;
// the x on hover or focus with a confirmation; the tree's one-line overflow with a list of all tags; viewers.
import { Locator, Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { cancelConfirm, confirm, confirmDialog } from "../lib/ui";
import { contrast, PRESETS, textFor } from "../../web/src/tagColor";

const V1 = `${P}tg1`;
const V2 = `${P}tg2`;
const V3 = `${P}tg3`;
const TAG = `${P}shared`;
const LONG = Array.from({ length: 8 }, (_, i) => `${P}environment-${i}-${"x".repeat(12)}`);
const VIEWER = `${P}tgviewer`;
const PASS = "e2e-tag-pass-123";

const rgb = (c: string) => (c.match(/\d+/g) ?? []).slice(0, 3).map(Number) as [number, number, number];
const hex = (c: number[]) => (c.length < 3 || c.some((v) => v === undefined) ? "none" : "#" + c.map((v) => v.toString(16).padStart(2, "0")).join(""));

/** Background and text colour of a chip, as hex. */
async function chipColors(chip: Locator) {
  const [bg, fg] = await chip.evaluate((el) => [getComputedStyle(el).backgroundColor, getComputedStyle(el).color]);
  return { bg: hex(rgb(bg)), fg: hex(rgb(fg)) };
}
const tree = (page: Page) => page.getByRole("tree", { name: "Resources" });
const treeRow = (page: Page, name: string) => tree(page).getByRole("treeitem", { name: new RegExp(`^${name}, `) });

test.describe.serial("tag colours and the tag editor", () => {
  let v1: { node: number; uuid: string; name: string };
  let v2: { node: number; uuid: string; name: string };
  let v3: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => {
    await cleanVMs(api, V1, V2, V3);
    for (const t of [TAG, ...LONG]) await api.del(`/tags/${encodeURIComponent(t)}/color`).catch(() => {});
    for (const u of await api.get("/users")) if (u.username === VIEWER) await api.del(`/users/${u.id}`).catch(() => {});
  });

  test("the text colour chosen for a chip is readable (AA) for every colour", async () => {
    let worst = 99;
    for (let r = 0; r < 256; r += 15) for (let g = 0; g < 256; g += 15) for (let b = 0; b < 256; b += 15) {
      const bg = hex([r, g, b]);
      worst = Math.min(worst, contrast(bg, textFor(bg)));
    }
    for (const p of PRESETS) worst = Math.min(worst, contrast(p, textFor(p)));
    expect(worst).toBeGreaterThanOrEqual(4.5);
    expect(textFor("#ffffff")).not.toBe("#ffffff");
    expect(textFor("#000000")).toBe("#ffffff");
  });

  test("+ Add Tag puts a new tag next to the name; it shows in the header, the list and the tree", async ({ page, api, n1 }) => {
    v1 = await createCirros(api, n1, V1);
    v2 = await createCirros(api, n1, V2);
    await openVM(page, v1);
    const add = page.getByRole("button", { name: "+ Add Tag", exact: true });
    await expect(add).toBeVisible();
    await add.click();
    const input = page.getByRole("combobox", { name: "Tag name" });
    await expect(input).toBeFocused();
    await input.fill("-bad");
    await input.press("Enter");
    await expect(page.locator(".tagedit .error")).toContainText("start with a letter or digit");
    await input.fill(TAG);
    await input.press("Enter");
    const chip = page.locator(".titleline .tag", { hasText: TAG });
    await expect(chip).toBeVisible();
    await expect(page.getByRole("button", { name: "+ Add Tag", exact: true })).toBeVisible(); // back to the button
    expect((await api.vmDetail(v1.node, v1.uuid)).tags).toEqual([TAG]);

    // same line as the name, after the state badge, with the button after the chip
    const h1 = (await page.getByRole("heading", { level: 1 }).boundingBox())!;
    const c = (await chip.boundingBox())!;
    const b = (await page.getByRole("button", { name: "+ Add Tag", exact: true }).boundingBox())!;
    expect(c.x).toBeGreaterThan(h1.x + h1.width - 1);
    expect(Math.abs(c.y + c.height / 2 - (h1.y + h1.height / 2))).toBeLessThan(h1.height / 2);
    expect(b.x).toBeGreaterThan(c.x + c.width - 1);

    await page.goto("/");
    await expect(page.locator("tbody tr", { hasText: V1 }).locator(".tag", { hasText: TAG })).toBeVisible();
    await expect(treeRow(page, V1).locator(".tgstrip .tag", { hasText: TAG })).toBeVisible();
  });

  test("the colour picker: hex field, preset and wheel each make one request; it persists and shows everywhere", async ({ page, api }) => {
    const puts: string[] = [];
    page.on("request", (r) => r.method() === "PUT" && r.url().includes("/color") && puts.push(r.url()));
    await openVM(page, v1);
    const chip = page.locator(".titleline .tag", { hasText: TAG });
    const open = async () => {
      await page.getByRole("button", { name: `Colour of tag ${TAG}` }).click();
      const pick = page.getByRole("group", { name: `Colour for tag ${TAG}` });
      await expect(pick).toBeVisible();
      return pick;
    };
    // inside the window, at the phone width as well
    let pick = await open();
    const box = (await pick.boundingBox())!;
    const vp = page.viewportSize()!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(vp.width);
    expect(box.y + box.height).toBeLessThanOrEqual(vp.height);
    await expect(pick.getByRole("button", { name: /^Colour #/ })).toHaveCount(10);

    // hex field + Enter
    await pick.getByLabel("Hex colour").fill("#102030");
    await pick.getByLabel("Hex colour").press("Enter");
    await expect(pick).toBeHidden();
    await expect.poll(async () => (await chipColors(chip)).bg).toBe("#102030");
    expect((await chipColors(chip)).fg).toBe("#ffffff"); // light text on a dark colour
    expect(puts).toHaveLength(1);
    expect((await api.get("/tags")).find((t: any) => t.name === TAG).color).toBe("#102030");

    // a bad hex is refused in place, with no request
    pick = await open();
    await pick.getByLabel("Hex colour").fill("#12");
    await pick.getByLabel("Hex colour").press("Enter");
    await expect(pick.getByRole("alert")).toContainText("six hex digits");
    expect(puts).toHaveLength(1);

    // preset
    await pick.getByRole("button", { name: "Colour #ffc53d" }).click();
    await expect(pick).toBeHidden();
    await expect.poll(async () => (await chipColors(chip)).bg).toBe("#ffc53d");
    expect(textFor("#ffc53d")).toBe((await chipColors(chip)).fg);
    expect(puts).toHaveLength(2);

    // the wheel: a drag with many moves, one request on release
    pick = await open();
    const ring = pick.getByRole("slider", { name: "Hue" });
    const r = (await ring.boundingBox())!;
    const cx = r.x + r.width / 2,
      cy = r.y + r.height / 2,
      rad = r.width / 2 - 10;
    await page.mouse.move(cx, cy - rad);
    await page.mouse.down();
    for (let a = 0; a <= 90; a += 10) await page.mouse.move(cx + rad * Math.sin((a * Math.PI) / 180), cy - rad * Math.cos((a * Math.PI) / 180));
    expect(puts).toHaveLength(2); // nothing sent while dragging
    await page.mouse.up();
    await expect.poll(() => puts.length).toBe(3);
    await expect(ring).toHaveAttribute("aria-valuenow", /^(8\d|9\d)$/); // about 90 degrees: green-cyan
    // keyboard: arrows only change the preview, Enter applies
    await ring.focus();
    await page.keyboard.press("ArrowRight");
    await page.keyboard.press("ArrowRight");
    expect(puts).toHaveLength(3);
    await page.keyboard.press("Enter");
    await expect(pick).toBeHidden();
    expect(puts).toHaveLength(4);

    // one more preset so the checks below have a known value
    pick = await open();
    await pick.getByRole("button", { name: "Colour #6e56cf" }).click();
    await expect(pick).toBeHidden();
    await expect.poll(async () => (await chipColors(chip)).bg).toBe("#6e56cf");

    // reload: persists, and shows in the header, the list and the tree
    await page.reload();
    await expect.poll(async () => (await chipColors(page.locator(".titleline .tag", { hasText: TAG }))).bg).toBe("#6e56cf");
    await page.goto("/");
    await expect.poll(async () => (await chipColors(page.locator("tbody tr", { hasText: V1 }).locator(".tag", { hasText: TAG }))).bg).toBe("#6e56cf");
    await expect.poll(async () => (await chipColors(treeRow(page, V1).locator(".tgstrip .tag", { hasText: TAG }))).bg).toBe("#6e56cf");
    expect(textFor("#6e56cf")).toBe((await chipColors(treeRow(page, V1).locator(".tgstrip .tag", { hasText: TAG }))).fg);

    // Remove colour: back to the neutral chip, everywhere
    await openVM(page, v1);
    pick = await open();
    await pick.getByRole("button", { name: "Remove colour" }).click();
    await expect(pick).toBeHidden();
    await expect(chip).not.toHaveClass(/colored/);
    expect((await api.get("/tags")).find((t: any) => t.name === TAG).color).toBeUndefined();
    // and back to a colour for the next tests
    await api.put(`/tags/${encodeURIComponent(TAG)}/color`, { color: "#6e56cf" });
    // the server refuses anything but #rrggbb
    for (const bad of ["red", "#fff", "#12345g", "url(x)"]) {
      const res = await api.ctx.fetch(`/api/tags/${encodeURIComponent(TAG)}/color`, { method: "PUT", data: { color: bad } });
      expect(res.status(), bad).toBe(400);
    }
  });

  test("a second VM takes the existing tag, with its colour, from the suggestion list", async ({ page, api }) => {
    await openVM(page, v2);
    await page.getByRole("button", { name: "+ Add Tag", exact: true }).click();
    const input = page.getByRole("combobox", { name: "Tag name" });
    await input.fill(TAG.slice(0, 6).toUpperCase()); // case-insensitive filter
    const list = page.getByRole("listbox", { name: "Existing tags" });
    const opt = list.getByRole("option").filter({ hasText: TAG });
    await expect(opt).toBeVisible();
    expect((await chipColors(opt.locator(".tag"))).bg).toBe("#6e56cf"); // suggestions show the colour
    await input.press("ArrowDown");
    await expect(opt).toHaveAttribute("aria-selected", "true");
    await input.press("Enter");
    const chip = page.locator(".titleline .tag", { hasText: TAG });
    await expect(chip).toBeVisible();
    await expect.poll(async () => (await chipColors(chip)).bg).toBe("#6e56cf");
    expect((await api.vmDetail(v2.node, v2.uuid)).tags).toEqual([TAG]);
    // Escape returns to the button
    await page.getByRole("button", { name: "+ Add Tag", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "+ Add Tag", exact: true })).toBeVisible();
    // blur with empty text too
    await page.getByRole("button", { name: "+ Add Tag", exact: true }).click();
    await page.getByRole("heading", { level: 1 }).click();
    await expect(page.getByRole("button", { name: "+ Add Tag", exact: true })).toBeVisible();
    // a click on a suggestion assigns it as well (a third tag, created through the API first)
    await api.put(`/nodes/${v1.node}/vms/${v1.uuid}/tags`, { tags: [TAG, `${P}other`] });
    await page.reload();
    await page.getByRole("button", { name: "+ Add Tag", exact: true }).click();
    await page.getByRole("combobox", { name: "Tag name" }).fill(`${P}oth`);
    await page.getByRole("option").filter({ hasText: `${P}other` }).click();
    await expect(page.locator(".titleline .tag", { hasText: `${P}other` })).toBeVisible();
    expect((await api.vmDetail(v2.node, v2.uuid)).tags).toEqual([TAG, `${P}other`]);
    await api.put(`/nodes/${v1.node}/vms/${v1.uuid}/tags`, { tags: [TAG] });
    await api.put(`/nodes/${v2.node}/vms/${v2.uuid}/tags`, { tags: [TAG] });
  });

  test("the x shows on hover and on focus only, keeps the chip's width, and removal asks first", async ({ page, api }) => {
    await openVM(page, v2);
    const chip = page.locator(".titleline .tag.editable", { hasText: TAG });
    const x = chip.locator(".tagx"); // not by role: while hidden it is out of the accessibility tree
    await expect(chip.locator("button.tagx")).toHaveAttribute("aria-label", `Remove tag ${TAG}`);
    await expect(x).toHaveCSS("visibility", "hidden");
    const w0 = (await chip.boundingBox())!.width;
    await chip.hover();
    await expect(x).toBeVisible();
    expect((await chip.boundingBox())!.width).toBe(w0);
    await page.getByRole("heading", { level: 1 }).hover();
    await expect(x).toHaveCSS("visibility", "hidden");
    await chip.getByRole("button", { name: `Colour of tag ${TAG}` }).focus(); // keyboard focus
    await expect(x).toBeVisible();
    await page.getByRole("heading", { level: 1 }).click();
    await expect(x).toHaveCSS("visibility", "hidden");
    // the VM list and the tree show no x at all
    // cancel keeps the tag
    await chip.hover();
    await x.click();
    await expect(confirmDialog(page)).toContainText(`Remove tag ${TAG} from VM ${V2}?`);
    await cancelConfirm(page);
    expect((await api.vmDetail(v2.node, v2.uuid)).tags).toEqual([TAG]);
    await expect(chip).toBeVisible();
    // confirm removes it
    await chip.hover();
    await x.click();
    await confirm(page, "Remove tag");
    await expect(chip).toHaveCount(0);
    expect((await api.vmDetail(v2.node, v2.uuid)).tags ?? []).toEqual([]);
    // the colour belongs to the name: the other VM still has the tag, still coloured
    expect((await api.get("/tags")).find((t: any) => t.name === TAG)).toMatchObject({ color: "#6e56cf", count: 1 });
  });

  test("tree: eight long tags stay on one line, do not widen the sidebar, and hovering or focusing lists them all", async ({ page, api, n1 }) => {
    v3 = await createCirros(api, n1, V3);
    await api.put(`/nodes/${v3.node}/vms/${v3.uuid}/tags`, { tags: LONG });
    await page.goto("/");
    const row = treeRow(page, V3);
    await expect(row).toBeVisible();
    const strip = row.locator(".tgstrip");
    await expect(strip.locator(".tag")).toHaveCount(8);
    // measurements
    const sidebar = page.locator(".sidebar");
    const m = await page.evaluate(() => {
      const sb = document.querySelector(".sidebar")!.getBoundingClientRect();
      const t = document.querySelector(".tree")!;
      const rows = Array.from(document.querySelectorAll<HTMLElement>(".trow")).map((r) => r.getBoundingClientRect().height);
      return { sbw: sb.width, scroll: t.scrollWidth - t.clientWidth, heights: [...new Set(rows)] };
    });
    expect(m.scroll).toBeLessThanOrEqual(0);
    expect(m.heights).toEqual([26]); // every row, tagged or not, has the same height
    const sbw0 = (await sidebar.boundingBox())!.width;
    expect(Math.abs(sbw0 - m.sbw)).toBeLessThan(1);
    expect(sbw0).toBeLessThanOrEqual(262);
    // never wraps, never ends past the row
    const fitOk = await row.evaluate((r) => {
      const rr = r.getBoundingClientRect();
      const chips = Array.from(r.querySelectorAll<HTMLElement>(".tgstrip .tag")).filter((c) => getComputedStyle(c).display !== "none");
      const tops = new Set(chips.map((c) => Math.round(c.getBoundingClientRect().top)));
      const box = r.querySelector(".tgbox")!.getBoundingClientRect();
      return { n: chips.length, lines: tops.size, inRow: r.querySelector(".tgbox")!.getBoundingClientRect().right <= rr.right + 1, boxW: box.width };
    });
    expect(fitOk.lines).toBe(1);
    expect(fitOk.inRow).toBe(true);
    expect(fitOk.n).toBeLessThan(8);
    // the overflow indicator
    await expect(row.locator(".tgbox[data-over]")).toHaveCount(1);
    await expect(row.locator(".tmore")).toBeVisible();
    await expect(row.locator(".tmore")).toHaveText("…");
    // hover lists all eight
    await expect(page.locator(".tagpop")).toHaveCount(0);
    await row.locator(".tgbox").hover();
    const pop = page.locator(".tagpop");
    await expect(pop).toBeVisible();
    await expect(pop.locator(".tag")).toHaveCount(8);
    for (const t of LONG) await expect(pop.locator(".tag", { hasText: t })).toBeVisible();
    const pb = (await pop.boundingBox())!;
    const vp = page.viewportSize()!;
    expect(pb.x).toBeGreaterThanOrEqual(0);
    expect(pb.x + pb.width).toBeLessThanOrEqual(vp.width);
    expect(pb.y + pb.height).toBeLessThanOrEqual(vp.height);
    await page.mouse.move(700, 500);
    await expect(pop).toHaveCount(0);
    // keyboard focus on the row lists them too, Escape closes
    await row.focus();
    await expect(pop).toBeVisible();
    await expect(pop.locator(".tag")).toHaveCount(8);
    await page.keyboard.press("Escape");
    await expect(pop).toHaveCount(0);
    // the VM page still shows all of them (wrapping onto further lines at this width)
    await openVM(page, v3);
    await expect(page.locator(".titleline .tag.editable")).toHaveCount(8);
    // a colour reaches the tree's chip too, without reloading
    const first = row;
    await page.goto("/");
    await api.put(`/tags/${encodeURIComponent(LONG[0])}/color`, { color: "#ffffff" });
    await expect.poll(async () => (await chipColors(first.locator(".tgstrip .tag").first())).bg, { timeout: 15_000 }).toBe("#ffffff");
    expect((await chipColors(first.locator(".tgstrip .tag").first())).fg).not.toBe("#ffffff");
    // a narrower sidebar content (phone drawer) is covered by the visual pass (14)
  });

  test("viewer: sees the chips, has no button, no x, no picker", async ({ browser, api }) => {
    await api.post("/users", { username: VIEWER, password: PASS, role: "viewer" });
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] }, baseURL: env.base, viewport: { width: 1360, height: 860 } });
    const page = await ctx.newPage();
    await page.goto("/");
    await page.getByLabel("Username").fill(VIEWER);
    await page.getByLabel("Password").fill(PASS);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Virtual machines" })).toBeVisible();
    await openVM(page, v1);
    const chip = page.locator(".titleline .tag", { hasText: TAG });
    await expect(chip).toBeVisible();
    await expect.poll(async () => (await chipColors(chip)).bg).toBe("#6e56cf");
    await expect(page.getByRole("button", { name: "+ Add Tag" })).toHaveCount(0);
    await expect(page.locator(".tagx, .tagname")).toHaveCount(0);
    await chip.click();
    await expect(page.getByRole("group", { name: /^Colour for tag/ })).toHaveCount(0);
    // the server agrees
    const res = await ctx.request.put(`/api/tags/${encodeURIComponent(TAG)}/color`, { data: { color: "#000000" } });
    expect(res.status()).toBe(403);
    expect((await ctx.request.get("/api/tags")).status()).toBe(200);
    await ctx.close();
  });
});
