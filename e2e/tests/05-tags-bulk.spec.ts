// Tags on the detail page, list filter and search, then bulk select -> tag -> force off -> delete.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { autoDialogs, dialog, expectBadge, field, row, confirm, confirmDialog } from "../lib/ui";

const A = `${P}bulk-a`;
const B = `${P}bulk-b`;

test.describe.serial("tags and bulk actions", () => {
  let a: { node: number; uuid: string; name: string };
  let b: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, A, B));

  test("tags: add on the detail page, filter and search the list, remove", async ({ page, api, n1 }) => {
    a = await createCirros(api, n1, A, { start: true });
    b = await createCirros(api, n1, B, { start: true });
    await openVM(page, a);
    const input = page.getByPlaceholder("add tag");
    await input.fill("blue team");
    await input.press("Enter");
    await expect(page.locator(".tagedit .tag", { hasText: "blue team" })).toBeVisible();
    expect((await api.vmDetail(a.node, a.uuid)).tags).toEqual(["blue team"]);
    // an invalid tag is refused with a message
    await input.fill("-bad");
    await input.press("Enter");
    await expect(page.locator(".tagedit .error")).toBeVisible();
    await input.fill("");

    await page.goto("/datacenter/vms");
    await expect(row(page, A).locator(".tag", { hasText: "blue team" })).toBeVisible();
    await page.getByLabel("Filter by tag").selectOption("blue team");
    await expect(row(page, A)).toBeVisible();
    await expect(row(page, B)).toHaveCount(0);
    await expect(page.getByText(/1 of \d+ shown/)).toBeVisible();
    await page.getByLabel("Filter by tag").selectOption("");
    await page.getByLabel("Search VMs").fill(`${P}bulk-b`);
    await expect(row(page, B)).toBeVisible();
    await expect(row(page, A)).toHaveCount(0);
    await page.getByLabel("Search VMs").fill("zzz-no-such-vm");
    await expect(page.getByText("No VM matches the search or tag filter.")).toBeVisible();
    await page.getByLabel("Search VMs").fill("");

    // clicking a chip in the list filters by it
    await row(page, A).locator(".tag.pick").click();
    await expect(page.getByLabel("Filter by tag")).toHaveValue("blue team");
    await page.getByLabel("Filter by tag").selectOption("");

    await openVM(page, a);
    await page.getByRole("button", { name: "Remove tag blue team" }).click();
    await expect(page.locator(".tagedit .tag")).toHaveCount(0);
    expect((await api.vmDetail(a.node, a.uuid)).tags ?? []).toEqual([]);
  });

  test("bulk: select, tag, force off, delete with typed confirmation", async ({ page, api }) => {
    autoDialogs(page);
    await page.goto("/datacenter/vms");
    await page.getByLabel("Search VMs").fill(`${P}bulk-`);
    await expect(row(page, A)).toBeVisible();
    await expect(row(page, B)).toBeVisible();
    await page.getByLabel("Select all shown VMs").check();
    const bar = page.locator(".bulkbar");
    await expect(bar).toContainText("2 selected");

    // tag both
    await bar.getByRole("button", { name: "Tags" }).click();
    const td = dialog(page, "Tags on the selected VMs");
    await expect(field(td, "Tag")).toBeFocused();
    await field(td, "Tag").fill("bulk-test");
    await td.getByRole("button", { name: "Apply" }).click();
    const res = dialog(page, /^Add tag "bulk-test"/);
    await expect(res).toContainText("2 of 2 done");
    await res.getByRole("button", { name: "Close", exact: true }).click();
    for (const v of [a, b]) expect((await api.vmDetail(v.node, v.uuid)).tags).toContain("bulk-test");
    await expect(row(page, A).locator(".tag", { hasText: "bulk-test" })).toBeVisible();

    // Delete is refused for running VMs: they are listed as skipped
    await bar.getByRole("button", { name: "Delete" }).click();
    const early = dialog(page, /^Delete 0 VM/);
    await expect(early).toContainText("will be skipped");
    await expect(early.getByRole("button", { name: /Delete 0 VM/ })).toBeDisabled();
    await early.getByRole("button", { name: "Cancel" }).click();

    // force off both
    await bar.getByRole("button", { name: "Halt (Force Shutdown)" }).click();
    // one confirmation for the whole selection, listing both VMs
    const ask = dialog(page, /^Halt \(Force Shutdown\): 2 VMs/);
    await expect(ask).toContainText(A);
    await expect(ask).toContainText(B);
    await confirm(page, /^Halt \(Force Shutdown\): 2 VMs/);
    const off = dialog(page, "Halt (Force Shutdown)");
    await expect(off).toContainText("2 of 2 done");
    await off.getByRole("button", { name: "Close", exact: true }).click();
    await expectBadge(row(page, A), "shutoff");
    await expectBadge(row(page, B), "shutoff");

    // delete, with the typed word
    await bar.getByRole("button", { name: "Delete" }).click();
    const del = dialog(page, "Delete 2 VM(s)?");
    const go = del.getByRole("button", { name: "Delete 2 VM(s)" });
    await expect(go).toBeDisabled();
    await expect(field(del, 'Type "delete" to confirm')).toBeFocused();
    await field(del, 'Type "delete" to confirm').fill("delet");
    await expect(go).toBeDisabled();
    await field(del, 'Type "delete" to confirm').fill("delete");
    await del.getByLabel("Also delete their disk volumes").check();
    await go.click();
    const done = dialog(page, /^Delete: 2 of 2 done/);
    await expect(done).toBeVisible();
    await done.getByRole("button", { name: "Close", exact: true }).click();
    await expect(row(page, A)).toHaveCount(0);
    expect(await api.vmByName(A)).toBeUndefined();
    expect(await api.vmByName(B)).toBeUndefined();
    const vols = await api.get(`/nodes/${a.node}/pools/default/volumes`);
    expect(vols.filter((v: any) => v.name.startsWith(`${P}bulk-`))).toHaveLength(0);
  });
});
