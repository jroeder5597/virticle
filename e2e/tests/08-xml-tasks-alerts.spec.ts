// XML editor (valid and invalid), the Tasks page with failures, the Alerts panel.
import { cleanVMs, createCirros, expect, openVM, tab, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { row } from "../lib/ui";

const NAME = `${P}xml1`;

test.describe.serial("XML editor, tasks, alerts", () => {
  let vm: { node: number; uuid: string; name: string };

  test.afterAll(async ({ api, n1 }) => {
    await cleanVMs(api, NAME);
    await api.del(`/nodes/${n1}/pools/default/volumes/${P}dupvol.qcow2`).catch(() => {});
  });

  test("a valid XML edit saves and shows up in the definition", async ({ page, api, n1 }) => {
    vm = await createCirros(api, n1, NAME);
    await openVM(page, vm, "xml");
    const ta = page.getByLabel("Domain XML");
    await expect(page.getByRole("button", { name: "Save" })).toBeDisabled();
    const xml = await ta.inputValue();
    expect(xml).toContain(`<name>${NAME}</name>`);
    await ta.fill(xml.replace(`</name>`, `</name>\n  <description>edited by the e2e suite</description>`));
    await expect(page.getByRole("button", { name: "Save" })).toBeEnabled();
    await page.getByRole("button", { name: "Save" }).click();
    await expect(page.getByText("Saved.")).toBeVisible();
    await expect(page.locator(".xml .error")).toHaveCount(0);
    expect((await api.vmDetail(vm.node, vm.uuid)).xml).toContain("edited by the e2e suite");
  });

  test("an invalid XML edit shows libvirt's error and keeps the text", async ({ page, api }) => {
    await openVM(page, vm, "xml");
    const ta = page.getByLabel("Domain XML");
    const xml = await ta.inputValue();
    const bad = xml.replace("<vcpu", "<vcpux").replace("</vcpu>", "</vcpux>");
    await ta.fill(bad);
    await page.getByRole("button", { name: "Save" }).click();
    const err = page.locator(".xml .error");
    await expect(err).toBeVisible();
    await expect(err).toContainText(/xml|element|vcpu|schema|parse/i);
    await expect(ta).toHaveValue(bad);
    await expect(page.getByText("Saved.")).toHaveCount(0);
    // Revert restores the saved definition
    await page.getByRole("button", { name: "Revert" }).click();
    await expect(ta).toHaveValue(/edited by the e2e suite/);
    await expect(page.locator(".xml .error")).toHaveCount(0);
    expect((await api.vmDetail(vm.node, vm.uuid)).vcpus).toBe(1);
  });

  test("Tasks page lists a failed task with its error text, and a toast announces it", async ({ page, api, n1 }) => {
    const body = { name: `${P}dupvol.qcow2`, capacity: 2 ** 20 * 64, format: "qcow2" };
    await api.post(`/nodes/${n1}/pools/default/volumes`, body);
    await page.goto("/tasks");
    await expect(page.getByRole("heading", { name: "Tasks" })).toBeVisible();
    // the second create fails: the toast shows up wherever the user is, and the table keeps the error
    await api.call("POST", `/nodes/${n1}/pools/default/volumes`, body, [400, 409, 422, 500]);
    const failed = row(page.locator("main"), `default/${P}dupvol.qcow2`).filter({ hasText: "Error" });
    await expect(failed.first()).toBeVisible();
    await expect(failed.first().locator(".statusline")).not.toBeEmpty();
    await expect(page.locator(".toast", { hasText: "failed" }).first()).toBeVisible();
    const ok = row(page.locator("main"), `default/${P}dupvol.qcow2`).filter({ hasText: "ok" });
    await expect(ok.first()).toBeVisible();
  });

  test("Alerts panel opens, lists or says nothing needs attention, closes with Escape", async ({ page }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: /^Alerts/ }).click();
    const panel = page.getByRole("dialog", { name: "Alerts" });
    await expect(panel).toBeVisible();
    await expect(panel.locator("ul li, .pad").first()).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
  });
});
