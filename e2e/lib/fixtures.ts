import { expect, Page, test as base } from "@playwright/test";
import { Api, removeVM } from "./api";
import { env, P } from "./env";

type Worker = { api: Api; n1: number; n2: number };

export const test = base.extend<{ unused: void; noBrowserDialogs: void }, Worker>({
  // The app must never open a browser pop-up (alert, confirm, prompt): every question is an in-app dialog.
  noBrowserDialogs: [
    async ({ page }, use) => {
      const seen: string[] = [];
      page.on("dialog", (d) => {
        seen.push(`${d.type()}: ${d.message().slice(0, 80)}`);
        d.dismiss().catch(() => {});
      });
      await use();
      expect(seen, "browser pop-ups opened by the app").toEqual([]);
    },
    { auto: true },
  ],
  api: [
    async ({}, use) => {
      const api = await Api.login();
      await use(api);
      await api.dispose();
    },
    { scope: "worker" },
  ],
  n1: [async ({ api }, use) => use(await api.nodeId(env.node1)), { scope: "worker" }],
  n2: [async ({ api }, use) => use(await api.nodeId(env.node2)), { scope: "worker" }],
});
export { expect };

/** Creates a small VM from the cirros image through the API (the UI path is covered in vm-create.spec). */
export async function createCirros(api: Api, node: number, name: string, opts: { start?: boolean; memoryMiB?: number; network?: string } = {}) {
  const r = await api.post(`/nodes/${node}/vms`, {
    name,
    vcpus: 1,
    memoryMiB: opts.memoryMiB ?? 128,
    firmware: "bios",
    pool: "default",
    diskGiB: 1,
    baseVolume: env.cirros,
    network: opts.network ?? "default",
    start: opts.start ?? false,
  });
  return { node, uuid: r.uuid as string, name };
}

/** Deletes the named VMs (and their disks) if they exist. For afterAll hooks. */
export async function cleanVMs(api: Api, ...names: string[]) {
  const all = await api.vms();
  for (const n of names) {
    for (const vm of all.filter((v) => v.name === n)) await removeVM(api, vm);
  }
}

export const vmUrl = (node: number, uuid: string, tab?: string) => `/vm/${node}/${uuid}${tab ? "?tab=" + tab : ""}`;

/** Opens a VM detail page and waits for its header. */
export async function openVM(page: Page, vm: { node: number; uuid: string; name: string }, tab?: string) {
  await page.goto(vmUrl(vm.node, vm.uuid, tab));
  await expect(page.getByRole("heading", { level: 1, name: vm.name })).toBeVisible();
}

export async function tab(page: Page, name: string) {
  await page.locator(".tabs").getByRole("button", { name, exact: true }).click();
}

export async function stateOf(api: Api, vm: { node: number; uuid: string }) {
  return (await api.vmDetail(vm.node, vm.uuid)).state as string;
}

export { P };
