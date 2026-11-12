// The cluster CPU baseline: a new VM gets the best model every node can run, it live-migrates between
// nodes with a green CPU pre-flight row, and Advanced switches it to host-model and back.
import { cleanVMs, expect, openVM, test } from "../lib/fixtures";
import { env, P } from "../lib/env";
import { dialog, field, pick, vmAction } from "../lib/ui";

const NAME = `${P}cpu1`;
const NAME2 = `${P}cpu2`;

interface Baseline {
  model?: string;
  features: { policy: string; name: string }[];
  nodes: { id: number; name: string; hostModel: string }[];
  skipped: { id: number; name: string; reason: string }[];
  computed: string;
  error?: string;
}

/** The required features of the <cpu> element in a domain definition. */
function required(xml: string): string[] {
  const cpu = /<cpu\b[\s\S]*?<\/cpu>/.exec(xml)?.[0] ?? "";
  return [...cpu.matchAll(/<feature policy='require' name='([^']+)'\/>/g)].map((m) => m[1]).sort();
}

// The VM's XML carries exactly the baseline's required features, also while it
// runs (the baseline leaves out what QEMU would take away, such as pdcm).
function expectBaseline(xml: string, base: Baseline) {
  expect(required(xml)).toEqual(base.features.filter((f) => f.policy === "require").map((f) => f.name).sort());
  expect(base.features.map((f) => f.name)).not.toContain("pdcm");
}

test.describe.serial("cluster CPU baseline", () => {
  let vm: { node: number; uuid: string; name: string };
  let base: Baseline;

  test.afterAll(async ({ api }) => cleanVMs(api, NAME, NAME2));

  test("the API reports the baseline, the nodes it covers and when it was computed", async ({ api, n1, n2 }) => {
    base = await api.get<Baseline>("/cluster/cpu-baseline");
    expect(base.error).toBeUndefined();
    expect(base.model).toBeTruthy();
    expect(base.features.length).toBeGreaterThan(5);
    const ids = base.nodes.map((n) => n.id);
    expect(ids).toContain(n1);
    expect(ids).toContain(n2);
    for (const n of base.nodes) expect(n.hostModel).toBeTruthy();
    expect(Array.isArray(base.skipped)).toBe(true);
    expect(Date.parse(base.computed)).toBeGreaterThan(Date.now() - 24 * 3600_000);
  });

  test("a new VM gets the baseline as a custom model, recorded as cluster baseline", async ({ page, api, n1 }) => {
    await page.goto("/datacenter/vms");
    await page.getByRole("button", { name: "New VM" }).click();
    const dlg = dialog(page, "New virtual machine");
    await field(dlg, "Name").fill(NAME);
    await pick(dlg, field(dlg, "Node"), env.node1);
    await field(dlg, "vCPUs").fill("1");
    await field(dlg, "Memory (MiB)").fill("128");
    await dlg.getByRole("button", { name: "Disk image" }).click();
    await pick(dlg, "Base image", new RegExp("^" + env.cirros.replace(/\./g, "\\.")));
    await field(dlg, "Disk size (GiB)").fill("1");
    await dlg.getByRole("button", { name: "Advanced" }).click();
    await expect(field(dlg, "CPU type")).toContainText(`Cluster baseline (${base.model})`);
    await dlg.getByRole("button", { name: "Create" }).click();
    await expect(dlg).toBeHidden();
    await expect(page).toHaveURL(/\/vm\/\d+\/[0-9a-f-]+\?tab=console/);
    const got = await api.vmByName(NAME);
    vm = { node: got.node, uuid: got.uuid, name: NAME };
    expect(vm.node).toBe(n1);

    const t = await api.get(`/nodes/${vm.node}/vms/${vm.uuid}/tuning`);
    expect(t.saved.cpuMode).toBe("custom");
    expect(t.saved.cpuModel).toBe(base.model);
    expect(t.cpuPolicy).toMatchObject({ policy: "cluster-baseline", model: base.model, relation: "same" });
    const d = await api.vmDetail(vm.node, vm.uuid);
    expect(d.cpu).toMatchObject({ policy: "cluster-baseline", model: base.model, relation: "same" });
    expectBaseline(d.xml, base);
    expect(d.state).toBe("running");
    // the overview says where the CPU came from
    await openVM(page, vm);
    await expect(page.locator("section.card").filter({ has: page.getByRole("heading", { name: "Hardware", exact: true }) })).toContainText(`Cluster baseline (${base.model})`);
  });

  test("it live-migrates n1 to n2 with a green CPU row", async ({ page, api, n2 }) => {
    test.setTimeout(360_000);
    await openVM(page, vm);
    await vmAction(page, "Migrate", "Migrate to another node");
    const dlg = dialog(page, `Migrate ${NAME}`);
    await pick(dlg, "Target node", env.node2);
    const cpu = dlg.locator("ul.checklist li").filter({ hasText: "CPU" }).filter({ hasText: "compatible" });
    await expect(cpu).toBeVisible({ timeout: 60_000 });
    await expect(cpu.getByRole("img", { name: "ok" })).toBeVisible();
    await expect(dlg.getByRole("img", { name: "failed" })).toHaveCount(0);
    await dlg.getByRole("button", { name: "Migrate", exact: true }).click();
    await expect(dlg).toContainText("Done.", { timeout: 240_000 });
    await expect(page).toHaveURL(new RegExp(`/vm/${n2}/${vm.uuid}`), { timeout: 30_000 });
    const d = await api.vmDetail(n2, vm.uuid);
    expect(d.state).toBe("running");
    expect(d.cpu?.policy).toBe("cluster-baseline"); // the marker moved with the definition
  });

  test("Advanced switches the VM to host-model and back to the baseline", async ({ page, api, n2 }) => {
    await openVM(page, { ...vm, node: n2 }, "hardware");
    const open = () => page.getByRole("button", { name: /^Advanced/ }).first().click();
    await open();
    await expect(page.getByRole("combobox", { name: "CPU mode", exact: true })).toContainText(`Cluster baseline (${base.model})`);
    await pick(page, "CPU mode", "host-model");
    await page.getByRole("button", { name: "Save CPU" }).click();
    await api.waitFor("host-model saved", async () => (await api.get(`/nodes/${n2}/vms/${vm.uuid}/tuning`)).saved.cpuMode === "host-model");
    let t = await api.get(`/nodes/${n2}/vms/${vm.uuid}/tuning`);
    expect(t.cpuPolicy).toBeUndefined();
    expect((await api.vmDetail(n2, vm.uuid)).cpu).toBeUndefined();

    await expect(page.getByRole("combobox", { name: "CPU mode", exact: true })).toContainText("host-model");
    await pick(page, "CPU mode", /^Cluster baseline/);
    await page.getByRole("button", { name: "Save CPU" }).click();
    await api.waitFor("baseline saved", async () => (await api.get(`/nodes/${n2}/vms/${vm.uuid}/tuning`)).saved.cpuMode === "custom");
    t = await api.get(`/nodes/${n2}/vms/${vm.uuid}/tuning`);
    expect(t.saved.cpuModel).toBe(base.model);
    expect(t.cpuPolicy).toMatchObject({ policy: "cluster-baseline", model: base.model });
    expectBaseline((await api.vmDetail(n2, vm.uuid)).xml, base);
  });

  test("the create API takes host-model and host-passthrough, and refuses other values", async ({ api, n1 }) => {
    const body = { name: NAME2, vcpus: 1, memoryMiB: 128, firmware: "bios", pool: "default", diskGiB: 1, baseVolume: env.cirros, network: "default", start: false };
    await expect(api.post(`/nodes/${n1}/vms`, { ...body, cpu: "turbo" })).rejects.toThrow(/cpu must be/);
    const r = await api.post(`/nodes/${n1}/vms`, { ...body, cpu: "host-model" });
    let t = await api.get(`/nodes/${n1}/vms/${r.uuid}/tuning`);
    expect(t.saved.cpuMode).toBe("host-model");
    expect(t.cpuPolicy).toBeUndefined();
    await api.del(`/nodes/${n1}/vms/${r.uuid}?disks=1`);
    const r2 = await api.post(`/nodes/${n1}/vms`, { ...body, cpu: "host-passthrough" });
    t = await api.get(`/nodes/${n1}/vms/${r2.uuid}/tuning`);
    expect(t.saved.cpuMode).toBe("host-passthrough");
    await api.del(`/nodes/${n1}/vms/${r2.uuid}?disks=1`);
  });
});
