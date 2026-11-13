// Snapshots tab: take (with memory), tree, revert, delete, and the schedule form.
import { cleanVMs, createCirros, expect, openVM, tab, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row, confirm, pick } from "../lib/ui";

const NAME = `${P}snap1`;
/** A snapshot's row in the tree. */
const node = (page: any, name: string) => page.getByRole("treeitem", { name: new RegExp(`^${name}(,|$)`) });

test.describe.serial("snapshots", () => {
  test.use({ timezoneId: "UTC" }); // schedule times are entered in browser time and stored in UTC
  let vm: { node: number; uuid: string; name: string };
  const snaps = (api: any) => api.get(`/nodes/${vm.node}/vms/${vm.uuid}/snapshots`) as Promise<any[]>;

  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  test("take a snapshot with memory of the running VM", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, NAME, { start: true });
    await openVM(page, vm, "snapshots");
    await expect(page.getByText("No snapshots yet.")).toBeVisible();
    await page.getByRole("button", { name: "Take snapshot" }).first().click();
    const dlg = dialog(page, "Take snapshot");
    await expect(dlg.getByLabel(/Save the running state too/)).toBeChecked();
    await field(dlg, "Name").fill("first");
    await field(dlg, "Description").fill("before the change");
    await dlg.getByRole("button", { name: "Take snapshot" }).click();
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    const r = node(page, "first");
    await expect(r).toBeVisible();
    await expect(r).toContainText("with memory");
    await expect(r).toContainText("before the change");
    await expect(r.locator(".badge", { hasText: "current" })).toBeVisible();
    const list = await snaps(api);
    expect(list.map((s) => s.name)).toEqual(["first"]);
    expect(list[0].hasMemory).toBe(true);
  });

  test("a second snapshot becomes a child in the tree", async ({ page, api }) => {
    await openVM(page, vm, "snapshots");
    await page.getByRole("button", { name: "Take snapshot" }).first().click();
    const dlg = dialog(page, "Take snapshot");
    await field(dlg, "Name").fill("second");
    await field(dlg, "Name").press("Enter"); // Enter submits dialogs that are not forms too
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    const child = node(page, "second");
    await expect(child).toBeVisible();
    await expect(child).toHaveAttribute("aria-level", "2"); // one level below its parent
    await expect(node(page, "first")).toHaveAttribute("aria-level", "1");
    await expect(node(page, "first").locator(".badge", { hasText: "current" })).toHaveCount(0);
    await expect(page.getByRole("treeitem", { name: /You are here/ })).toHaveAttribute("aria-level", "3");
    const list = await snaps(api);
    expect(list.find((s) => s.name === "second").parent).toBe("first");
  });

  test("the dialog offers only what the earlier snapshots allow, with the reason", async ({ page, api }) => {
    // Both snapshots saved the running state. On a node that confines QEMU with AppArmor (or whose earlier
    // snapshots are internal) a snapshot without the state is impossible: the checkbox stays checked and disabled,
    // with the server's sentence under it. Elsewhere the choice is free.
    const o = await api.get(`/nodes/${vm.node}/vms/${vm.uuid}/snapshot-options`);
    expect(o.withState.allowed).toBe(true);
    await openVM(page, vm, "snapshots");
    await page.getByRole("button", { name: "Take snapshot" }).first().click();
    const dlg = dialog(page, "Take snapshot");
    const box = dlg.getByLabel(/Save the running state too/);
    if (o.diskOnly.allowed) {
      await expect(box).toBeEnabled();
    } else {
      await expect(box).toBeChecked();
      await expect(box).toBeDisabled();
      await expect(dlg.getByText(o.diskOnly.reason)).toBeVisible();
      // the server refuses the same request with the same sentence
      expect(await api.call("POST", `/nodes/${vm.node}/vms/${vm.uuid}/snapshots`, { name: "nostate", memory: false }, [400])).toMatchObject({ error: o.diskOnly.reason });
    }
    await page.keyboard.press("Escape");
  });

  test("revert to the first snapshot", async ({ page, api }) => {
    await openVM(page, vm, "snapshots");
    await node(page, "first").getByRole("button", { name: "Revert to first" }).click();
    const dlg = dialog(page, "Revert to first");
    await dlg.getByRole("button", { name: "Revert" }).click();
    await expect(dlg).toBeHidden({ timeout: 60_000 });
    await expect(node(page, "first").locator(".badge", { hasText: "current" })).toBeVisible();
    expect((await snaps(api)).find((s) => s.name === "first").isCurrent).toBe(true);
    await expectBadge(page.locator(".head"), "running");
  });

  test("delete the child and then the parent", async ({ page, api }) => {
    await openVM(page, vm, "snapshots");
    await node(page, "second").getByRole("button", { name: "Delete second" }).click();
    const d1 = dialog(page, "Delete second");
    await d1.getByRole("button", { name: "Delete 1 snapshot" }).click();
    await expect(d1).toBeHidden({ timeout: 60_000 });
    await expect(node(page, "second")).toHaveCount(0);
    await node(page, "first").getByRole("button", { name: "Delete first" }).click();
    const d2 = dialog(page, "Delete first");
    await d2.getByRole("button", { name: "Delete 1 snapshot" }).click();
    await expect(d2).toBeHidden({ timeout: 60_000 });
    await expect(page.getByText("No snapshots yet.")).toBeVisible();
    expect(await snaps(api)).toHaveLength(0);
  });

  test("schedule: save, shown in words, edit, remove", async ({ page, api }) => {
    autoDialogs(page);
    await openVM(page, vm, "snapshots");
    const sec = page.locator("section.card", { hasText: "Scheduled snapshots" });
    await sec.getByRole("button", { name: "Set up schedule" }).click();
    await field(sec, "Time").fill("03:30");
    await field(sec, "Keep the newest").fill("3");
    await sec.getByRole("button", { name: "Save schedule" }).click();
    await expect(sec).toContainText("Daily at 03:30");
    await expect(sec).toContainText("keeping the newest 3");
    let got = await api.get(`/nodes/${vm.node}/vms/${vm.uuid}/snapshot-schedule`);
    expect(got.schedule).toMatchObject({ mode: "daily", at: "03:30", keep: 3, enabled: true, prefix: "auto" });

    await sec.getByRole("button", { name: "Edit schedule" }).click();
    await pick(sec, "When", "At a fixed interval");
    await field(sec, "Every (hours)").fill("12");
    await sec.getByRole("button", { name: "Save schedule" }).click();
    await expect(sec).toContainText("Every 12 hours");

    await sec.getByRole("button", { name: "Remove" }).click();
    await confirm(page, "Remove snapshot schedule");
    await expect(sec.getByRole("button", { name: "Set up schedule" })).toBeVisible();
    got = await api.get(`/nodes/${vm.node}/vms/${vm.uuid}/snapshot-schedule`);
    expect(got.schedule).toBeNull();
  });
});
