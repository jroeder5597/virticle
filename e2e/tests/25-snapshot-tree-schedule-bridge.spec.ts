// Snapshot tree and the delete dialog, schedule local times and Next Run, the bridge picker.
import { Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, field } from "../lib/ui";
import { buildSnapRows, branchOf } from "../../web/src/snapTree";
import { describeLocal, describeUtc, localToUtc, utcToLocal } from "../../web/src/tz";

const NAME = `${P}st1`;
const NAME2 = `${P}st2`;
const at = (iso: string) => Date.parse(iso);

test.describe("snapshot tree rows (pure)", () => {
  const s = (name: string, parent: string | undefined, created: number, isCurrent = false) => ({ name, parent, created, isCurrent });
  test("a root with several branches", () => {
    const rows = buildSnapRows([s("a", undefined, 1), s("b", "a", 2), s("c", "b", 3), s("d", "a", 4, true)], true);
    expect(rows.map((r) => `${r.kind === "now" ? "*" : r.name}:${r.depth}:${r.isLast ? "L" : "-"}`)).toEqual(["a:0:L", "b:1:-", "c:2:L", "d:1:L", "*:2:L"]);
    // c hangs below b, which has a later sibling (d): the trunk line at b's column continues past c
    expect(rows[2].trunk).toEqual([true]);
    expect(rows[1].descendants).toBe(1);
    expect(rows[0].descendants).toBe(3);
    expect(branchOf(rows, "a")).toEqual(["a", "b", "c", "d"]);
    expect(branchOf(rows, "b")).toEqual(["b", "c"]);
    // the current snapshot's last child is the running state, so d is not the last of its parent's children list
    expect(rows[4].parent).toBe(3);
  });
  test("a deep chain", () => {
    const chain = Array.from({ length: 6 }, (_, i) => s(`n${i}`, i ? `n${i - 1}` : undefined, i + 1, i === 5));
    const rows = buildSnapRows(chain, false);
    expect(rows.map((r) => r.depth)).toEqual([0, 1, 2, 3, 4, 5]);
    expect(rows.every((r) => r.isLast)).toBe(true);
    expect(rows[5].trunk).toEqual([false, false, false, false]);
  });
  test("an orphan whose parent is missing is a root; several roots; cycles do not loop", () => {
    const rows = buildSnapRows([s("o", "gone", 5), s("p", "o", 6), s("r", undefined, 1)], false);
    expect(rows.map((r) => `${r.name}:${r.depth}`)).toEqual(["r:0", "o:0", "p:1"]);
    const cyc = buildSnapRows([s("x", "y", 1), s("y", "x", 2)], false);
    expect(cyc.length).toBe(0); // neither is a root; nothing to show, and no endless loop
    expect(buildSnapRows([], true)).toEqual([]);
  });
});

test.describe("time zone conversion (pure)", () => {
  test("America/New_York: UTC evening or early morning, DST changes the offset", () => {
    expect(describeUtc("03:00", at("2026-03-01T12:00:00Z"), "America/New_York")).toBe("03:00 UTC is 22:00 EST (the previous day) in your time zone (America/New_York)");
    // after the change on 8 March the same UTC time is an hour later locally
    expect(describeUtc("03:00", at("2026-03-09T12:00:00Z"), "America/New_York")).toBe("03:00 UTC is 23:00 EDT (the previous day) in your time zone (America/New_York)");
    expect(describeUtc("15:30", at("2026-03-01T12:00:00Z"), "America/New_York")).toBe("15:30 UTC is 10:30 EST in your time zone (America/New_York)");
    expect(describeLocal("22:00", at("2026-03-01T12:00:00Z"), "America/New_York")).toBe("22:00 in your time zone (America/New_York) is 03:00 UTC (the next day)");
  });
  test("Asia/Kolkata: +5:30, no daylight saving", () => {
    expect(utcToLocal("03:00", at("2026-03-01T12:00:00Z"), "Asia/Kolkata")).toMatchObject({ time: "08:30", dayShift: 0 });
    expect(utcToLocal("20:00", at("2026-03-01T12:00:00Z"), "Asia/Kolkata")).toMatchObject({ time: "01:30", dayShift: 1 });
    expect(localToUtc("08:30", at("2026-03-01T12:00:00Z"), "Asia/Kolkata")).toMatchObject({ time: "03:00" });
    expect(localToUtc("00:15", at("2026-03-01T12:00:00Z"), "Asia/Kolkata")).toMatchObject({ time: "18:45", dayShift: -1 });
  });
  test("Pacific/Auckland across the end of daylight saving (5 April 2026, 03:00 local)", () => {
    // before the change: NZDT, +13
    expect(utcToLocal("03:00", at("2026-03-01T12:00:00Z"), "Pacific/Auckland")).toMatchObject({ time: "16:00", dayShift: 0 });
    // the next 03:00 UTC after the evening of 4 April is the one on 5 April, already NZST (+12)
    expect(utcToLocal("03:00", at("2026-04-04T12:00:00Z"), "Pacific/Auckland")).toMatchObject({ time: "15:00" });
    expect(utcToLocal("03:00", at("2026-04-10T12:00:00Z"), "Pacific/Auckland")).toMatchObject({ time: "15:00" });
    // reverse, just before the change: the next 08:00 local is on 5 April, in NZST
    expect(localToUtc("08:00", at("2026-04-04T00:00:00Z"), "Pacific/Auckland")).toMatchObject({ time: "20:00", dayShift: -1 });
    expect(localToUtc("08:00", at("2026-03-01T00:00:00Z"), "Pacific/Auckland")).toMatchObject({ time: "19:00" });
  });
  test("bad input gives nothing", () => {
    expect(utcToLocal("25:00", Date.now(), "UTC")).toBeUndefined();
    expect(localToUtc("", Date.now(), "UTC")).toBeUndefined();
    expect(describeUtc("3pm", Date.now(), "UTC")).toBeUndefined();
  });
});

test.describe.serial("snapshot tree, delete dialog, schedule, bridge picker", () => {
  let vm: { node: number; uuid: string; name: string };
  let vm2: { node: number; uuid: string; name: string };
  const base = () => `/nodes/${vm.node}/vms/${vm.uuid}`;
  const snaps = (api: any) => api.get(`${base()}/snapshots`) as Promise<any[]>;
  const node = (page: Page, name: string) => page.getByRole("treeitem", { name: new RegExp(`^${name}(,|$)`) });
  const longName = "scheduled-2026-11-11T03-00-00Z-daily";

  test.beforeAll(async ({ api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    // a -> b -> c, revert to a, take d: a has two branches and d is current
    for (const n of ["a", "b", "c"]) await api.post(`${base()}/snapshots`, { name: n, description: `snapshot ${n}` });
    await api.post(`${base()}/snapshots/a/revert`, {});
    await api.post(`${base()}/snapshots`, { name: "d", description: "taken after the revert" });
    await api.post(`${base()}/snapshots`, { name: longName, description: "A long description that goes on and on, to see how the dialog and the tree treat user text of this kind in a narrow place" });
  });
  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME, NAME2);
    await api.del(`/nodes/${n1}/pools/default/volumes/${NAME2}.x1.qcow2`).catch(() => {});
  });

  test("the tree shows both branches, the current marker, the running state, and keeps fixed rows", async ({ page }) => {
    await openVM(page, vm, "snapshots");
    const level = async (n: string) => Number(await node(page, n).getAttribute("aria-level"));
    expect([await level("a"), await level("b"), await level("c"), await level("d")]).toEqual([1, 2, 3, 2]);
    expect(await level(longName)).toBe(3); // taken after d, so it hangs below d
    await expect(node(page, longName).locator(".badge", { hasText: "current" })).toBeVisible();
    await expect(page.getByRole("treeitem", { name: /You are here/ })).toBeVisible();
    await expect(page.locator(".srow .badge", { hasText: "current" })).toHaveCount(1);
    // one fixed height for every row
    const heights = await page.locator(".srow").evaluateAll((els) => [...new Set(els.map((e) => Math.round(e.getBoundingClientRect().height)))]);
    expect(heights).toHaveLength(1);
    // the tree fits its card and the page does not scroll sideways
    const tree = await page.locator(".stree-scroll").boundingBox();
    const card = await page.locator("main").boundingBox();
    expect(tree!.x + tree!.width).toBeLessThanOrEqual(card!.x + card!.width + 1);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)).toBeLessThanOrEqual(0);
    // connector lines exist for the branches
    expect(await page.locator(".gc.e").count()).toBeGreaterThanOrEqual(4);
    expect(await page.locator(".gc.t").count()).toBeGreaterThanOrEqual(1);
  });

  test("keyboard: arrows move between rows, Left goes to the parent, Right to the first child", async ({ page }) => {
    await openVM(page, vm, "snapshots");
    await node(page, "a").focus();
    await page.keyboard.press("ArrowDown");
    await expect(node(page, "b")).toBeFocused();
    await page.keyboard.press("ArrowRight");
    await expect(node(page, "c")).toBeFocused();
    await page.keyboard.press("ArrowLeft");
    await expect(node(page, "b")).toBeFocused();
    await page.keyboard.press("ArrowLeft");
    await expect(node(page, "a")).toBeFocused();
    await page.keyboard.press("End");
    await expect(page.getByRole("treeitem", { name: /You are here/ })).toBeFocused();
    await page.keyboard.press("Home");
    await expect(node(page, "a")).toBeFocused();
  });

  test("Delete from b: b is ticked, with its branch c is ticked too, one task deletes both", async ({ page, api }) => {
    const before = (await api.get("/tasks")).filter((t: any) => t.kind === "snapshot delete").length;
    await openVM(page, vm, "snapshots");
    await node(page, "b").getByRole("button", { name: "Delete b" }).click();
    const dlg = dialog(page, "Delete b");
    await expect(dlg.getByRole("checkbox", { name: "Select b", exact: true })).toBeChecked();
    await expect(dlg.getByRole("checkbox", { name: "Select c" })).not.toBeChecked();
    await expect(dlg).toContainText("1 of 5 selected");
    // what happens to what is not selected is stated
    await expect(dlg).toContainText("Snapshots you do not tick are kept");
    await expect(dlg).toContainText("attached to its parent");
    await dlg.getByRole("button", { name: "Select b and everything below it" }).click();
    await expect(dlg.getByRole("checkbox", { name: "Select c" })).toBeChecked();
    await expect(dlg).toContainText("2 of 5 selected");
    await expect(dlg.getByRole("button", { name: "Delete 2 snapshots" })).toBeEnabled();
    // select none disables the confirm button
    await dlg.getByRole("button", { name: "Select none" }).click();
    await expect(dlg.getByRole("button", { name: "Delete 0 snapshots" })).toBeDisabled();
    await dlg.getByRole("button", { name: "Select b and everything below it" }).click();
    await dlg.getByRole("button", { name: "Delete 2 snapshots" }).click();
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    // the dialog closes at once; the delete runs as one background task
    await expect.poll(async () => (await snaps(api)).map((s) => s.name).sort(), { timeout: 60_000 }).toEqual(["a", "d", longName].sort());
    const tasks = (await api.get("/tasks")).filter((t: any) => t.kind === "snapshot delete");
    expect(tasks.length - before).toBe(1);
    await expect.poll(async () => (await api.get("/tasks")).filter((t: any) => t.kind === "snapshot delete")[0].status).toBe("ok");
    await expect(node(page, "b")).toHaveCount(0);
    // d kept its parent
    await expect(node(page, "d")).toHaveAttribute("aria-level", "2");
  });

  test("the dialog keeps sentences whole and aligns check boxes with their labels, with long names", async ({ page }) => {
    await openVM(page, vm, "snapshots");
    await page.getByRole("button", { name: "Delete snapshots" }).click();
    const dlg = dialog(page, "Delete snapshots");
    const box = await dlg.boundingBox();
    expect(box!.width).toBeGreaterThan(700); // wide for the tree
    // no paragraph breaks words anywhere
    for (const p of await dlg.locator("p").all()) expect(await p.evaluate((e) => getComputedStyle(e).overflowWrap)).toBe("normal");
    // the long name is on the row and the check box is vertically centred on it
    const row = dlg.locator(".srow", { hasText: longName });
    const rb = await row.boundingBox();
    const cb = await row.getByRole("checkbox").boundingBox();
    expect(Math.abs(cb!.y + cb!.height / 2 - (rb!.y + rb!.height / 2))).toBeLessThan(6);
    expect(cb!.x + cb!.width).toBeLessThanOrEqual(rb!.x + rb!.width);
    await page.keyboard.press("Escape");
    // the Revert dialog: the force option's text starts on the check box's line
    await node(page, "a").getByRole("button", { name: "Revert to a" }).click();
    const rv = dialog(page, "Revert to a");
    const label = rv.locator("label.check");
    const input = await label.locator("input").boundingBox();
    const lb = await label.boundingBox();
    expect(input!.y - lb!.y).toBeLessThan(8);
    expect(lb!.height).toBeLessThan(80);
    await page.keyboard.press("Escape");
  });

  test("a failing deletion names what was and was not deleted", async ({ page, api }) => {
    await openVM(page, vm, "snapshots");
    await node(page, "d").getByRole("button", { name: "Delete d" }).click();
    const dlg = dialog(page, "Delete d");
    await expect(dlg.getByRole("button", { name: "Delete 1 snapshot" })).toBeEnabled();
    // the snapshot disappears behind the dialog's back
    await api.del(`${base()}/snapshots/d`);
    await dlg.getByRole("button", { name: "Delete 1 snapshot" }).click();
    await expect(dlg.locator(".error")).toContainText("this VM has no snapshot named d");
    await expect(dlg).toBeVisible();
    await page.keyboard.press("Escape");
  });

  test("the API validates every name and refuses what libvirt would refuse", async ({ api }) => {
    const call = (path: string, data: unknown) => api.call("POST", `${base()}${path}`, data, [400]);
    expect(await call("/snapshots/delete", { names: [] })).toMatchObject({ error: expect.stringContaining("at least one") });
    expect(await call("/snapshots/delete", { names: ["nope"] })).toMatchObject({ error: expect.stringContaining("no snapshot named nope") });
    expect(await call("/snapshots/delete", { names: ["../x"] })).toMatchObject({ error: expect.stringContaining("snapshot name must") });
    const plan = await call("/snapshots/delete-plan", { names: ["a", longName] });
    expect(plan.order).toEqual([longName, "a"]); // the tip of the branch first
    expect(plan.problems).toEqual([]);
    expect((await snaps(api)).length).toBe(2); // planning changed nothing
  });

  test("an external snapshot on this AppArmor node is shown disabled with the reason", async ({ page, api, n1 }) => {
    vm2 = await createCirros(api, n1, NAME2, { start: true });
    await api.post(`/nodes/${n1}/vms/${vm2.uuid}/snapshots`, { name: "x1", memory: false });
    const list = await api.get(`/nodes/${n1}/vms/${vm2.uuid}/snapshots`);
    test.skip(list[0].kind !== "external" || list[0].deletable, "this node can delete external snapshots");
    expect(list[0].reason).toContain("AppArmor");
    await openVM(page, vm2, "snapshots");
    await node(page, "x1").getByRole("button", { name: "Delete x1" }).click();
    const dlg = dialog(page, "Delete x1");
    await expect(dlg.getByRole("checkbox", { name: "Select x1" })).toBeDisabled();
    await expect(dlg.locator(".srow", { hasText: "x1" })).toContainText("AppArmor");
    await expect(dlg.getByRole("button", { name: /^Delete \d+ snapshot/ })).toBeDisabled();
    await page.keyboard.press("Escape");
  });

  test.describe("schedule times are entered in the browser time zone and stored in UTC", () => {
    for (const [zone, utcRe, shown] of [
      ["America/New_York", /^1[23]:00$/, "08:00"],
      ["Asia/Kolkata", /^02:30$/, "08:00"],
      ["Pacific/Auckland", /^(19|20):00$/, "08:00"],
    ] as const) {
      test(`${zone}: 08:00 local is stored as UTC and shown as 08:00 again`, async ({ browser, api }) => {
        const ctx = await browser.newContext({ storageState: ".auth/admin.json", timezoneId: zone });
        const page = await ctx.newPage();
        await openVM(page, vm, "snapshots");
        const sec = page.locator("#snap-schedule");
        await sec.getByRole("button", { name: "Set up schedule" }).click();
        await expect(field(sec, "Time")).toHaveValue("03:00");
        await expect(sec).not.toContainText("UTC");
        await field(sec, "Time").fill(shown);
        await sec.getByRole("button", { name: "Save schedule" }).click();
        await expect(sec).toContainText(`Daily at ${shown}`);
        await expect(sec).not.toContainText("UTC");
        const got = await api.get(`${base()}/snapshot-schedule`);
        expect(got.schedule.at).toMatch(utcRe);
        await api.del(`${base()}/snapshot-schedule`);
        await ctx.close();
      });
    }
    test("Pacific/Auckland on the evening before daylight saving ends: the next occurrence is in NZST", async ({ browser, api }) => {
      const ctx = await browser.newContext({ storageState: ".auth/admin.json", timezoneId: "Pacific/Auckland" });
      const page = await ctx.newPage();
      await page.clock.install({ time: new Date("2026-04-04T12:00:00Z") });
      await openVM(page, vm, "snapshots");
      const sec = page.locator("#snap-schedule");
      await sec.getByRole("button", { name: "Set up schedule" }).click();
      await field(sec, "Time").fill("08:00");
      await sec.getByRole("button", { name: "Save schedule" }).click();
      await expect(sec).toContainText("Daily at");
      // the next 08:00 in Auckland is on 5 April, already NZST (+12)
      expect((await api.get(`${base()}/snapshot-schedule`)).schedule.at).toBe("20:00");
      await api.del(`${base()}/snapshot-schedule`);
      await ctx.close();
    });
    test("an interval is entered in hours; an old minutes value is rounded and the form says so", async ({ page, api }) => {
      await api.put(`${base()}/snapshot-schedule`, { enabled: true, mode: "interval", everyMinutes: 90, keep: 3, prefix: "auto", memory: false, quiesce: false });
      await openVM(page, vm, "snapshots");
      const sec = page.locator("#snap-schedule");
      await expect(sec).toContainText("Every 90 minutes");
      await sec.getByRole("button", { name: "Edit schedule" }).click();
      await expect(field(sec, "Every (hours)")).toHaveValue("2");
      await expect(sec).toContainText("rounds that to whole hours");
      await sec.getByRole("button", { name: "Save schedule" }).click();
      await expect(sec).toContainText("Every 2 hours");
      expect((await api.get(`${base()}/snapshot-schedule`)).schedule).toMatchObject({ everyMinutes: 120, enabled: true });
      await api.del(`${base()}/snapshot-schedule`);
    });
  });

  test("Next Run: the server computes it, the page shows local time", async ({ page, api }) => {
    const sched = { enabled: true, mode: "daily", at: "03:30", keep: 3, prefix: `${P}auto`, memory: false, quiesce: false };
    await api.put(`${base()}/snapshot-schedule`, sched);
    // a schedule that has taken nothing yet is due at once (the scheduler's own rule): the answer is "now"
    const first = await api.get(`${base()}/snapshot-schedule`);
    expect(Math.abs(first.nextRun - Date.now() / 1000)).toBeLessThan(5);
    // once a scheduled snapshot exists, the next run is the next 03:30 UTC
    await api.post(`${base()}/snapshots`, { name: `${P}auto-20260101-000000`, description: "Scheduled snapshot (virticle)" });
    const got = await api.get(`${base()}/snapshot-schedule`);
    expect(got.nextRun).toBeGreaterThan(Date.now() / 1000);
    expect(got.nextRun - Date.now() / 1000).toBeLessThanOrEqual(86400 + 60);
    expect(new Date(got.nextRun * 1000).toISOString().slice(11, 16)).toBe("03:30");
    expect(["this", "disabled"]).toContain(got.runner); // "disabled" when the server runs with -no-scheduler
    await openVM(page, vm, "snapshots");
    const line = page.locator("#snap-schedule .nextrun").first();
    await expect(line).toContainText("Next Run:");
    if (got.runner === "disabled") await expect(line).toContainText("-no-scheduler");
    // paused schedule: no next run
    await api.put(`${base()}/snapshot-schedule`, { enabled: false, mode: "daily", at: "03:30", keep: 3, prefix: `${P}auto`, memory: false, quiesce: false });
    expect((await api.get(`${base()}/snapshot-schedule`)).nextRun).toBeUndefined();
    await api.del(`${base()}/snapshot-schedule`);
  });

  test("Next Run with a scheduler: local time, one timer refreshes the text when it passes", async ({ browser }) => {
    const ctx = await browser.newContext({ storageState: ".auth/admin.json", timezoneId: "America/New_York" });
    const page = await ctx.newPage();
    const soon = Math.floor(Date.now() / 1000) + 4;
    let requests = 0;
    await page.route(`**${base()}/snapshot-schedule`, async (route) => {
      requests++;
      const r = { schedule: { enabled: true, mode: "daily", at: "03:00", keep: 3, prefix: "x", memory: false, quiesce: false }, runner: "this", tick: 20, nextRun: requests === 1 ? soon : soon + 86400 };
      await route.fulfill({ json: r });
    });
    await openVM(page, vm, "snapshots");
    const line = page.locator("#snap-schedule .nextrun").first();
    await expect(line).toContainText("Next Run:");
    await expect(line).toContainText(/E[SD]T/);
    expect(requests).toBe(1);
    const first = (await line.textContent())!;
    // when the time passes the page asks once more and shows the following run
    await expect(line).not.toHaveText(first, { timeout: 15_000 });
    await expect.poll(() => requests, { timeout: 15_000 }).toBeGreaterThanOrEqual(2); // a refetch when the time passes
    const n = requests;
    await page.waitForTimeout(4000);
    expect(requests - n).toBeLessThanOrEqual(1); // and no polling after it
    await expect(line).toContainText("Next Run:");
    await ctx.close();
  });

  test("backup schedule: Next Run and the local time in the form", async ({ page, api }) => {
    await api.put(`${base()}/backup-schedule`, { enabled: true, mode: "daily", at: "04:10", targetNode: 0, pool: "default", keepLast: 2 });
    const got = await api.get(`${base()}/backup-schedule`);
    // no backup exists yet, so the schedule is due at once (the scheduler's own rule)
    expect(Math.abs(got.nextRun - Date.now() / 1000)).toBeLessThan(10);
    expect(got.runner).toBe("disabled");
    await openVM(page, vm, "backups");
    const sec = page.locator("#backup-schedule");
    await expect(sec.locator(".nextrun").first()).toContainText("Next Run:");
    await expect(sec.locator(".nextrun").first()).toContainText("-no-scheduler");
    await sec.getByRole("button", { name: "Edit schedule" }).click();
    await expect(field(sec, "Time")).toHaveValue(/^\d\d:\d\d$/);
    await expect(sec).not.toContainText("UTC");
    await api.del(`${base()}/backup-schedule`);
  });

  test.describe("bridge in the Network select", () => {
    const addInterface = async (page: Page) => {
      await openVM(page, vm, "hardware");
      await page.getByRole("button", { name: "Add interface" }).click();
      return dialog(page, "Add network interface");
    };
    test("one list of virtual networks and the host bridges that no virtual network manages", async ({ page, api, n1 }) => {
      const nets: any[] = await api.get(`/nodes/${n1}/networks`);
      const nics: any[] = (await api.get(`/nodes/${n1}/interfaces`)).interfaces;
      const managed = new Set(nets.map((n) => n.bridge).filter(Boolean));
      const bridges = nics.filter((n) => (n.type === "bridge" || n.kind === "openvswitch") && !managed.has(n.name)).map((n) => n.name);
      const dlg = await addInterface(page);
      const combo = dlg.getByRole("combobox", { name: "Network", exact: true });
      await expect(dlg.getByRole("button", { name: "Host bridge" })).toHaveCount(0); // no second picker
      await expect(combo).not.toHaveText("");
      await combo.click();
      const opts = page.getByRole("listbox").getByRole("option");
      await expect(opts).toHaveCount(nets.length + bridges.length);
      for (const n of nets) await expect(page.getByRole("option", { name: new RegExp("^" + n.name + " \\(") })).toHaveCount(1);
      for (const b of bridges) await expect(page.getByRole("option", { name: new RegExp("^" + b + " \\(host bridge\\)") })).toHaveCount(1);
      // a bridge that libvirt manages is not listed twice: pick its virtual network instead
      for (const n of nets.filter((x) => x.bridge)) await expect(page.getByRole("option", { name: new RegExp("^" + n.bridge + " \\(host bridge\\)") })).toHaveCount(0);
      if (bridges.length > 0) {
        await page.getByRole("option", { name: new RegExp("^" + bridges[0] + " \\(host bridge\\)") }).click();
        const req = page.waitForRequest((r) => r.method() === "POST" && /\/nics$/.test(r.url()));
        await dlg.getByRole("button", { name: "Add", exact: true }).click();
        expect((await req).postDataJSON()).toMatchObject({ bridge: bridges[0] });
        await expect(dlg).toBeHidden();
        await expect(page.locator("#hw-nics")).toContainText(bridges[0]);
      } else {
        await page.keyboard.press("Escape");
        await page.keyboard.press("Escape");
      }
    });
    test("Model and MAC address are under Advanced; Escape closes only the list", async ({ page }) => {
      const dlg = await addInterface(page);
      await expect(dlg.getByRole("combobox", { name: "Model", exact: true })).toHaveCount(0);
      await dlg.getByRole("button", { name: "Advanced" }).click();
      await expect(dlg.getByRole("combobox", { name: "Model", exact: true })).toBeVisible();
      await expect(field(dlg, "MAC address")).toBeVisible();
      await dlg.getByRole("combobox", { name: "Network", exact: true }).click();
      await page.keyboard.press("Escape");
      await expect(page.getByRole("listbox")).toHaveCount(0);
      await expect(dlg).toBeVisible();
      await page.keyboard.press("Escape");
    });
    test("the server refuses a name that is not a bridge, or not there, with a clear message", async ({ api, n1 }) => {
      const nics: any[] = (await api.get(`/nodes/${n1}/interfaces`)).interfaces;
      const plain = nics.find((n) => n.type === "physical");
      const call = (bridge: string) => api.call("POST", `${base()}/nics`, { bridge, model: "virtio" }, [400]);
      if (plain) expect(await call(plain.name)).toMatchObject({ error: `${plain.name} is a physical interface of this node, not a bridge` });
      expect(await call("br-nope")).toMatchObject({ error: "this node has no interface named br-nope" });
    });
  });
});
