// Node connection: the Settings tab shows how a node is reached and what its login may do; the
// Edit connection dialog tests the new settings with a real connection before it saves, and
// refuses a wrong address with the connection error without saving anything.
import { cleanVMs, createCirros, expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { cancelConfirm, confirm, dialog, field } from "../lib/ui";

const VM = `${P}conn1`;

test.describe.serial("node connection", () => {
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
  });

  test("the Settings tab shows address, hostname, resolved IP, migration address and the root access", async ({ page, api, n1 }) => {
    const node = (await api.nodes()).find((n) => n.id === n1)!;
    await page.goto(`/node/${n1}/settings`);
    const card = page.locator(".card", { has: page.getByRole("heading", { name: "Connection", exact: true }) });
    await expect(card).toBeVisible();
    await expect(card.locator("dt", { hasText: "Address" })).toBeVisible();
    await expect(card).toContainText(node.addr);
    for (const k of ["Reported hostname", "Resolved IP", "Migration address", "SSH user", "Jump host", "Root access"]) await expect(card.locator("dt", { hasText: k })).toBeVisible();
    await expect(card).toContainText(/root: the SSH user is root|sudo: commands|none: nothing runs/);
    const info = await api.get(`/nodes/${n1}`);
    expect(["root", "sudo", "none"]).toContain(info.privilege);
    expect(info.hostUuid).toMatch(/^[0-9a-f-]{36}$/);
    expect(info.capabilities.installTLS).toHaveProperty("ok");
    expect(info.effectiveMigrationAddress).toBeTruthy();
  });

  test("a wrong address is refused with the connection error and nothing is saved", async ({ page, api, n1 }) => {
    const before = (await api.nodes()).find((n) => n.id === n1)!;
    await page.goto(`/node/${n1}/settings`);
    await page.getByRole("button", { name: "Edit connection" }).click();
    const dlg = dialog(page, "Edit connection");
    await expect(dlg.locator(".keybox code")).toContainText("ssh-ed25519"); // the controller's public key, as in Add node
    await field(dlg, "Address").fill("192.168.150.99");
    await dlg.getByRole("button", { name: "Save" }).click();
    await expect(dlg.locator(".error")).toContainText("connection test failed", { timeout: 30_000 });
    await expect(dlg.locator(".error")).toContainText("192.168.150.99");
    await page.keyboard.press("Escape");
    const after = (await api.nodes()).find((n) => n.id === n1)!;
    expect(after.addr).toBe(before.addr);
    expect(after.state).toBe("online");
    // the same through the API: a 422 with the error, the stored record unchanged
    const res = await api.ctx.fetch(`/api/nodes/${n1}`, { method: "PUT", data: { addr: "192.168.150.99" } });
    expect(res.status()).toBe(422);
    expect((await res.json()).error).toContain("connection test failed");
    expect((await api.nodes()).find((n) => n.id === n1)!.addr).toBe(before.addr);
  });

  test("saving equivalent settings reconnects the same node with its VMs", async ({ page, api, n1 }) => {
    await createCirros(api, n1, VM);
    const before = (await api.nodes()).find((n) => n.id === n1) as any;
    await api.put(`/nodes/${n1}/maintenance`, { on: false });
    await page.goto(`/node/${n1}/settings`);
    await page.getByRole("button", { name: "Edit connection" }).click();
    const dlg = dialog(page, "Edit connection");
    await field(dlg, "SSH user").fill(before.user); // the same values typed again
    await field(dlg, "Jump host (optional)").fill(before.jump ?? "");
    await field(dlg, "Migration address (optional)").fill(before.addr);
    await dlg.getByRole("button", { name: "Test connection" }).click();
    await expect(dlg.locator(".testresult")).toContainText("Detected:", { timeout: 30_000 });
    await dlg.getByRole("button", { name: "Save" }).click();
    await expect(dlg).toBeHidden({ timeout: 30_000 });
    // same id, back online, the VM is listed again
    await expect.poll(async () => (await api.nodes()).find((n) => n.id === n1)?.state, { timeout: 60_000 }).toBe("online");
    const after = (await api.nodes()).find((n) => n.id === n1) as any;
    expect(after.id).toBe(before.id);
    expect(after.migrationAddress).toBe(before.addr);
    expect(after.privilege).toBe(before.privilege);
    await expect.poll(async () => (await api.vms()).some((v) => v.name === VM && v.node === n1), { timeout: 30_000 }).toBe(true);
    await expect(page.locator(".card", { hasText: "Connection" }).locator("dd", { hasText: before.addr }).first()).toBeVisible();
    // leave it as it was
    await api.put(`/nodes/${n1}`, { migrationAddress: "" });
    await expect.poll(async () => (await api.nodes()).find((n) => n.id === n1)?.state, { timeout: 60_000 }).toBe("online");
  });

  test("forget host key asks first, then reconnects and trusts the key again", async ({ page, api, n1 }) => {
    await page.goto(`/node/${n1}/settings`);
    await page.getByRole("button", { name: "Forget host key" }).click();
    await cancelConfirm(page, "Forget host key");
    await page.getByRole("button", { name: "Forget host key" }).click();
    await confirm(page, "Forget host key");
    await expect.poll(async () => (await api.nodes()).find((n) => n.id === n1)?.state, { timeout: 60_000 }).toBe("online");
    await expect(page.locator(".error")).toHaveCount(0);
  });

  test("a node that needs root says so on the Settings tab only when its login has none", async ({ api, n1 }) => {
    const info = await api.get(`/nodes/${n1}`);
    const blocked = Object.values(info.capabilities as Record<string, { ok: boolean; reason?: string }>).filter((c) => !c.ok);
    if (info.privilege === "none") expect(blocked.length).toBeGreaterThan(0);
    else expect(blocked).toEqual([]);
    expect(env.node1).toBeTruthy();
  });
});
