// Image catalogue: list, one-click download of CirrOS (21 MB, checksum verified on the node), the
// "already in this pool" mark, "Create VM from this image", name clash, cancelling a download, and
// the hint in the New VM dialog. Wrong-checksum handling needs the test entry `ic-bad` from an
// <data>/images.json override (see the notes in internal/catalog/catalog.go); without it that test skips.
import { expect, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, row } from "../lib/ui";

const POOL = `${P}icpool`;
const EMPTY = `${P}icempty`;
const CIRROS_VOL = "cirros-0.6.3-x86_64-disk.img";
const BIG_VOL = `${P}cancel.qcow2`;

test.describe.serial("image catalogue", () => {
  const poolBase = (n: number, p = POOL) => `/nodes/${n}/pools/${p}`;
  const vols = (api: any, n: number, p = POOL) => api.get(`${poolBase(n, p)}/volumes`) as Promise<any[]>;
  const imgs = (api: any, n: number) => api.get(`${poolBase(n)}/images`);

  test.afterAll(async ({ api, n1 }) => {
    await api.post(`${poolBase(n1)}/images/cancel`, { name: BIG_VOL }).catch(() => {});
    const l: any[] = await api.get(`/nodes/${n1}/pools`);
    for (const p of [POOL, EMPTY]) {
      if (!l.some((x) => x.name === p)) continue;
      for (const v of await vols(api, n1, p).catch(() => [])) await api.del(`${poolBase(n1, p)}/volumes/${encodeURIComponent(v.name)}`).catch(() => {});
      await api.post(`${poolBase(n1, p)}/action`, { action: "remove" }).catch(() => {});
    }
  });

  test("the catalogue lists the built-in images", async ({ api, n1 }) => {
    const { images } = await api.get("/images");
    const ids = images.map((i: any) => i.id);
    for (const id of ["debian-12", "debian-13", "ubuntu-22.04", "ubuntu-24.04", "rocky-9", "rocky-10", "alma-9", "alma-10", "fedora-45", "cirros-0.6.3"]) expect(ids).toContain(id);
    for (const i of images) expect(i.url).toMatch(/^https:\/\//);
    // pools for the tests
    for (const [name, dir] of [
      [POOL, `/var/lib/libvirt/${POOL}`],
      [EMPTY, `/var/lib/libvirt/${EMPTY}`],
    ]) await api.post(`/nodes/${n1}/pools`, { type: "dir", name, target: dir });
    const r = await imgs(api, n1);
    expect(r.downloadable).toBe(true);
    expect(r.images.some((i: any) => i.present || i.downloading)).toBe(false);
  });

  test("download CirrOS from the dialog, then it is marked as present", async ({ page, api, n1 }) => {
    await page.goto("/storage");
    await page.getByRole("combobox").first().selectOption({ label: env.node1 });
    await row(page, POOL).click();
    await page.getByRole("button", { name: "Image catalogue" }).click();
    const dlg = dialog(page, "Image catalogue");
    const item = dlg.locator("li.catitem", { hasText: "CirrOS" });
    await expect(item).toBeVisible();
    await expect(dlg.locator("li.catitem", { hasText: "Debian 13" })).toContainText("cloud-init");
    await expect(item).toContainText("no cloud-init");
    await item.getByRole("button", { name: "Download" }).click();
    await expect(item.locator(".catok")).toHaveText("already in this pool", { timeout: 120_000 });
    await expect(item.getByRole("link", { name: "Create VM from this image" })).toBeVisible();
    const v = (await vols(api, n1)).find((x) => x.name === CIRROS_VOL);
    expect(v).toBeTruthy();
    expect(v.allocation).toBe(21692416);
    const task = (await api.get("/tasks")).find((t: any) => t.kind === "image" && t.target === `${POOL}/${CIRROS_VOL}`);
    expect(task.status).toBe("ok");
    // the pool directory holds no leftovers
    expect((await vols(api, n1)).map((x) => x.name)).toEqual([CIRROS_VOL]);

    // "Create VM from this image" opens the New VM dialog with the image preselected
    await item.getByRole("link", { name: "Create VM from this image" }).click();
    const nvm = dialog(page, "New virtual machine");
    await expect(nvm).toBeVisible();
    await expect(field(nvm, "Base image")).toHaveValue(CIRROS_VOL);
    await expect(field(nvm, "Storage pool")).toHaveValue(POOL);
    await page.keyboard.press("Escape");
  });

  test("a second download of the same image is refused", async ({ api, n1 }) => {
    const res = await api.ctx.fetch(`/api${poolBase(n1)}/images`, { method: "POST", data: { id: "cirros-0.6.3" } });
    expect(res.status()).toBe(400);
    expect(await res.text()).toContain("already exists");
    const bad = await api.ctx.fetch(`/api${poolBase(n1)}/images`, { method: "POST", data: { id: "cirros-0.6.3", name: "../x" } });
    expect(bad.status()).toBe(400);
    const unknown = await api.ctx.fetch(`/api${poolBase(n1)}/images`, { method: "POST", data: { id: "no-such-image" } });
    expect(unknown.status()).toBe(400);
  });

  test("cancelling a download removes the partial file", async ({ api, n1 }) => {
    await api.post(`${poolBase(n1)}/images`, { id: "debian-13", name: BIG_VOL });
    const started = await api.waitFor("download running with progress", async () => (await api.get("/tasks")).find((t: any) => t.target === `${POOL}/${BIG_VOL}` && t.status === "running" && t.progress > 0.01), 90_000, 1500);
    expect((await imgs(api, n1)).running).toContain(BIG_VOL);
    await api.post(`${poolBase(n1)}/images/cancel`, { name: BIG_VOL });
    const t = await api.waitFor("task to end", async () => (await api.get("/tasks")).find((x: any) => x.id === started.id && x.status !== "running"), 30_000, 1000); // by id: earlier runs leave tasks with the same target
    expect(t.status).toBe("failed");
    expect(t.error).toContain("cancelled");
    expect((await imgs(api, n1)).running).toEqual([]);
    expect((await vols(api, n1)).map((x) => x.name)).toEqual([CIRROS_VOL]);
  });

  test("a wrong checksum fails cleanly and leaves no file", async ({ api, n1 }) => {
    const { images } = await api.get("/images");
    test.skip(!images.some((i: any) => i.id === "ic-bad"), "needs the ic-bad test entry in <data>/images.json");
    await api.post(`${poolBase(n1)}/images`, { id: "ic-bad" });
    const t = await api.waitFor("task to end", async () => (await api.get("/tasks")).find((x: any) => x.target === `${POOL}/ic-bad.img` && x.status !== "running"), 120_000, 2000);
    expect(t.status).toBe("failed");
    expect(t.error).toContain("checksum mismatch");
    expect((await vols(api, n1)).map((x) => x.name)).toEqual([CIRROS_VOL]);
  });

  test("New VM dialog hints at the catalogue when the pool has no image", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await field(dlg, "Node").selectOption({ label: env.node1 });
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await field(dlg, "Storage pool").selectOption(EMPTY);
    const link = dlg.getByRole("link", { name: "Get a cloud image from the catalogue" });
    await expect(link).toBeVisible();
    await field(dlg, "Storage pool").selectOption(POOL);
    await expect(link).toBeHidden();
    await field(dlg, "Storage pool").selectOption(EMPTY);
    await link.click();
    await expect(page).toHaveURL(new RegExp(`/node/${n1}/storage`));
    await expect(dialog(page, "Image catalogue")).toBeVisible();
    await expect(dialog(page, "Image catalogue")).toContainText(EMPTY);
    await page.keyboard.press("Escape");
  });
});
