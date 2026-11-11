// Storage page: directory pool, volumes (create, upload, download from URL, import, clone, resize, delete), remove pool.
import { Page } from "@playwright/test";
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, expectBadge, field, row, confirm } from "../lib/ui";

const POOL = `${P}pool`;
const PATH = `/var/lib/libvirt/${P}pool`;
const CIRROS_URL = "https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img";

/** Answer the next window.prompt/confirm and remember what it asked. */
function answerNext(page: Page, answer?: string) {
  const asked: { type: string; message: string; def: string }[] = [];
  page.once("dialog", (d) => {
    asked.push({ type: d.type(), message: d.message(), def: d.defaultValue() });
    d.type() === "prompt" ? d.accept(answer ?? d.defaultValue()) : d.accept();
  });
  return asked;
}

test.describe.serial("storage", () => {
  const vols = (api: any, n: number) => api.get(`/nodes/${n}/pools/${POOL}/volumes`) as Promise<any[]>;
  const vol = (api: any, n: number, name: string) => vols(api, n).then((l) => l.find((v) => v.name === name));

  test.afterAll(async ({ api, n1 }) => {
    const l: any[] = await api.get(`/nodes/${n1}/pools`);
    if (l.some((p) => p.name === POOL)) {
      for (const v of await vols(api, n1).catch(() => [])) await api.del(`/nodes/${n1}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
      await api.post(`/nodes/${n1}/pools/${POOL}/action`, { action: "remove" }).catch(() => {});
    }
  });

  test("create a directory pool", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await expect(page.getByRole("heading", { name: "Storage" })).toBeVisible();
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await page.getByRole("button", { name: "New pool" }).click();
    const dlg = dialog(page, "New storage pool");
    await expect(field(dlg, "Name")).toBeFocused();
    await field(dlg, "Name").fill(POOL);
    await field(dlg, "Directory on the node").fill(PATH);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, POOL);
    await expect(r).toBeVisible();
    await expectBadge(r, "running");
    await expect(r).toContainText(PATH);
    const p = (await api.get(`/nodes/${n1}/pools`)).find((x: any) => x.name === POOL);
    expect(p).toMatchObject({ type: "dir", state: "running", target: PATH });
  });

  test("a pool with a duplicate name shows libvirt's error and keeps the dialog open", async ({ page }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await page.getByRole("button", { name: "New pool" }).click();
    const dlg = dialog(page, "New storage pool");
    await field(dlg, "Name").fill(POOL);
    await field(dlg, "Directory on the node").fill(PATH + "-b");
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg.locator(".error")).toBeVisible();
    await expect(dlg).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dlg).toBeHidden();
  });

  test("create a volume", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, POOL).click();
    await expect(page.getByRole("heading", { name: `Volumes in ${POOL}` })).toBeVisible();
    await expect(page.getByText("This pool is empty.")).toBeVisible();
    await page.getByRole("button", { name: "New volume" }).click();
    const dlg = dialog(page, `New volume in ${POOL}`);
    await field(dlg, "Name").fill(`${P}vol1`);
    await field(dlg, "Size (GiB)").fill("1");
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    const r = row(page, `${P}vol1.qcow2`);
    await expect(r).toBeVisible();
    await expect(r).toContainText("1.0 GiB");
    expect(await vol(api, n1, `${P}vol1.qcow2`)).toMatchObject({ format: "qcow2", capacity: 2 ** 30 });
  });

  test("upload a file and import it as qcow2", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, POOL).click();
    await page.locator('input[type="file"]').setInputFiles({ name: `${P}raw.img`, mimeType: "application/octet-stream", buffer: Buffer.alloc(1024 * 1024) });
    const r = row(page, `${P}raw.img`);
    await expect(r).toBeVisible();
    expect((await vol(api, n1, `${P}raw.img`)).format).toBe("raw");

    await r.getByRole("button", { name: "Import" }).click();
    const dlg = dialog(page, "Import a disk image or OVA");
    await expect(field(dlg, "Source volume")).toHaveValue(`${P}raw.img`);
    await dlg.getByRole("button", { name: "Inspect" }).click();
    await expect(dlg).toContainText("Detected format raw");
    await expect(field(dlg, "New volume name")).toHaveValue(`${P}raw-imported`);
    await dlg.getByRole("button", { name: "Import", exact: true }).click();
    const fin = dialog(page, "Import finished");
    await expect(fin).toContainText(`${P}raw-imported.qcow2`);
    await fin.getByRole("button", { name: "Close", exact: true }).click();
    await expect(row(page, `${P}raw-imported.qcow2`)).toBeVisible();
    expect(await vol(api, n1, `${P}raw-imported.qcow2`)).toMatchObject({ format: "qcow2" });
  });

  test("download from a URL", async ({ page, api, n1 }) => {
    test.setTimeout(240_000);
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, POOL).click();
    await page.getByRole("button", { name: "Download from URL" }).click();
    const dlg = dialog(page, `Download into ${POOL}`);
    await field(dlg, "URL").fill(CIRROS_URL);
    await expect(field(dlg, "Volume name")).toHaveValue("cirros-0.6.3-x86_64-disk.img");
    await field(dlg, "Volume name").fill(`${P}cirros-dl.img`);
    await dlg.getByRole("button", { name: "Download" }).click();
    await expect(dlg).toBeHidden({ timeout: 200_000 });
    const r = row(page, `${P}cirros-dl.img`);
    await expect(r).toBeVisible();
    const v = await vol(api, n1, `${P}cirros-dl.img`);
    expect(v.allocation).toBeGreaterThan(10 * 2 ** 20);
  });

  test("clone, resize and delete volumes", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, POOL).click();

    await row(page, `${P}vol1.qcow2`).getByRole("button", { name: "Clone" }).click();
    const cd = page.getByRole("dialog", { name: /^Clone / });
    await field(cd, "Name of the copy").fill(`${P}vol1-copy.qcow2`);
    await cd.getByRole("button", { name: "Clone", exact: true }).click();
    await expect(row(page, `${P}vol1-copy.qcow2`)).toBeVisible();
    expect(await vol(api, n1, `${P}vol1-copy.qcow2`)).toBeTruthy();

    await row(page, `${P}vol1-copy.qcow2`).getByRole("button", { name: "Resize" }).click();
    const rd = page.getByRole("dialog", { name: /^Resize / });
    await field(rd, "New size (GiB)").fill("2");
    await rd.getByRole("button", { name: "Resize", exact: true }).click();
    await expect(row(page, `${P}vol1-copy.qcow2`)).toContainText("2.0 GiB");
    expect((await vol(api, n1, `${P}vol1-copy.qcow2`)).capacity).toBe(2 * 2 ** 30);

    await row(page, `${P}vol1-copy.qcow2`).getByRole("button", { name: "Delete" }).click();
    await expect(page.getByRole("dialog").last()).toContainText(`${P}vol1-copy.qcow2`);
    await confirm(page, /^Delete volume/);
    await expect(row(page, `${P}vol1-copy.qcow2`)).toHaveCount(0);
    expect(await vol(api, n1, `${P}vol1-copy.qcow2`)).toBeUndefined();
  });

  test("stop and start the pool, then remove it", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    const r = row(page, POOL);
    await r.getByRole("button", { name: "Stop" }).click();
    await confirm(page, /^Stop pool/);
    await expectBadge(r, "inactive");
    await r.getByRole("button", { name: "Start" }).click();
    await expectBadge(r, "running");
    for (const v of await vols(api, n1)) await api.del(`/nodes/${n1}/pools/${POOL}/volumes/${encodeURIComponent(v.name)}`);
    await r.getByRole("button", { name: "Remove" }).click();
    await confirm(page, /^Remove pool/);
    await expect(r).toHaveCount(0);
    expect((await api.get(`/nodes/${n1}/pools`)).some((x: any) => x.name === POOL)).toBe(false);
  });
});
