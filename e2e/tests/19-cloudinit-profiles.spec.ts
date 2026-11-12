// Cloud-init profiles: page CRUD, no password leaves the server, the create dialog inherits a default
// profile, a VM made from it gets a seed disk, deleting the profile leaves the VM alone, viewers are refused.
import { cleanVMs, expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { autoDialogs, dialog, field, row, pick, pickPool } from "../lib/ui";

const PROF = `${P}prof`;
const VM = `${P}cpvm1`;
const KEY = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKtnt72pPWXOyE7vKx9bVhHeiDsdpEoi3OD1lfAd3vmi e2e@test";

test.describe.serial("cloud-init profiles", () => {
  test.afterAll(async ({ api }) => {
    await cleanVMs(api, VM);
    for (const p of (await api.get("/cloudinit-profiles")).profiles) if (p.name.startsWith(P)) await api.del(`/cloudinit-profiles/${p.id}`).catch(() => {});
  });

  test("create, edit and list a profile through the page", async ({ page, api }) => {
    autoDialogs(page);
    await page.goto("/account");
    await page.getByRole("link", { name: "Manage profiles" }).click();
    await expect(page.getByRole("heading", { name: "Cloud-init profiles" })).toBeVisible();
    await page.getByRole("button", { name: "New profile" }).click();
    const dlg = dialog(page, "New cloud-init profile");
    await field(dlg, "Name").fill(PROF);
    await field(dlg, "Default user").fill("cpuser");
    await field(dlg, "Password").fill("e2e-profile-pass-1");
    await field(dlg, "Packages").fill("qemu-guest-agent jq");
    await field(dlg, "SSH public keys").fill(KEY);
    await dlg.getByLabel("Default profile").check();
    await dlg.getByRole("button", { name: "Save" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, PROF);
    await expect(r).toContainText("cpuser");
    await expect(r).toContainText("default");

    const all = (await api.get("/cloudinit-profiles")).profiles.find((p: any) => p.name === PROF);
    expect(all.hasPassword).toBe(true);
    expect(JSON.stringify(all)).not.toMatch(/e2e-profile-pass|\$6\$/);

    // Editing without typing a password keeps it.
    await r.getByRole("button", { name: "Edit" }).click();
    const ed = dialog(page, /^Edit /);
    await field(ed, "Description").fill("edited");
    await ed.getByRole("button", { name: "Save" }).click();
    await expect(ed).toBeHidden();
    const again = (await api.get("/cloudinit-profiles")).profiles.find((p: any) => p.name === PROF);
    expect(again.description).toBe("edited");
    expect(again.hasPassword).toBe(true);
  });

  test("server validation", async ({ api }) => {
    const bad = (body: object) => api.call("POST", "/cloudinit-profiles", body, [400]);
    expect((await bad({ name: PROF })).error).toMatch(/already exists/);
    expect((await bad({ name: `${P}x`, user: "u", sshKeys: ["ssh-ed25519 nope"] })).error).toMatch(/SSH public key/);
    expect((await bad({ name: `${P}x`, rawUserData: "plain: text" })).error).toMatch(/#cloud-config/);
  });

  test("the create dialog preselects the default profile and a VM made from it gets a seed", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await field(dlg, "Name").fill(VM);
    await pick(dlg, field(dlg, "Node"), env.node1);
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    // the pool select is hidden when the node has a single running pool
    if (await dlg.getByRole("combobox", { name: "Storage pool" }).count()) await pickPool(dlg, "Storage pool", "default");
    await pick(dlg, "Base image", new RegExp("^" + env.cirros.replace(/\./g, "\\.")));
    await field(dlg, "Disk size (GiB)").fill("1");
    await expect(dlg.locator("summary", { hasText: "Cloud-init" })).toContainText(PROF);
    await expect(field(dlg, "Profile")).toContainText(PROF);
    await expect(field(dlg, "User")).toHaveAttribute("placeholder", "cpuser");
    await expect(field(dlg, "Password")).toHaveAttribute("placeholder", "(from the profile)");
    await dlg.getByLabel("Start after creating").uncheck();
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden({ timeout: 120_000 });
    const got = await api.vmByName(VM);
    const d = await api.vmDetail(got.node, got.uuid);
    expect(d.disks.some((k: any) => k.device === "cdrom" && /seed/.test(k.source ?? ""))).toBe(true);

    // Deleting the profile does not touch the VM.
    const prof = (await api.get("/cloudinit-profiles")).profiles.find((p: any) => p.name === PROF);
    await api.del(`/cloudinit-profiles/${prof.id}`);
    expect((await api.vmDetail(got.node, got.uuid)).name).toBe(VM);
    const r = await api.call("POST", `/nodes/${n1}/vms`, { name: `${P}cpvm2`, vcpus: 1, memoryMiB: 128, pool: "default", diskGiB: 1, baseVolume: env.cirros, cloudInit: { profile: prof.id } }, [400]);
    expect(r.error).toMatch(/does not exist/);
  });

  test("a viewer cannot read profiles", async ({ api }) => {
    const name = `${P}cpviewer`;
    const u = await api.post("/users", { username: name, password: "e2e-viewer-pass-12", role: "viewer" });
    try {
      const { Api } = await import("../lib/api");
      const v = await Api.login(name, "e2e-viewer-pass-12");
      await v.call("GET", "/cloudinit-profiles", undefined, [403]);
      const res = await v.ctx.fetch("/api/cloudinit-profiles");
      expect(res.status()).toBe(403);
    } finally {
      await api.del(`/users/${u.id}`);
    }
  });
});
