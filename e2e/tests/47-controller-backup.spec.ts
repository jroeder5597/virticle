// Settings > Controller backup: the controller's own backup. Safe on any lab: it only downloads an archive and looks inside it; nothing is
// restored (restore is a command on the controller's machine) and the daily copy setting is not touched.
import { execFileSync } from "child_process";
import { mkdtempSync, readFileSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import { Page } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { env } from "../lib/env";
import { field } from "../lib/ui";

const card = (page: Page) => page.getByRole("form", { name: "Download backup" });

async function download(page: Page, password: string, passphrase = "", again = passphrase) {
  const c = card(page);
  await field(c, "Your password").fill(password);
  if (passphrase) {
    await field(c, "Passphrase (optional)").fill(passphrase);
    await field(c, "Passphrase again").fill(again);
  }
  const wait = page.waitForEvent("download", { timeout: 20_000 });
  await c.getByRole("button", { name: "Download backup" }).click();
  return wait;
}

test.describe("controller backup", () => {
  test("the Controller backup page is in the Settings menu and says what a backup holds", async ({ page }) => {
    await page.goto("/");
    await page.locator(".topbar").getByRole("button", { name: "Settings" }).click();
    await page.getByRole("menuitem", { name: "Controller backup", exact: true }).click();
    await expect(page).toHaveURL(/\/controller-backup$/);
    await expect(page.getByRole("heading", { name: "Controller backup", exact: true })).toBeVisible();
    await expect(page.getByText("The file contains private keys.")).toBeVisible();
    await expect(page.getByText("It does not hold VMs, storage or networks")).toBeVisible();
    await expect(page.getByText("Restore is done on the command line")).toBeVisible();
    await expect(page.locator("code").filter({ hasText: "virticle restore" })).toBeVisible();
    await expect(page.getByRole("checkbox", { name: "Keep a daily backup on this server" })).toBeVisible();
    await expect(page.getByText("not against losing this machine")).toBeVisible();
  });

  test("a download needs the password again", async ({ page, api }) => {
    await page.goto("/controller-backup");
    // refused by the server, with no file
    const res = await api.ctx.fetch("/api/controller-backup/download", { method: "POST", data: { password: "not the password at all" } });
    expect(res.status()).toBe(403);
    expect(res.headers()["content-disposition"]).toBeUndefined();
    // in the page: an empty password does not even send
    const c = card(page);
    await c.getByRole("button", { name: "Download backup" }).click();
    await expect(field(c, "Your password")).toBeFocused();
    // a wrong password shows the error and the field is emptied
    await field(c, "Your password").fill("not the password at all");
    await c.getByRole("button", { name: "Download backup" }).click();
    await expect(c.getByText("the password is not right")).toBeVisible();
    await expect(field(c, "Your password")).toHaveValue("");
  });

  test("the downloaded archive opens and has a manifest", async ({ page }) => {
    test.skip(!env.pass, "needs E2E_PASS, the administrator's password");
    await page.goto("/controller-backup");
    const dl = await download(page, env.pass);
    expect(dl.suggestedFilename()).toMatch(/^virticle-backup-.+-\d{8}T\d{6}Z\.tar\.gz$/);
    const dir = mkdtempSync(join(tmpdir(), "e2e-ctlbackup-"));
    const file = join(dir, dl.suggestedFilename());
    await dl.saveAs(file);
    const manifest = JSON.parse(execFileSync("tar", ["-xzOf", file, "manifest.json"]).toString());
    expect(manifest.format).toBe(1);
    expect(manifest.schemaVersion).toBeGreaterThan(0);
    const paths = manifest.files.map((f: any) => f.path);
    expect(paths).toContain("virticle.db");
    expect(paths).toContain("id_ed25519");
    expect(paths).not.toContain("metrics.db");
    for (const f of manifest.files) expect(f.sha256).toMatch(/^[0-9a-f]{64}$/);
    // the checksum of one file in the archive is the one in the manifest
    const key = manifest.files.find((f: any) => f.path === "id_ed25519");
    const sum = execFileSync("sh", ["-c", `tar -xzOf "$1" data/id_ed25519 | sha256sum | cut -d' ' -f1`, "sh", file]).toString().trim();
    expect(sum).toBe(key.sha256);
    // the page remembers it
    await expect(page.getByText(/^Last backup: .*by download/)).toBeVisible();
  });

  test("a passphrase encrypts the download, and two different ones are refused", async ({ page }) => {
    test.skip(!env.pass, "needs E2E_PASS, the administrator's password");
    await page.goto("/controller-backup");
    const c = card(page);
    await field(c, "Your password").fill(env.pass);
    await field(c, "Passphrase (optional)").fill("a long passphrase one");
    await field(c, "Passphrase again").fill("a long passphrase two");
    await c.getByRole("button", { name: "Download backup" }).click();
    await expect(c.getByText("The two passphrases differ.")).toBeVisible();

    const dl = await download(page, env.pass, "a long passphrase one");
    expect(dl.suggestedFilename()).toMatch(/\.tar\.gz\.age$/);
    const dir = mkdtempSync(join(tmpdir(), "e2e-ctlbackup-"));
    const file = join(dir, dl.suggestedFilename());
    await dl.saveAs(file);
    expect(readFileSync(file).subarray(0, 19).toString()).toBe("age-encryption.org/");
    await expect(page.getByText(/by download.*encrypted/)).toBeVisible();
  });
});
