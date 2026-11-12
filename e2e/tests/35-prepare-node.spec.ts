// Prepare node, check only, against lab node 1. Read-only: the dialog signs in with an administrator
// key pasted into it, looks at the host and changes nothing. The key is a secret for this one run: after
// the form is submitted it must be nowhere on the page, in the task list or in the audit log.
//
// The administrator key is read from E2E_ADMIN_KEY_FILE (default: the lab's controller key, which is
// authorized for root on the lab nodes); the test is skipped when it cannot be read.
import { readFileSync } from "node:fs";
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";
import { dialog, field, pick } from "../lib/ui";

const KEY_FILE = process.env.E2E_ADMIN_KEY_FILE ?? "/root/virt-proj/lab/keys/controller";
let key = "";
try {
  key = readFileSync(KEY_FILE, "utf8").trim();
} catch {
  /* skipped below */
}

test.describe.serial("prepare node, check only", () => {
  test.skip(!key, `no administrator key at ${KEY_FILE}`);

  test("the dialog checks a node, shows the rows, and leaves no secret behind", async ({ page, api, n1 }) => {
    const node = (await api.nodes()).find((n) => n.id === n1) as any;
    const before = await api.get(`/nodes/${n1}`);
    const body = key.split("\n").filter((l) => !l.startsWith("-----"))[1] ?? key.slice(40, 80); // a line of the key itself
    expect(body.length).toBeGreaterThan(20);

    await page.goto("/nodes");
    await page.getByRole("button", { name: "Add node" }).click();
    await field(dialog(page, "Add node"), "Address").fill(node.addr);
    await field(dialog(page, "Add node"), "Jump host (optional)").fill(node.jump ?? "");
    await dialog(page, "Add node").getByRole("button", { name: "Prepare this node" }).click();

    const dlg = dialog(page, "Prepare node");
    await expect(dlg).toBeVisible();
    await expect(field(dlg, "Address")).toHaveValue(node.addr);
    await pick(dlg, "Credential", "Private key");
    await dlg.locator("textarea").fill(key);
    await expect(dlg.getByLabel("Check only, change nothing")).toBeChecked();
    await dlg.getByRole("button", { name: "Check", exact: true }).click();

    // the form is gone and the secret with it
    const res = dialog(page, "Check node");
    await expect(res).toBeVisible();
    await expect(res.locator("textarea, input[type=password]")).toHaveCount(0);

    const list = res.locator("ul.checklist");
    await expect(list).toContainText("Administrator access", { timeout: 60_000 });
    for (const t of ["Architecture", "Distribution", "CPU virtualisation", "KVM device", "IOMMU", "Free space", "Security module", "Time sync", "libvirt", "Other controller", "Existing VMs", "Service account"]) {
      await expect(list.locator("strong", { hasText: new RegExp(`^${t}$`) })).toBeVisible();
    }
    await expect(list).toContainText("Existing VMs are left alone");
    await expect(list).toContainText(/Debian GNU\/Linux 13|Rocky Linux 10/);
    await expect(res.getByRole("button", { name: "Add this node" })).toHaveCount(0); // a check does not offer to register
    await expect(res.getByRole("img", { name: "not run" })).toHaveCount(0);

    // no secret on the page, in the tasks or in the audit log
    await expect(page.locator("body")).not.toContainText(body);
    await expect(page.locator("body")).not.toContainText("PRIVATE KEY");
    const tasks = JSON.stringify(await api.get("/tasks"));
    expect(tasks).toContain("check node");
    expect(tasks).not.toContain(body);
    expect(tasks).not.toContain("PRIVATE KEY");
    const audit = JSON.stringify(await api.get("/audit"));
    expect(audit).toContain("prepare");
    expect(audit).not.toContain(body);
    expect(audit).not.toContain("PRIVATE KEY");
    const html = await page.content();
    expect(html).not.toContain(body);

    // nothing changed on the node or in the controller
    const after = await api.get(`/nodes/${n1}`);
    expect(after.privilege).toBe(before.privilege);
    expect((await api.nodes()).find((n) => n.id === n1)?.state).toBe("online");
    await page.keyboard.press("Escape");
  });

  test("the API refuses a run without a credential and a bad account name", async ({ api }) => {
    const miss = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", checkOnly: true } });
    expect(miss.status()).toBe(400);
    expect((await miss.json()).error).toContain("password or private key");
    const bad = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", password: "x", user: "Bad Name", checkOnly: true } });
    expect(bad.status()).toBe(400);
    expect((await bad.json()).error).toContain("not a valid account name");
    const root = await api.ctx.fetch("/api/prepare", { method: "POST", data: { addr: "192.0.2.1", password: "x", user: "root", checkOnly: true } });
    expect(root.status()).toBe(400);
  });

  test("a wrong address ends as an error row and never as a half-done run", async ({ page, api }) => {
    const r = await api.ctx.fetch("/api/prepare?async=1", { method: "POST", data: { addr: "127.0.0.1:1", adminUser: "root", password: "secret-for-this-test-only", checkOnly: true } });
    expect(r.status()).toBe(202);
    const id = (await r.json()).task as number;
    await expect.poll(async () => (await api.get(`/prepare/${id}`)).done, { timeout: 60_000 }).toBe(true);
    const job = await api.get(`/prepare/${id}`);
    expect(job.ok).toBe(false);
    expect(job.rows[0].status).toBe("error");
    expect(JSON.stringify(job)).not.toContain("secret-for-this-test-only");
    expect(JSON.stringify(await api.get("/tasks"))).not.toContain("secret-for-this-test-only");
    expect(JSON.stringify(await api.get("/audit"))).not.toContain("secret-for-this-test-only");
    void page;
    void env;
  });
});
