// Notifications: a destination is added, its address is never shown again in full, the test message
// reports a failure in plain words, and the destination is deleted.
// Safe on any lab: the only destination points at the controller's own address, at a path that does not
// exist, so nothing is ever sent to anybody else.
import { env, P } from "../lib/env";
import { expect, test } from "../lib/fixtures";
import { dialog, field, pick } from "../lib/ui";

const NAME = `${P}notify`;
const PATH = "/api/__notify-test";
const URL_ = env.base.replace(/\/$/, "") + PATH;

test.describe.serial("notifications", () => {
  test.afterAll(async ({ api }) => {
    const r = await api.get("/notify/destinations");
    for (const d of r.destinations) if (d.name === NAME) await api.del(`/notify/destinations/${d.id}`).catch(() => {});
  });

  test("add a webhook destination; the address is not shown again", async ({ page, api }) => {
    await page.goto("/alerts-settings");
    await expect(page.getByRole("heading", { name: "Notifications" })).toBeVisible();
    await page.getByRole("button", { name: "Add destination" }).click();
    const dlg = dialog(page, "Add destination");
    await pick(dlg, "Type", "Webhook");
    await field(dlg, "Name").fill(NAME);
    await field(dlg, "URL").fill(URL_);
    // The choices below "Advanced" are closed by default and keep their defaults.
    await expect(dlg.getByRole("button", { name: "Advanced" })).toHaveAttribute("aria-expanded", "false");
    await dlg.getByRole("button", { name: "Add destination" }).click();
    await expect(dlg).toBeHidden();

    const item = page.getByRole("listitem", { name: NAME });
    await expect(item).toBeVisible();
    await expect(item).toContainText("Warning and critical");
    await expect(item).toContainText(new URL(env.base).host);
    await expect(item).toContainText("ends in");
    await expect(page.locator("main")).not.toContainText("__notify-test");
    const listed = JSON.stringify(await api.get("/notify/destinations"));
    expect(listed).not.toContain("__notify-test");
    expect(listed).not.toContain(PATH);
  });

  test("the test message reports the failure in plain words", async ({ page }) => {
    await page.goto("/alerts-settings");
    const item = page.getByRole("listitem", { name: NAME });
    await item.getByRole("button", { name: "Send test message" }).click();
    await expect(item).toContainText(/Failing: .*(does not know this address|did not accept|Cannot|refused)/);
  });

  test("editing keeps the saved address; delete removes the destination", async ({ page }) => {
    await page.goto("/alerts-settings");
    const item = page.getByRole("listitem", { name: NAME });
    await item.getByRole("button", { name: "Edit" }).click();
    const dlg = dialog(page, "Edit destination");
    await expect(field(dlg, "URL")).toHaveAttribute("placeholder", /^Saved: .*ends in/);
    await dlg.getByRole("button", { name: "Cancel" }).click();
    await item.getByRole("button", { name: "Delete" }).click();
    await dialog(page, `Delete ${NAME}`).getByRole("button", { name: "Delete destination" }).click();
    await expect(item).toHaveCount(0);
  });
});
