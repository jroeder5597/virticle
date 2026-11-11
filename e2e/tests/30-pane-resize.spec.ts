import { expect, test } from "../lib/fixtures";

// The resource tree and the Tasks panel are sized by dragging the line between them and the page.
const width = (page: any) => page.locator(".sidebar").evaluate((e: HTMLElement) => Math.round(e.getBoundingClientRect().width));
const height = (page: any) => page.locator(".dock").evaluate((e: HTMLElement) => Math.round(e.getBoundingClientRect().height));

async function drag(page: any, name: string, dx: number, dy: number) {
  const h = page.getByRole("separator", { name });
  const b = (await h.boundingBox())!;
  const x = b.x + b.width / 2;
  const y = b.y + b.height / 2;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + dx / 2, y + dy / 2);
  await page.mouse.move(x + dx, y + dy);
  await page.mouse.up();
}

test.describe("pane resizing", () => {
  test.afterEach(async ({ api }) => {
    await api.put("/me/prefs", { sidebarW: "", dockH: "", tasksDock: "open" });
  });

  test("the tree is dragged wider, the size survives a reload, double-click resets it", async ({ page, api }) => {
    await api.put("/me/prefs", { sidebarW: "", dockH: "", tasksDock: "open" });
    await page.setViewportSize({ width: 1360, height: 860 });
    await page.goto("/datacenter/vms");
    await expect(page.locator(".sidebar")).toBeVisible();
    const before = await width(page);
    await drag(page, "Resize the resource tree", 120, 0);
    expect(await width(page)).toBe(before + 120);
    await expect.poll(async () => (await api.get("/me/prefs")).sidebarW).toBe(String(before + 120));
    await page.reload();
    await expect.poll(() => width(page)).toBe(before + 120);
    // limits: not narrower than 180, not wider than half the window
    await drag(page, "Resize the resource tree", -900, 0);
    expect(await width(page)).toBe(180);
    await drag(page, "Resize the resource tree", 1200, 0);
    expect(await width(page)).toBe(640);
    // nothing runs off the screen at the widest tree
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await page.getByRole("separator", { name: "Resize the resource tree" }).dblclick();
    await expect.poll(() => width(page)).toBe(before);
    await expect.poll(async () => (await api.get("/me/prefs")).sidebarW).toBe("");
  });

  test("the Tasks panel is dragged taller, survives a reload, keyboard moves it, collapse still works", async ({ page, api }) => {
    await api.put("/me/prefs", { sidebarW: "", dockH: "", tasksDock: "open" });
    await page.setViewportSize({ width: 1360, height: 860 });
    await page.goto("/datacenter/vms");
    await expect(page.locator(".dock.open")).toBeVisible();
    const before = await height(page);
    await drag(page, "Resize the tasks panel", 0, -150);
    expect(await height(page)).toBe(before + 150);
    await expect.poll(async () => (await api.get("/me/prefs")).dockH).toBe(String(before + 150));
    await page.reload();
    await expect.poll(() => height(page)).toBe(before + 150);
    const h = page.getByRole("separator", { name: "Resize the tasks panel" });
    await h.focus();
    await page.keyboard.press("ArrowDown");
    await expect.poll(() => height(page)).toBe(before + 150 - 16);
    // the limit: 70% of the window
    await drag(page, "Resize the tasks panel", 0, -900);
    expect(await height(page)).toBe(Math.round(860 * 0.7));
    // collapsed: the bar only, and no handle; opened again: the chosen height
    await page.locator(".dock-toggle").click();
    await expect(h).toHaveCount(0);
    expect(await height(page)).toBeLessThan(40);
    await page.locator(".dock-toggle").click();
    await expect.poll(() => height(page)).toBe(Math.round(860 * 0.7));
    await h.focus();
    await page.keyboard.press("Home");
    await expect.poll(() => height(page)).toBe(before);
  });

  test("phone width: no tree handle, the page still fits", async ({ page, api }) => {
    await api.put("/me/prefs", { sidebarW: "500", dockH: "", tasksDock: "closed" });
    await page.setViewportSize({ width: 390, height: 800 });
    await page.goto("/datacenter/vms");
    await expect(page.getByRole("separator", { name: "Resize the resource tree" })).toBeHidden();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  });
});
