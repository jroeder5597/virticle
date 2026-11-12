// One design language for results: a dot (green ok, yellow warning, red failed, hollow not run) and
// plain text in the normal colour. Covers Test connection (steps, a wrong SSH user, a wrong
// address) and a migration pre-flight with a warning. Nothing is saved.
import { Locator, Page } from "@playwright/test";
import { cleanVMs, createCirros, expect, openVM, test } from "../lib/fixtures";
import { P } from "../lib/env";
import { dialog, field, vmAction } from "../lib/ui";

const VM = `${P}chk1`;

async function openEdit(page: Page, node: number) {
  await page.goto(`/node/${node}/settings`);
  await page.getByRole("button", { name: "Edit connection" }).click();
  const dlg = dialog(page, "Edit connection");
  await expect(dlg.locator(".keybox code")).toContainText("ssh-ed25519");
  return dlg;
}

const step = (dlg: Locator, title: string | RegExp) => dlg.locator("ul.checklist li").filter({ hasText: title });

test.describe.serial("result checks", () => {
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
  });

  test("Test connection lists its steps with green dots", async ({ page, n1 }) => {
    const dlg = await openEdit(page, n1);
    await dlg.getByRole("button", { name: "Test connection" }).click();
    const list = dlg.locator("ul.checklist");
    await expect(list).toContainText("libvirt reachable", { timeout: 30_000 });
    for (const t of ["Address resolves", "Host reachable", "SSH host key", /^SSH sign-in as /, "libvirt reachable", "Node access", "Migration address"]) {
      await expect(step(dlg, t)).toHaveCount(1);
    }
    await expect(dlg.getByRole("img", { name: "failed" })).toHaveCount(0);
    await expect(dlg.getByRole("img", { name: "not run" })).toHaveCount(0);
    await expect(step(dlg, "libvirt reachable").getByRole("img", { name: "ok" })).toBeVisible();
    await expect(dlg.locator(".checksum")).toContainText("passed");
    // the green is the colour of a running VM's dot in the tree
    const same = await page.evaluate(() => {
      const probe = document.createElement("span");
      probe.className = "dot v-running";
      document.body.appendChild(probe);
      const running = getComputedStyle(probe).backgroundColor;
      probe.remove();
      const ok = document.querySelector(".checklist .cdot.c-ok");
      return { running, ok: ok ? getComputedStyle(ok).backgroundColor : "" };
    });
    expect(same.ok).toBe(same.running);
    await page.keyboard.press("Escape");
  });

  test("a wrong SSH user is a red dot on the sign-in step and the rest is not run", async ({ page, api, n1 }) => {
    const before = (await api.nodes()).find((n) => n.id === n1)!;
    const dlg = await openEdit(page, n1);
    await field(dlg, "SSH user").fill("e2enosuchuser");
    await dlg.getByRole("button", { name: "Test connection" }).click();
    const bad = step(dlg, "SSH sign-in as e2enosuchuser");
    await expect(bad.getByRole("img", { name: "failed" })).toBeVisible({ timeout: 40_000 });
    await expect(step(dlg, "Host reachable").getByRole("img", { name: "ok" })).toBeVisible();
    await expect(step(dlg, "SSH host key").getByRole("img", { name: /ok|info/ })).toBeVisible();
    for (const t of ["libvirt reachable", "Node access", "Migration address"]) {
      const s = step(dlg, t);
      await expect(s.getByRole("img", { name: "not run" })).toBeVisible();
      await expect(s).toContainText("Not run");
    }
    await expect(dlg.locator(".checksum")).toContainText("failed");
    await page.keyboard.press("Escape");
    expect((await api.nodes()).find((n) => n.id === n1)!.user).toBe(before.user);
  });

  test("an unreachable address is a red dot on the reach step", async ({ page, api, n1 }) => {
    const before = (await api.nodes()).find((n) => n.id === n1)!;
    const dlg = await openEdit(page, n1);
    await field(dlg, "Address").fill("192.168.150.99");
    await dlg.getByRole("button", { name: "Test connection" }).click();
    await expect(step(dlg, "Host reachable").getByRole("img", { name: "failed" })).toBeVisible({ timeout: 40_000 });
    await expect(step(dlg, "SSH host key").getByRole("img", { name: "not run" })).toBeVisible();
    await expect(step(dlg, /^SSH sign-in as /).getByRole("img", { name: "not run" })).toBeVisible();
    await page.keyboard.press("Escape");
    expect((await api.nodes()).find((n) => n.id === n1)!.addr).toBe(before.addr);
  });

  test("a pre-flight warning is a yellow dot and the text keeps the normal colour", async ({ page, api, n1 }) => {
    const vm = await createCirros(api, n1, VM);
    // make sure the list has a warning whatever the lab's encryption state is
    await page.route("**/migrate/check", async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.checks = [...(body.checks ?? []), { check: "E2E warning", ok: false, severity: "warning", message: "a made-up warning" }];
      await route.fulfill({ response: res, json: body });
    });
    await openVM(page, vm);
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${VM}`);
    const row = dlg.locator("ul.checklist li").filter({ hasText: "E2E warning" });
    await expect(row).toBeVisible({ timeout: 60_000 });
    await expect(row.getByRole("img", { name: "warning" })).toBeVisible();
    const colours = await row.evaluate((li) => {
      const dot = getComputedStyle(li.querySelector(".cdot")!).backgroundColor;
      const title = getComputedStyle(li.querySelector("strong")!).color;
      const text = getComputedStyle(li.querySelector(".ctext")!).color;
      const row = getComputedStyle(li);
      const root = getComputedStyle(document.documentElement);
      const probe = document.createElement("span");
      probe.style.color = root.getPropertyValue("--text");
      probe.style.backgroundColor = root.getPropertyValue("--warn");
      document.body.appendChild(probe);
      const want = { text: getComputedStyle(probe).color, warn: getComputedStyle(probe).backgroundColor };
      probe.remove();
      return { dot, title, text, want, border: row.borderTopColor, borderW: row.borderTopWidth, bg: row.backgroundColor };
    });
    expect(colours.dot).toBe(colours.want.warn);
    expect(colours.title).toBe(colours.want.text);
    expect(colours.text).toBe(colours.want.text);
    expect(colours.borderW).toBe("0px");
    expect(colours.bg).toBe("rgba(0, 0, 0, 0)");
    await page.keyboard.press("Escape");
  });
});
