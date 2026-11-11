// Resource tree for the sidebar: datacenter > nodes > VMs, storage pools, networks and host NICs.
// Resource use: two requests on load (/nodes, /vms); pools, networks and NICs of a node are fetched
// only once its group is opened. No polling: the event stream triggers debounced refetches, only
// for what changed, and nothing happens while the tab is hidden (one refresh when it is shown again).
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import { MenuPopup, MenuEntry } from "./Menu";
import { useConfirm } from "./ui";
import { powerConfirm, powerLabel } from "./power";
import { ChevronRight, ChevronDown, Ellipsis, Play, Power, Terminal, Pause, RotateCw, Camera, Copy, ArrowRightLeft, Plus, Wrench, HardDrive, Network, Cable, Boxes, LayoutTemplate, ExternalLink, Search } from "lucide-react";
import { get, NodeStatus, Network as Net, Pool, post, put, ServerEvent, VM } from "./api";
import { atLeast, useRole } from "./auth";
import { useEvents } from "./live";
import { TreeTags } from "./TreeTags";
import "./shell.css";

interface NIC {
  name: string;
  state: string;
  type: string;
  master?: string;
}
interface NodeData {
  pools?: Pool[];
  nets?: Net[];
  nics?: NIC[];
}
type Kind = "dc" | "node" | "vm" | "group" | "pool" | "net" | "nic";
interface RowModel {
  key: string;
  kind: Kind;
  level: number;
  label: string;
  href: string;
  open?: boolean; // undefined: not expandable
  data?: unknown;
  extra?: string; // state / percentage / type, already formatted
  muted?: boolean;
  node?: number;
}
type MenuItem = MenuEntry;
interface Quick {
  label: string;
  icon: LucideIcon;
  run: () => void;
}

const STORE = "vp.tree.open";
const defaultOpen = (key: string) => key === "dc" || /^n:\d+$/.test(key);
function loadOpen(): Record<string, boolean> {
  try {
    return JSON.parse(localStorage.getItem(STORE) ?? "{}") ?? {};
  } catch {
    return {};
  }
}

/** Keeps the previous object whenever the new one is field-for-field equal, so memoised rows stay put. */
function reconcile<T>(old: T[] | undefined, next: T[], id: (x: T) => string): T[] {
  if (!old) return next;
  const m = new Map(old.map((o) => [id(o), o]));
  let same = old.length === next.length;
  const out = next.map((n, i) => {
    const o = m.get(id(n));
    if (o && JSON.stringify(o) === JSON.stringify(n)) {
      if (old[i] !== o) same = false;
      return o;
    }
    same = false;
    return n;
  });
  return same ? old : out;
}

function useDebounced(fn: () => void, ms = 300) {
  const t = useRef<number>();
  const ref = useRef(fn);
  ref.current = fn;
  return useCallback(() => {
    window.clearTimeout(t.current);
    t.current = window.setTimeout(() => ref.current(), ms);
  }, [ms]);
}

const hidden = () => document.visibilityState === "hidden";

export default function Tree() {
  const role = useRole();
  const canOp = atLeast(role, "operator");
  const canAdmin = role === "admin";
  const nav = useNavigate();
  const loc = useLocation();

  const [nodes, setNodes] = useState<NodeStatus[] | undefined>();
  const [vms, setVMs] = useState<VM[] | undefined>();
  const [data, setData] = useState<Record<number, NodeData>>({});
  const [open, setOpen] = useState(loadOpen);
  const [filter, setFilter] = useState("");
  const [tabKey, setTabKey] = useState("");
  const [busy, setBusy] = useState<Set<string>>(new Set());
  const [error, setError] = useState("");
  const [menu, setMenu] = useState<{ x: number; y: number; up: number; items: MenuItem[]; key: string } | null>(null);
  const box = useRef<HTMLDivElement>(null);
  const confirm = useConfirm();
  const askRef = useRef(confirm.ask);
  askRef.current = confirm.ask;

  // ---- data ----
  const dirty = useRef(new Set<string>()); // what to refresh when the tab becomes visible: "nodes", "vms", "d:<id>"
  const wanted = useRef({ storage: new Set<number>(), network: new Set<number>() });

  const dataRef = useRef(data);
  dataRef.current = data;
  const loadNodes = useCallback(
    () => get<{ nodes: NodeStatus[] }>("/nodes").then((r) => setNodes((o) => reconcile(o, r.nodes, (n) => String(n.id))), () => {}),
    [],
  );
  const loadVMs = useCallback(() => get<VM[]>("/vms").then((r) => setVMs((o) => reconcile(o, r, (v) => v.node + "/" + v.uuid)), () => {}), []);
  const loadStorage = useCallback(
    (id: number) =>
      get<Pool[]>(`/nodes/${id}/pools`).then(
        (p) => setData((d) => ({ ...d, [id]: { ...d[id], pools: reconcile(d[id]?.pools, p, (x) => x.uuid) } })),
        () => setData((d) => ({ ...d, [id]: { ...d[id], pools: d[id]?.pools ?? [] } })),
      ),
    [],
  );
  // The interfaces endpoint may not exist (404) or fail: show networks only, no error.
  const loadNetwork = useCallback(
    // Host NICs cost an SSH command on the node: fetched once, not on every network event.
    (id: number, refresh = false) =>
      Promise.all([
        get<Net[]>(`/nodes/${id}/networks`).catch(() => undefined),
        refresh && dataRef.current[id]?.nics ? undefined : get<{ interfaces: NIC[] }>(`/nodes/${id}/interfaces`).then((r) => r.interfaces ?? [], () => [] as NIC[]),
      ]).then(([n, nics]) =>
        setData((d) => ({
          ...d,
          [id]: { ...d[id], nets: n ? reconcile(d[id]?.nets, n, (x) => x.uuid) : d[id]?.nets ?? [], nics: nics ? reconcile(d[id]?.nics, nics, (x) => x.name) : d[id]?.nics },
        })),
      ),
    [],
  );

  const firstLoad = useRef(0);
  useEffect(() => {
    firstLoad.current = Date.now();
    loadNodes();
    loadVMs();
  }, [loadNodes, loadVMs]);

  const refreshNode = useCallback(
    (id: number) => {
      if (wanted.current.storage.has(id)) loadStorage(id);
      if (wanted.current.network.has(id)) loadNetwork(id, true);
    },
    [loadStorage, loadNetwork],
  );
  const flushVMs = useDebounced(() => (hidden() ? dirty.current.add("vms") : loadVMs()));
  const flushNodes = useDebounced(() => (hidden() ? dirty.current.add("nodes") : loadNodes()));
  const pending = useRef(new Set<number>());
  const flushData = useDebounced(() => {
    const ids = [...pending.current];
    pending.current.clear();
    ids.forEach((id) => (hidden() ? dirty.current.add("d:" + id) : refreshNode(id)));
  });

  useEvents((e: ServerEvent) => {
    if (e.type === "domain") flushVMs();
    else if (e.type === "node") {
      // The socket's "resync" on connect right after our own first load would only repeat it.
      if (e.detail === "resync" && Date.now() - firstLoad.current < 4000) return;
      flushNodes();
      if (e.detail === "resync") {
        flushVMs();
        wanted.current.storage.forEach((id) => pending.current.add(id));
        wanted.current.network.forEach((id) => pending.current.add(id));
        flushData();
      }
    } else if (e.type === "task") {
      if (e.task?.status !== "running") flushVMs();
    } else if ((e.type === "pool" || e.type === "network") && e.node !== undefined) {
      pending.current.add(e.node);
      flushData();
    }
  });
  useEffect(() => {
    const on = () => {
      if (hidden()) return;
      const d = [...dirty.current];
      dirty.current.clear();
      d.forEach((k) => (k === "vms" ? loadVMs() : k === "nodes" ? loadNodes() : refreshNode(Number(k.slice(2)))));
    };
    document.addEventListener("visibilitychange", on);
    return () => document.removeEventListener("visibilitychange", on);
  }, [loadVMs, loadNodes, refreshNode]);

  // ---- open state ----
  const needle = filter.trim().toLowerCase();
  const isOpen = useCallback((key: string) => (needle ? true : open[key] ?? defaultOpen(key)), [open, needle]);
  const setOpenKey = useCallback((key: string, v: boolean) => {
    setOpen((o) => {
      const n = { ...o, [key]: v };
      try {
        localStorage.setItem(STORE, JSON.stringify(n));
      } catch {
        /* storage unavailable: state lasts for this page only */
      }
      return n;
    });
  }, []);

  // The current route's item and its ancestors.
  const selected = useMemo(() => {
    const p = loc.pathname;
    let m;
    if ((m = p.match(/^\/vm\/(\d+)\/([^/]+)/))) return { key: `v:${m[1]}/${m[2]}`, anc: ["dc", `n:${m[1]}`] };
    if ((m = p.match(/^\/node\/(\d+)\/storage/))) return { key: `s:${m[1]}`, anc: ["dc", `n:${m[1]}`] };
    if ((m = p.match(/^\/node\/(\d+)\/network/))) return { key: `w:${m[1]}`, anc: ["dc", `n:${m[1]}`] };
    if ((m = p.match(/^\/node\/(\d+)/))) return { key: `n:${m[1]}`, anc: ["dc"] };
    if (p === "/nodes") return { key: "dc", anc: [] as string[] };
    return { key: "", anc: [] as string[] };
  }, [loc.pathname]);
  useEffect(() => {
    setOpen((o) => (selected.anc.every((k) => (o[k] ?? defaultOpen(k))) ? o : { ...o, ...Object.fromEntries(selected.anc.map((k) => [k, true])) }));
  }, [selected]);

  // ---- rows ----
  const byNode = useMemo(() => {
    const m = new Map<number, VM[]>();
    [...(vms ?? [])].sort((a, b) => a.name.localeCompare(b.name)).forEach((v) => m.set(v.node, [...(m.get(v.node) ?? []), v]));
    return m;
  }, [vms]);

  const rows = useMemo(() => {
    const out: RowModel[] = [];
    const hit = (s: string) => !needle || s.toLowerCase().includes(needle);
    const wantS = new Set<number>(),
      wantN = new Set<number>();
    for (const n of nodes ?? []) {
      const name = n.hostname || n.addr;
      const nodeHit = hit(name);
      const list: RowModel[] = [];
      const online = n.state === "online";
      const d = data[n.id] ?? {};
      const nk = `n:${n.id}`;
      const nodeOpen = isOpen(nk);
      const push = (r: RowModel) => list.push(r);
      for (const v of byNode.get(n.id) ?? []) {
        if (nodeHit || hit(v.name) || (v.tags ?? []).some(hit)) {
          push({ key: `v:${n.id}/${v.uuid}`, kind: "vm", level: 3, label: v.name, href: `/vm/${n.id}/${v.uuid}`, data: v, extra: v.state, node: n.id });
        }
      }
      if (online) {
        const sk = `s:${n.id}`,
          wk = `w:${n.id}`;
        const pools = (d.pools ?? []).filter((p) => nodeHit || hit(p.name));
        const sOpen = isOpen(sk) && nodeOpen;
        if (sOpen) wantS.add(n.id);
        if (!needle || nodeHit || pools.length || d.pools === undefined) {
          push({ key: sk, kind: "group", level: 3, label: "Storage", href: `/node/${n.id}/storage`, open: sOpen, extra: d.pools ? String(d.pools.length) : "", node: n.id });
          if (sOpen) for (const p of pools) push({ key: `p:${n.id}/${p.uuid}`, kind: "pool", level: 4, label: p.name, href: `/node/${n.id}/storage`, data: p, extra: p.state !== "running" ? "inactive" : p.capacity ? Math.round((p.allocation / p.capacity) * 100) + "%" : "", muted: p.state !== "running", node: n.id });
        }
        const nets = (d.nets ?? []).filter((x) => nodeHit || hit(x.name));
        const nics = (d.nics ?? []).filter((x) => nodeHit || hit(x.name));
        const wOpen = isOpen(wk) && nodeOpen;
        if (wOpen) wantN.add(n.id);
        if (!needle || nodeHit || nets.length || nics.length || d.nets === undefined) {
          push({ key: wk, kind: "group", level: 3, label: "Network", href: `/node/${n.id}/network`, open: wOpen, extra: d.nets ? String((d.nets?.length ?? 0) + (d.nics?.length ?? 0)) : "", node: n.id });
          if (wOpen) {
            for (const x of nets) push({ key: `m:${n.id}/${x.uuid}`, kind: "net", level: 4, label: x.name, href: `/node/${n.id}/network`, data: x, extra: x.active ? "" : "inactive", muted: !x.active, node: n.id });
            for (const x of nics) push({ key: `i:${n.id}/${x.name}`, kind: "nic", level: 4, label: x.name, href: `/node/${n.id}/network`, data: x, extra: x.state === "up" ? x.type : x.state === "down" ? "down" : x.type, muted: x.state === "down", node: n.id });
          }
        }
      }
      if (needle && !nodeHit && list.every((r) => r.kind === "group")) continue;
      out.push({ key: nk, kind: "node", level: 2, label: name, href: `/node/${n.id}`, open: list.length ? nodeOpen : undefined, data: n, extra: n.state + (n.maintenance ? " maintenance" : ""), node: n.id });
      if (nodeOpen) out.push(...list);
    }
    wanted.current = { storage: wantS, network: wantN };
    return [{ key: "dc", kind: "dc" as Kind, level: 1, label: "Datacenter", href: "/nodes", open: isOpen("dc") } as RowModel, ...(isOpen("dc") ? out : [])];
  }, [nodes, byNode, data, isOpen, needle]);

  // Fetch what became visible: opened groups only.
  const wantedKey = rows.filter((r) => r.kind === "group" && r.open).map((r) => r.key).join(",");
  useEffect(() => {
    for (const id of wanted.current.storage) if (data[id]?.pools === undefined) loadStorage(id);
    for (const id of wanted.current.network) if (data[id]?.nets === undefined) loadNetwork(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wantedKey, loadStorage, loadNetwork]);
  // A node that went away loses its cached children; it is refetched when it returns and its groups are open.
  const offlineKey = (nodes ?? []).filter((n) => n.state !== "online").map((n) => n.id).join(",");
  useEffect(() => {
    setData((d) => {
      const ids = offlineKey.split(",").filter(Boolean).map(Number).filter((id) => d[id]);
      if (!ids.length) return d;
      const n = { ...d };
      ids.forEach((id) => delete n[id]);
      return n;
    });
  }, [offlineKey]);

  // ---- actions ----
  const fail = (e: any) => setError(String(e?.message ?? e));
  useEffect(() => {
    if (!error) return;
    const t = setTimeout(() => setError(""), 8000);
    return () => clearTimeout(t);
  }, [error]);
  const vmAction = useCallback(async (v: VM, a: string) => {
    const k = `v:${v.node}/${v.uuid}`;
    setBusy((b) => new Set(b).add(k));
    try {
      await post(`/nodes/${v.node}/vms/${v.uuid}/action`, { action: a });
      loadVMs();
    } catch (e) {
      fail(e);
    } finally {
      setBusy((b) => {
        const n = new Set(b);
        n.delete(k);
        return n;
      });
    }
  }, [loadVMs]);

  // Destructive power actions ask first.
  const power = useCallback(
    async (v: VM, a: string) => {
      const spec = powerConfirm(a, [v.name]);
      if (spec && !(await askRef.current(spec))) return;
      return vmAction(v, a);
    },
    [vmAction],
  );

  const quick = useCallback(
    (v: VM): Quick[] => {
      if (!canOp || v.template) return [];
      const go = (tab: string) => nav(`/vm/${v.node}/${v.uuid}?tab=${tab}`);
      if (v.state === "shutoff") return [{ label: powerLabel.start, icon: Play, run: () => power(v, "start") }];
      if (v.state === "paused") return [{ label: powerLabel.resume, icon: Play, run: () => power(v, "resume") }];
      if (v.state === "running" || v.state === "blocked")
        return [
          { label: powerLabel.shutdown, icon: Power, run: () => power(v, "shutdown") },
          { label: "Console", icon: Terminal, run: () => go("console") },
        ];
      return [];
    },
    [canOp, nav, vmAction, power],
  );
  const vmItems = useCallback(
    (v: VM): MenuItem[] => {
      const page = (tab?: string) => nav(`/vm/${v.node}/${v.uuid}` + (tab ? `?tab=${tab}` : ""));
      const items: MenuItem[] = [{ label: "Open", icon: ExternalLink, run: () => page() }];
      if (!canOp) return items;
      const on = v.state !== "shutoff";
      if (v.state === "shutoff" && !v.template) items.push({ label: powerLabel.start, icon: Play, run: () => power(v, "start") });
      if (v.state === "paused") items.push({ label: powerLabel.resume, icon: Play, run: () => power(v, "resume") });
      if (v.state === "running") {
        items.push({ label: powerLabel.reboot, icon: RotateCw, run: () => power(v, "reboot") });
        items.push({ label: powerLabel.shutdown, icon: Power, run: () => power(v, "shutdown") });
        items.push({ label: powerLabel.pause, icon: Pause, run: () => power(v, "pause") });
      }
      if (on) {
        items.push({ label: powerLabel.poweroff, icon: Power, danger: true, run: () => power(v, "poweroff") });
        items.push({ label: powerLabel.reset, icon: RotateCw, danger: true, run: () => power(v, "reset") });
        items.push({ label: "Console", icon: Terminal, run: () => page("console") });
      }
      items.push("sep", { label: "Snapshots", icon: Camera, run: () => page("snapshots") }, { label: "Migrate", icon: ArrowRightLeft, run: () => page() }, { label: "Clone", icon: Copy, run: () => page() });
      return items;
    },
    [canOp, nav, power],
  );
  const nodeItems = useCallback(
    (n: NodeStatus): MenuItem[] => {
      const items: MenuItem[] = [{ label: "Open node", icon: ExternalLink, run: () => nav(`/node/${n.id}`) }];
      if (canOp && n.state === "online") items.push({ label: "Add VM", icon: Plus, run: () => nav(`/?new=${n.id}`) });
      if (canAdmin) {
        items.push({
          label: n.maintenance ? "End maintenance" : "Enter maintenance",
          icon: Wrench,
          run: () => put(`/nodes/${n.id}/maintenance`, { on: !n.maintenance }).then(loadNodes, fail),
        });
        items.push({ label: "Evacuate", icon: ArrowRightLeft, run: () => nav("/nodes") });
      }
      return items;
    },
    [canOp, canAdmin, nav, loadNodes],
  );

  const rowQuick = (r: RowModel): Quick[] => {
    if (r.kind === "vm") return quick(r.data as VM);
    if (r.kind === "node" && canOp && (r.data as NodeStatus).state === "online") return [{ label: "Add VM", icon: Plus, run: () => nav(`/?new=${r.node}`) }];
    return [];
  };
  const rowItems = (r: RowModel): MenuItem[] => (r.kind === "vm" ? vmItems(r.data as VM) : r.kind === "node" ? nodeItems(r.data as NodeStatus) : []);

  // ---- handlers shared by memoised rows (read latest state through a ref) ----
  const latest = useRef({ rows, rowQuick, rowItems, setOpenKey, nav });
  latest.current = { rows, rowQuick, rowItems, setOpenKey, nav };
  const handlers = useRef({
    toggle: (key: string) => {
      const r = latest.current.rows.find((x) => x.key === key);
      if (r && r.open !== undefined) latest.current.setOpenKey(key, !r.open);
    },
    quick: (r: RowModel) => latest.current.rowQuick(r),
    menu: (r: RowModel, x: number, y: number, up = y) => {
      const items = latest.current.rowItems(r);
      if (items.length) setMenu({ x, y, up, items, key: r.key });
    },
    focus: (key: string) => setTabKey(key),
  }).current;

  // ---- keyboard ----
  const focusKey = (key: string) => box.current?.querySelector<HTMLElement>(`[data-key="${CSS.escape(key)}"]`)?.focus();
  const onKeyDown = (e: React.KeyboardEvent) => {
    const el = (e.target as HTMLElement).closest<HTMLElement>("[data-key]");
    if (!el || (e.target as HTMLElement).closest("button") && e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const key = el.dataset.key!;
    const i = rows.findIndex((r) => r.key === key);
    const r = rows[i];
    if (!r) return;
    let handled = true;
    switch (e.key) {
      case "ArrowDown":
        rows[i + 1] && focusKey(rows[i + 1].key);
        break;
      case "ArrowUp":
        rows[i - 1] && focusKey(rows[i - 1].key);
        break;
      case "Home":
        focusKey(rows[0].key);
        break;
      case "End":
        focusKey(rows[rows.length - 1].key);
        break;
      case "ArrowRight":
        if (r.open === false) setOpenKey(key, true);
        else if (r.open && rows[i + 1]?.level > r.level) focusKey(rows[i + 1].key);
        break;
      case "ArrowLeft":
        if (r.open) setOpenKey(key, false);
        else for (let j = i - 1; j >= 0; j--) if (rows[j].level < r.level) { focusKey(rows[j].key); break; }
        break;
      case "Enter":
        nav(r.href);
        break;
      case "ContextMenu":
      case "F10": {
        if (e.key === "F10" && !e.shiftKey) { handled = false; break; }
        const b = el.getBoundingClientRect();
        handlers.menu(r, b.left + 40, b.bottom, b.top);
        break;
      }
      default:
        handled = false;
    }
    if (handled) e.preventDefault();
  };

  const tab = rows.some((r) => r.key === tabKey) ? tabKey : rows.some((r) => r.key === selected.key) ? selected.key : rows[0].key;

  return (
    <div className="tree-wrap" ref={box}>
      <div className="tree-filter">
        <Search size={14} aria-hidden />
        <input type="search" placeholder="Filter" aria-label="Filter tree" value={filter} onChange={(e) => setFilter(e.target.value)} />
      </div>
      {error && (
        <div className="tree-err" role="alert" onClick={() => setError("")}>
          {error}
        </div>
      )}
      <div className="tree" role="tree" aria-label="Resources" onKeyDown={onKeyDown}>
        {nodes === undefined || vms === undefined ? (
          <div className="tree-skel" aria-hidden />
        ) : (
          rows.map((r) => <TreeRow key={r.key} r={r} selected={r.key === selected.key} tabbable={r.key === tab} busy={busy.has(r.key)} h={handlers} canOp={canOp} />)
        )}
        {nodes && nodes.length === 0 && <div className="tree-note">No nodes yet.</div>}
        {needle && rows.length === 1 && <div className="tree-note">Nothing matches.</div>}
      </div>
      {menu && <MenuPopup x={menu.x} y={menu.y} up={menu.up} items={menu.items} onClose={() => { setMenu(null); focusKey(menu.key); }} />}
      {confirm.dialog}
    </div>
  );
}

type H = { toggle: (k: string) => void; quick: (r: RowModel) => Quick[]; menu: (r: RowModel, x: number, y: number, up?: number) => void; focus: (k: string) => void };

const shallow = (a: any, b: any) => {
  const ka = Object.keys(a);
  return ka.length === Object.keys(b).length && ka.every((k) => a[k] === b[k]);
};

const TreeRow = memo(
  function TreeRow({ r, selected, tabbable, busy, h, canOp }: { r: RowModel; selected: boolean; tabbable: boolean; busy: boolean; h: H; canOp: boolean }) {
    const acts = r.kind === "vm" || r.kind === "node" ? h.quick(r) : [];
    const hasMenu = r.kind === "vm" || (r.kind === "node" && canOp);
    const ti = tabbable ? 0 : -1;
    const st = r.kind === "vm" ? r.extra ?? "" : r.kind === "node" ? (r.data as NodeStatus).state : "";
    const vm = r.kind === "vm" ? (r.data as VM) : undefined;
    const maint = r.kind === "node" && (r.data as NodeStatus).maintenance;
    return (
      <div
        className={"trow" + (selected ? " sel" : "") + (r.muted ? " dim" : "") + (vm?.template ? " tpl" : "") + (vm?.tags?.length ? " has-tags" : "")}
        role="treeitem"
        aria-level={r.level}
        aria-selected={selected}
        aria-expanded={r.open}
        aria-label={vm ? `${r.label}, ${vm.template ? "template, " : ""}${vm.state}${vm.tags?.length ? ", tags: " + vm.tags.join(", ") : ""}` : r.kind === "node" ? `${r.label}, ${st}${maint ? ", maintenance" : ""}` : r.label}
        data-key={r.key}
        data-kind={r.kind}
        data-state={st || undefined}
        tabIndex={ti}
        style={{ paddingLeft: 6 + (r.level - 1) * 14 }}
        onFocus={(e) => e.target === e.currentTarget && h.focus(r.key)}
        onContextMenu={(e) => {
          if (!hasMenu) return;
          e.preventDefault();
          h.menu(r, e.clientX, e.clientY);
        }}
      >
        <span className="chev">
          {r.open !== undefined && (
            <button tabIndex={-1} aria-label={r.open ? "Collapse" : "Expand"} onClick={() => h.toggle(r.key)}>
              {r.open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
            </button>
          )}
        </span>
        <RowIcon r={r} />
        <Link to={r.href} tabIndex={-1} className="lbl" title={r.label}>
          {r.label}
        </Link>
        {vm?.tags?.length ? <TreeTags tags={vm.tags} /> : null}
        {maint && <Wrench size={12} className="maint" aria-label="In maintenance" />}
        {r.kind === "pool" && r.extra && r.extra !== "inactive" && <PoolBar p={r.data as Pool} text={r.extra} />}
        {r.kind !== "pool" && r.extra && !vm && r.kind !== "node" && <span className="x">{r.extra}</span>}
        <span className="acts">
          {acts.map((a) => (
            <button key={a.label} tabIndex={ti} disabled={busy} aria-label={`${a.label} ${r.label}`} title={a.label} onClick={() => a.run()}>
              <a.icon size={14} />
            </button>
          ))}
          {hasMenu && (
            <button
              tabIndex={ti}
              aria-haspopup="menu"
              aria-label={`More actions for ${r.label}`}
              title="More actions"
              onClick={(e) => {
                const b = e.currentTarget.getBoundingClientRect();
                h.menu(r, b.right, b.bottom, b.top);
              }}
            >
              <Ellipsis size={14} />
            </button>
          )}
        </span>
      </div>
    );
  },
  (a, b) => shallow(a.r, b.r) && a.selected === b.selected && a.tabbable === b.tabbable && a.busy === b.busy && a.canOp === b.canOp && a.h === b.h,
);

function RowIcon({ r }: { r: RowModel }) {
  switch (r.kind) {
    case "dc":
      return <Boxes size={14} className="ico" aria-hidden />;
    case "node": {
      const s = (r.data as NodeStatus).state;
      return (
        <span className="ico">
          <span className={"dot n-" + s} title={s} />
        </span>
      );
    }
    case "vm": {
      const v = r.data as VM;
      return v.template ? <LayoutTemplate size={14} className="ico" aria-label="Template" /> : <span className="ico"><span className={"dot v-" + v.state} title={v.state} /></span>;
    }
    case "group":
      return r.label === "Storage" ? <HardDrive size={14} className="ico" aria-hidden /> : <Network size={14} className="ico" aria-hidden />;
    case "pool":
      return <HardDrive size={13} className="ico" aria-hidden />;
    case "net":
      return <Network size={13} className="ico" aria-hidden />;
    default:
      return <Cable size={13} className="ico" aria-hidden />;
  }
}

function PoolBar({ p, text }: { p: Pool; text: string }) {
  const pct = p.capacity ? Math.min(100, (p.allocation / p.capacity) * 100) : 0;
  return (
    <span className="pbar" title={`${text} used`}>
      <span className="x">{text}</span>
      <span className="track">
        <span className={pct > 90 ? "full" : ""} style={{ width: pct + "%" }} />
      </span>
    </span>
  );
}
