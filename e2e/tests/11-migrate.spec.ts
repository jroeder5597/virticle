// Migrate dialog: pre-flight list, live migration n1 -> n2 and back, progress, landing on the new node's page.
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, expectBadge, vmAction } from "../lib/ui";

const NAME = `${P}mig1`;

test.describe.serial("live migration", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api }) => cleanVMs(api, NAME));

  async function migrate(page: any, api: any, from: number, to: number, toName: string) {
    await openVM(page, { node: from, uuid: vm.uuid, name: NAME });
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${NAME}`);
    await expect(dlg.getByRole("combobox", { name: "Target node" })).toContainText(toName);
    await expect(dlg.getByLabel(/Live migration/)).toBeChecked();
    // the pre-flight list renders with at least one entry, none of them an error
    const checks = dlg.locator("ul.checklist li");
    await expect(checks.first()).toBeVisible({ timeout: 60_000 });
    expect(await checks.count()).toBeGreaterThan(2);
    await expect(dlg.getByRole("img", { name: "failed" })).toHaveCount(0);
    await expect(dlg).toContainText("Live: the VM keeps running");
    const go = dlg.getByRole("button", { name: "Migrate", exact: true });
    await expect(go).toBeEnabled();
    await go.click();
    // progress is shown while the task runs
    await expect(dlg.getByRole("progressbar")).toBeVisible();
    await expect(dlg).toContainText(`Moving ${NAME} to ${toName}`);
    await expect(dlg).toContainText("Done.", { timeout: 240_000 });
    // and the page follows the VM to its new node
    await expect(page).toHaveURL(new RegExp(`/vm/${to}/${vm.uuid}`), { timeout: 30_000 });
    await expect(page.getByRole("heading", { level: 1, name: NAME })).toBeVisible();
    await expect(page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Hardware", exact: true }) })).toContainText(toName);
    const d = await api.vmDetail(to, vm.uuid);
    expect(d.state).toBe("running");
    expect(d.nodeName).toBe(toName);
    await expectBadge(page.locator(".head"), "running");
  }

  test("migrate n1 to n2 while running", async ({ page, api, n1, n2 }) => {
    test.setTimeout(360_000);
    vm = await createCirros(api, n1, NAME, { start: true });
    await migrate(page, api, n1, n2, env.node2);
  });

  test("and back to n1", async ({ page, api, n1, n2 }) => {
    test.setTimeout(360_000);
    await migrate(page, api, n2, n1, env.node1);
  });

  test("a stopped VM gets the offline pre-flight and no live option", async ({ page, api, n1 }) => {
    // a target that is not running is not offered; with the VM stopped the offline path is checked instead
    await api.post(`/nodes/${n1}/vms/${vm.uuid}/action`, { action: "poweroff" });
    await api.waitFor("off", async () => (await api.vmDetail(n1, vm.uuid)).state === "shutoff");
    await openVM(page, { node: n1, uuid: vm.uuid, name: NAME });
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${NAME}`);
    await expect(dlg).toContainText("Offline: the VM is shut off", { timeout: 60_000 });
    await expect(dlg.getByLabel(/Live migration/)).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });
});
