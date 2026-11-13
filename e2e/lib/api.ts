// Thin API client used by the tests as ground truth and for cleanup.
import { APIRequestContext, request } from "@playwright/test";
import { env, P } from "./env";

export class Api {
  constructor(public ctx: APIRequestContext) {}

  static async login(user = env.user, pass = env.pass): Promise<Api> {
    const ctx = await request.newContext({ baseURL: env.base });
    const res = await ctx.post("/api/login", { data: { username: user, password: pass } });
    if (!res.ok()) throw new Error(`login as ${user} failed: ${res.status()} ${await res.text()}`);
    return new Api(ctx);
  }

  async call<T = any>(method: string, path: string, data?: unknown, okStatuses: number[] = []): Promise<T> {
    const res = await this.ctx.fetch("/api" + path, { method, data });
    if (!res.ok() && !okStatuses.includes(res.status())) throw new Error(`${method} ${path}: ${res.status()} ${await res.text()}`);
    const t = await res.text();
    try {
      return JSON.parse(t);
    } catch {
      return t as any;
    }
  }
  get = <T = any>(p: string) => this.call<T>("GET", p);
  post = <T = any>(p: string, d: unknown = {}) => this.call<T>("POST", p, d);
  put = <T = any>(p: string, d: unknown = {}) => this.call<T>("PUT", p, d);
  del = <T = any>(p: string) => this.call<T>("DELETE", p);

  async nodes(): Promise<{ id: number; hostname?: string; addr: string; state: string; maintenance?: boolean; user?: string }[]> {
    return (await this.get("/nodes")).nodes;
  }
  async nodeId(name: string): Promise<number> {
    const n = (await this.nodes()).find((x) => x.hostname === name || x.addr === name);
    if (!n) throw new Error(`node ${name} is not known to the controller`);
    return n.id;
  }
  async vms(): Promise<any[]> {
    return this.get("/vms");
  }
  async vmByName(name: string): Promise<any | undefined> {
    return (await this.vms()).find((v) => v.name === name);
  }
  async vmDetail(node: number, uuid: string) {
    return this.get(`/nodes/${node}/vms/${uuid}`);
  }

  async waitFor<T>(what: string, fn: () => Promise<T | undefined | false>, timeoutMs = 60_000, everyMs = 1500): Promise<T> {
    const end = Date.now() + timeoutMs;
    let last: unknown;
    while (Date.now() < end) {
      try {
        const v = await fn();
        if (v) return v as T;
      } catch (e) {
        last = e;
      }
      await new Promise((r) => setTimeout(r, everyMs));
    }
    throw new Error(`timed out waiting for ${what}${last ? ": " + last : ""}`);
  }

  async dispose() {
    await this.ctx.dispose();
  }
}

/** Removes a VM with its disks whatever state it is in. Never throws. */
export async function removeVM(api: Api, vm: { node: number; uuid: string; name: string; state: string }) {
  const base = `/nodes/${vm.node}/vms/${vm.uuid}`;
  try {
    if (vm.state !== "shutoff") {
      await api.post(`${base}/action`, { action: "poweroff" });
      await api.waitFor("vm off", async () => (await api.vmDetail(vm.node, vm.uuid)).state === "shutoff", 30_000, 1000);
    }
  } catch {
    /* try deleting anyway */
  }
  try {
    const d = await api.vmDetail(vm.node, vm.uuid);
    for (const s of d.snapshots ?? []) await api.del(`${base}/snapshots/${encodeURIComponent(s)}`).catch(() => {});
    if (d.template) await api.put(`${base}/template`, { template: false }).catch(() => {});
    await api.del(`${base}?disks=1`);
  } catch (e) {
    console.log(`  could not delete VM ${vm.name}: ${e}`);
  }
}

/** Deletes every object whose name carries the prefix. Returns what it found. */
export async function sweep(api: Api, log = false): Promise<string[]> {
  const found: string[] = [];
  const note = (s: string) => {
    found.push(s);
    if (log) console.log("  leftover: " + s);
  };
  // Several passes: a template cannot go while a linked clone still reads its disks.
  const seen = new Set<string>();
  for (let pass = 0; pass < 3; pass++) {
    const mine = (await api.vms()).filter((v) => v.name.startsWith(P));
    if (mine.length === 0) break;
    for (const vm of mine) {
      if (!seen.has(vm.uuid)) note(`vm ${vm.name} on ${vm.nodeName}`);
      seen.add(vm.uuid);
      await removeVM(api, vm);
    }
  }
  // Backups of prefixed VMs (or restored copies), wherever they were put.
  const backups: any[] = await api.get("/backups").catch(() => []);
  for (const b of backups) {
    if (!(b.manifest?.vm ?? b.dir).startsWith(P)) continue;
    note(`backup ${b.dir} in ${b.nodeName}:${b.pool}`);
    await api.del(`/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}`).catch((e) => console.log("  " + e));
  }
  for (const n of await api.nodes()) {
    if (n.state !== "online") continue;
    const pools: any[] = await api.get(`/nodes/${n.id}/pools`).catch(() => []);
    const volPath = (pool: string, vol: string) => `/nodes/${n.id}/pools/${encodeURIComponent(pool)}/volumes/${encodeURIComponent(vol)}`;
    for (const p of pools) {
      if (p.state !== "running" || p.name.startsWith(P)) continue;
      const vols: any[] = await api.get(`/nodes/${n.id}/pools/${encodeURIComponent(p.name)}/volumes`).catch(() => []);
      for (const v of vols.filter((x) => x.name.startsWith(P))) {
        note(`volume ${p.name}/${v.name} on ${n.hostname}`);
        await api.del(volPath(p.name, v.name)).catch((e) => console.log("  " + e));
      }
    }
    for (const p of pools.filter((x) => x.name.startsWith(P))) {
      note(`pool ${p.name} on ${n.hostname}`);
      if (p.state === "running") {
        const vols: any[] = await api.get(`/nodes/${n.id}/pools/${encodeURIComponent(p.name)}/volumes`).catch(() => []);
        for (const v of vols) await api.del(volPath(p.name, v.name)).catch(() => {});
      }
      await api.post(`/nodes/${n.id}/pools/${encodeURIComponent(p.name)}/action`, { action: "remove" }).catch((e) => console.log("  " + e));
    }
    const nets: any[] = await api.get(`/nodes/${n.id}/networks`).catch(() => []);
    for (const net of nets.filter((x) => x.name.startsWith(P))) {
      note(`network ${net.name} on ${n.hostname}`);
      await api.post(`/nodes/${n.id}/networks/${net.name}/action`, { action: "stop" }).catch(() => {}); // an active network is not deleted
      await api.post(`/nodes/${n.id}/networks/${net.name}/action`, { action: "delete" }).catch((e) => console.log("  " + e));
    }
  }
  for (const u of await api.get("/users").catch(() => [])) {
    if (u.username.startsWith(P)) {
      note(`user ${u.username}`);
      await api.del(`/users/${u.id}`).catch((e) => console.log("  " + e));
    }
  }
  for (const t of await api.get("/account/tokens").catch(() => [])) {
    if (t.name.startsWith(P)) {
      note(`token ${t.name}`);
      await api.del(`/account/tokens/${t.id}`).catch(() => {});
    }
  }
  return found;
}
