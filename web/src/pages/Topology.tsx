import { ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import ChevronDown from "lucide-react/icons/chevron-down";
import ChevronRight from "lucide-react/icons/chevron-right";
import { get, NodeStatus, put } from "../api";
import { CheckItem } from "../Checks";
import { useLive } from "../live";
import { useLiveDebounced } from "../liveDebounced";
import { PRESETS } from "../tagColor";
import { atLeast, useRole } from "../auth";
import { DropdownMenu } from "../Menu";
import { ErrorBox, StateBadge, useAction } from "../ui";
import { MigNetResult, MigrationResult, resultItems } from "./MigrationNetwork";
import { NodeFilter, netEvents, nodeEvents } from "./dcShared";
import { AddNetworkDialog, ChangeBanner, SwitchDialog, SwitchMode, useHostnet } from "./VirtualSwitch";
import { addBlocked, addrIP, HostBridge, HostnetState } from "./vswitchApi";
import "./topology.css";

// The shape of GET /api/topology (internal/api/topology.go).
interface Addr {
  address: string;
  management?: boolean;
  migration?: boolean;
  /** An address of the node on a VLAN of the switch (the VLAN device `iface`). */
  vlan?: number;
  iface?: string;
}
interface Member {
  name: string;
  state: "up" | "down";
  speedMbps?: number;
  active?: boolean;
}
interface Uplink {
  name: string;
  type: string;
  state: string;
  speedMbps?: number;
  members?: string[];
  mode?: "failover" | "lacp";
  memberStates?: Member[];
  lacpAnswered?: boolean;
  addresses: Addr[];
  management?: boolean;
  migration?: boolean;
}
interface Iface {
  vm: string;
  uuid: string;
  state: string;
  mac: string;
  ips: string[];
  linkDown?: boolean;
}
interface PortGroup {
  id: string;
  kind: "network" | "direct" | "missing";
  network?: string;
  mode?: string;
  vlan?: number;
  dhcp?: string;
  active: boolean;
  interfaces: Iface[];
}
interface Switch {
  id: string;
  kind: "linux-bridge" | "openvswitch" | "libvirt" | "other";
  name: string;
  state?: string;
  mtu?: number;
  uplinkNote?: string;
  uplinks: Uplink[];
  hostAddresses: Addr[];
  portGroups: PortGroup[];
}
interface TopoNode {
  node: number;
  name: string;
  error?: string;
  switches: Switch[];
  unused: Uplink[];
}

const KIND: Record<Switch["kind"], string> = { "linux-bridge": "Linux bridge", openvswitch: "Open vSwitch bridge", libvirt: "libvirt network bridge", other: "Not on a bridge" };
const SHOWN_OPEN = 3;

/** One accent per network name, the same on every node, from the fixed tag palette. The name is always written too. */
function accent(name: string): string {
  let h = 0;
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return PRESETS[h % PRESETS.length];
}

const speed = (m?: number) => (m ? (m >= 1000 ? `${m / 1000} Gbit/s` : `${m} Mbit/s`) : "");

/** "Use for migration" on an address (administrators): `can` says for which address it is offered. */
interface UseMigration {
  can: (a: Addr) => boolean;
  run: (a: Addr) => void;
  busy: boolean;
}

function Addrs({ list, use }: { list: Addr[]; use?: UseMigration }) {
  return (
    <>
      {list.filter((a) => !/^fe80:/i.test(a.address)).map((a) => (
        <span key={a.iface ?? "" + a.address} className="mono topoaddr">
          {a.vlan ? <span className="muted">VLAN {a.vlan} </span> : null}
          {a.address || <span className="muted">waiting for an address</span>}
          {a.management && <span className="topomgmt"> Management</span>}
          {a.migration && <span className="topomgmt"> Migration</span>}
          {use?.can(a) && (
            <button className="link topouse" onClick={() => use.run(a)} disabled={use.busy} data-topo-use={a.address.split("/")[0]}>
              Use for migration
            </button>
          )}
        </span>
      ))}
    </>
  );
}

function UplinkBox({ node, u, action, use }: { node: number; u: Uplink; action?: ReactNode; use?: UseMigration }) {
  // The mark sits by the address when the card shows that address; by the name only when no address carries it.
  const shown = u.addresses.filter((a) => !/^fe80:/i.test(a.address));
  const markManagement = u.management && !shown.some((a) => a.management);
  const markMigration = u.migration && !shown.some((a) => a.migration);
  return (
    <div className="topouplink" data-topo-id={`uplink:${u.name}`}>
      <div className="topotitle">
        <Link to={`/node/${node}/network`}>{u.name}</Link>
        <StateBadge state={u.state === "up" ? "up" : u.state === "down" ? "down" : "unknown"} />
        {speed(u.speedMbps) && <span className="muted">{speed(u.speedMbps)}</span>}
        {markManagement && <span className="topomgmt">Management</span>}
        {markMigration && <span className="topomgmt">Migration</span>}
      </div>
      <div className="muted">
        {u.type === "bond" ? "Bond" : u.type === "vlan" ? "VLAN device" : "Network card"}
        {u.type === "bond" && u.mode && <>, {u.mode === "lacp" ? "load balance (LACP)" : "failover"}</>}
        {!u.memberStates && u.members && u.members.length > 0 && <> of {u.members.join(", ")}</>}
      </div>
      {u.memberStates && u.memberStates.length > 0 && (
        <ul className="topoifs" aria-label={`Cards of ${u.name}`}>
          {u.memberStates.map((m) => (
            <li key={m.name} className="topoif" data-topo-id={`member:${m.name}`}>
              <StateBadge state={m.state} />
              <span className="mono">{m.name}</span>
              {speed(m.speedMbps) && <span className="muted">{speed(m.speedMbps)}</span>}
              {m.active && u.mode === "failover" && <span className="topomgmt">Active</span>}
            </li>
          ))}
        </ul>
      )}
      {u.type === "bond" && u.mode === "lacp" && u.lacpAnswered === false && (
        <div>
          <span className="topodown" data-topo-id={`lacp:${u.name}`}>
            LACP not answered by the switch
          </span>
        </div>
      )}
      {u.addresses.length > 0 && (
        <div>
          <Addrs list={u.addresses} use={use} />
        </div>
      )}
      {action}
    </div>
  );
}

function IfaceRow({ node, i }: { node: number; i: Iface }) {
  return (
    <li className="topoif">
      <StateBadge state={i.state} />
      <Link to={`/vm/${node}/${i.uuid}`}>{i.vm}</Link>
      <span className="mono muted">{i.mac}</span>
      {i.ips.length > 0 && <span className="mono">{i.ips.join(", ")}</span>}
      {i.linkDown && <span className="topodown">Link down</span>}
    </li>
  );
}

function PortGroupBox({ node, g, accents }: { node: number; g: PortGroup; accents: boolean }) {
  const title = g.kind === "direct" ? "Direct to bridge" : g.kind === "missing" ? `${g.network} (not found)` : g.network;
  const style = accents && g.kind === "network" && g.network ? { borderLeftColor: accent(g.network) } : undefined;
  return (
    <div className="topopg" style={style} data-topo-id={g.id}>
      <div className="topotitle">
        {g.kind === "network" ? <Link to={`/node/${node}/network`}>{title}</Link> : <span>{title}</span>}
        {g.mode && g.kind === "network" && <span className="muted">{g.mode}</span>}
        {g.vlan ? <span className="muted">VLAN {g.vlan}</span> : null}
        {g.kind === "network" && !g.active && <span className="muted">Stopped</span>}
      </div>
      {g.dhcp && <div className="muted">DHCP {g.dhcp}</div>}
      {g.interfaces.length === 0 ? (
        <div className="muted">No virtual machines</div>
      ) : (
        <ul className="topoifs">
          {g.interfaces.map((i) => (
            <IfaceRow key={i.uuid + i.mac} node={node} i={i} />
          ))}
        </ul>
      )}
    </div>
  );
}

/** What the admin can do with a switch: its menu, the Add network button, the note on hand-made bridges. */
interface SwitchActions {
  info?: HostBridge;
  st?: HostnetState;
  canAddNetwork: boolean;
  onAddNetwork: () => void;
  onEditVlans: () => void;
  onEditUplinks: () => void;
  onEditMtu: () => void;
  onAddAddress: () => void;
  onRemoveAddress: (id: number) => void;
  onRemove: () => void;
}

function SwitchBox({ node, s, accents, act, use }: { node: number; s: Switch; accents: boolean; act?: SwitchActions; use?: UseMigration }) {
  const info = act?.info;
  const inUse = info ? [...(info.networks.length ? [`${info.networks.length} ${info.networks.length === 1 ? "network" : "networks"}`] : []), ...(info.vms.length ? [`${info.vms.length} ${info.vms.length === 1 ? "VM" : "VMs"}`] : [])] : [];
  const removeWhy = info && inUse.length ? `The virtual switch is used by ${inUse.join(" and ")}. Remove or move them first.` : "";
  return (
    <section className="toposwitch" data-topo-id={s.id} aria-label={`Switch ${s.name}`}>
      <header>
        <h4>{s.name}</h4>
        <span className="muted">{KIND[s.kind]}</span>
        {s.state && s.kind !== "other" && <StateBadge state={s.state === "up" || s.state === "down" || s.state === "idle" ? s.state : "unknown"} />}
        {info?.vlanFiltering && <span className="muted">VLANs {info.vlans && info.vlans !== "2-4094" ? info.vlans : "all"}</span>}
        {s.mtu && s.mtu !== 1500 ? <span className="muted">MTU {s.mtu}</span> : null}
        {act && s.kind === "linux-bridge" && info && !info.managed && info.readOnly !== "managed by libvirt" && <span className="muted">Not managed by Virticle</span>}
        {act && (
          <span className="vsacts">
            {act.canAddNetwork && (
              <button onClick={act.onAddNetwork} data-vsw="add-network">
                Add network
              </button>
            )}
            {info?.managed && (
              <DropdownMenu
                label="Actions"
                ariaLabel={`Actions for ${s.name}`}
                items={[
                  ...(info.uplink ? [{ label: "Edit uplinks", run: act.onEditUplinks }] : []),
                  ...(act.st?.vlanCapable ? [{ label: "Edit VLANs", run: act.onEditVlans }] : []),
                  ...(info.vlanFiltering ? [{ label: "Add this node's address", run: act.onAddAddress }] : []),
                  ...(info.vlanAddresses ?? []).map((v) => {
                    const used = !!v.address && (act.st?.controllerAddresses ?? []).includes(addrIP(v.address));
                    return {
                      label: `Remove address on VLAN ${v.id}`,
                      run: () => act.onRemoveAddress(v.id),
                      disabled: used,
                      title: used ? "The controller reaches this node at this address." : undefined,
                    };
                  }),
                  { label: "Edit MTU", run: act.onEditMtu },
                  { label: removeWhy ? "Remove (in use)" : "Remove", run: act.onRemove, danger: true, disabled: !!removeWhy, title: removeWhy || undefined },
                ]}
              />
            )}
          </span>
        )}
      </header>
      <div className="topobody">
        <div className="topocol">
          <div className="topocolhead">Networks</div>
          {s.portGroups.length === 0 ? <div className="muted">No networks</div> : s.portGroups.map((g) => <PortGroupBox key={g.id} node={node} g={g} accents={accents} />)}
        </div>
        <div className="topotrunk" aria-hidden="true" />
        <div className="topocol">
          <div className="topocolhead">Uplinks</div>
          {s.uplinks.map((u) => (
            <UplinkBox key={u.name} node={node} u={u} use={use} />
          ))}
          {s.uplinks.length === 0 && s.uplinkNote && <div className="topouplink topoplain">{s.uplinkNote}</div>}
          {s.hostAddresses.length > 0 && (
            <div className="topohost" data-topo-id={`host:${s.name}`}>
              <div className="topotitle">
                <span>This node's address</span>
              </div>
              <Addrs list={s.hostAddresses} use={s.kind === "linux-bridge" || s.kind === "openvswitch" ? use : undefined} />
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

/** Drops what the text filter does not match: a port group whose network matches stays whole; otherwise only matching VMs stay. */
function filterNode(n: TopoNode, text: string): TopoNode {
  const t = text.trim().toLowerCase();
  if (!t) return n;
  const has = (s?: string) => !!s && s.toLowerCase().includes(t);
  const switches: Switch[] = [];
  for (const s of n.switches) {
    const whole = has(s.name);
    const groups: PortGroup[] = [];
    for (const g of s.portGroups) {
      if (whole || has(g.network)) groups.push(g);
      else {
        const ifs = g.interfaces.filter((i) => has(i.vm));
        if (ifs.length) groups.push({ ...g, interfaces: ifs });
      }
    }
    if (groups.length) switches.push({ ...s, portGroups: groups });
  }
  return { ...n, switches, unused: [] };
}

/** One node: the admin's line (pending change, Add virtual switch), then its switches and unused cards. */
function NodeBody({ n, accents, admin, reload }: { n: TopoNode; accents: boolean; admin: boolean; reload: () => void }) {
  const net = useHostnet(n.node, admin && !n.error);
  const st = net.data;
  const [dlg, setDlg] = useState<{ mode: SwitchMode; bridge?: HostBridge; uplink?: string; vlanId?: number } | null>(null);
  const [addNet, setAddNet] = useState<string | null>(null);
  const changed = () => {
    reload();
    net.reload();
  };
  const blocked = addBlocked(st, net.error);
  const bridgeOf = (name: string) => st?.bridges.find((b) => b.name === name);
  const switchActions = (s: Switch): SwitchActions | undefined => {
    if (!admin) return undefined;
    const info = bridgeOf(s.name);
    return {
      info,
      st,
      canAddNetwork: s.kind === "linux-bridge",
      onAddNetwork: () => setAddNet(s.name),
      onEditVlans: () => info && setDlg({ mode: "vlans", bridge: info }),
      onEditUplinks: () => info && setDlg({ mode: "uplinks", bridge: info }),
      onEditMtu: () => info && setDlg({ mode: "mtu", bridge: info }),
      onAddAddress: () => info && setDlg({ mode: "address", bridge: info }),
      onRemoveAddress: (id) => info && setDlg({ mode: "removeaddr", bridge: info, vlanId: id }),
      onRemove: () => info && setDlg({ mode: "remove", bridge: info }),
    };
  };
  const migAct = useAction();
  const [migDone, setMigDone] = useState<CheckItem[]>([]);
  const allAddrs = [...n.switches.flatMap((s) => [...s.hostAddresses, ...s.uplinks.flatMap((u) => u.addresses)]), ...n.unused.flatMap((u) => u.addresses)];
  const explicitMig = allAddrs.some((a) => a.migration);
  // The management address is offered only to go back to the default; an address that already carries migration is marked instead.
  const use: UseMigration | undefined = admin
    ? {
        can: (a) => !a.migration && (!a.management || explicitMig),
        busy: migAct.busy,
        run: async (a) => {
          setMigDone([]);
          let res: MigNetResult | undefined;
          const ok = await migAct.run(async () => {
            res = await put<MigNetResult>(`/nodes/${n.node}/migration-network`, { address: a.management ? "" : a.address.split("/")[0] });
          });
          if (ok && res) setMigDone(resultItems(res, "Migration network saved."));
          reload();
        },
      }
    : undefined;
  const canAdd = admin && !!st && !blocked;
  const usable = (u: Uplink) => canAdd && u.type === "physical";
  if (n.error) return <div className="error">This node could not be read: {n.error}</div>;
  return (
    <>
      {admin && (
        <div className="vsbar">
          {st && <ChangeBanner node={n.node} st={st} reload={changed} />}
          <div className="vsadd">
            <button onClick={() => setDlg({ mode: "add" })} disabled={!canAdd} data-vsw="add-switch">
              Add virtual switch
            </button>
            {blocked && <span className="muted vsblocked">{blocked}</span>}
          </div>
        </div>
      )}
      <ErrorBox error={migAct.error} />
      {migDone.length > 0 && <MigrationResult items={migDone} onClose={() => setMigDone([])} />}
      {n.switches.length === 0 && <p className="muted">This node has no bridges and no virtual networks.</p>}
      {n.switches.map((s) => (
        <SwitchBox key={s.id} node={n.node} s={s} accents={accents} act={switchActions(s)} use={use} />
      ))}
      {n.unused.length > 0 && (
        <section className="toposwitch topounused" aria-label="Unused network cards">
          <header>
            <h4>Unused network cards</h4>
            <span className="muted">Not part of any bridge</span>
          </header>
          <div className="topounusedlist">
            {n.unused.map((u) => (
              <UplinkBox
                key={u.name}
                node={n.node}
                u={u}
                use={use}
                action={
                  usable(u) ? (
                    <button className="vsuse" onClick={() => setDlg({ mode: "add", uplink: u.name })} data-vsw="use-card">
                      Use for a new virtual switch
                    </button>
                  ) : undefined
                }
              />
            ))}
          </div>
        </section>
      )}
      {dlg && st && <SwitchDialog key={dlg.mode + (dlg.bridge?.name ?? "") + (dlg.uplink ?? "") + (dlg.vlanId ?? "")} node={n.node} st={st} mode={dlg.mode} bridge={dlg.bridge} uplink={dlg.uplink} vlanId={dlg.vlanId} onClose={() => setDlg(null)} onChanged={changed} />}
      {addNet !== null && (
        <AddNetworkDialog
          node={n.node}
          bridge={addNet}
          info={bridgeOf(addNet)}
          vlanCapable={!!st?.vlanCapable}
          used={(n.switches.find((s) => s.name === addNet)?.portGroups ?? []).filter((g) => g.kind === "network").map((g) => ({ network: g.network ?? "", vlan: g.vlan }))}
          onClose={() => setAddNet(null)}
          onDone={changed}
        />
      )}
    </>
  );
}

/** The network topology of one node (`node`) or of all nodes. Loaded on opening; refreshed on the network and VM events the app already receives, and by the Refresh button. */
export default function Topology({ node }: { node?: number }) {
  const [pick, setPick] = useState(0);
  const [text, setText] = useState("");
  const [open, setOpen] = useState<Set<number>>(new Set());
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes", nodeEvents);
  const q = node ? `?node=${node}` : "";
  // One request covers every node and costs an SSH command per node, so a burst of events gives one reload.
  const topo = useLiveDebounced(() => get<{ nodes: TopoNode[] }>(`/topology${q}`), `topology${q}`, netEvents, 3000);
  const all = topo.data?.nodes;
  const shown = useMemo(() => (all ?? []).filter((n) => !pick || n.node === pick).map((n) => filterNode(n, text)), [all, pick, text]);
  const filtering = text.trim() !== "";
  const single = node !== undefined;
  const visible = filtering ? shown.filter((n) => n.error || n.switches.length > 0) : shown;
  const names = (all ?? []).flatMap((n) => n.switches.flatMap((s) => s.portGroups.filter((g) => g.kind === "network").map((g) => g.network)));
  const accents = !single && names.length > 0;
  const admin = atLeast(useRole(), "admin");

  const toggle = (id: number) =>
    setOpen((s) => {
      const n = new Set(s);
      n.has(id) ? n.delete(id) : n.add(id);
      return n;
    });

  return (
    <div className="topo">
      <ErrorBox error={topo.error} />
      {single ? (
        <div className="filters">
          <input type="search" placeholder="Filter by VM or network name" aria-label="Filter by VM or network name" value={text} onChange={(e) => setText(e.target.value)} />
          <button className="ghost" onClick={topo.reload}>
            Refresh
          </button>
        </div>
      ) : (
        <div className="topobar">
          <NodeFilter nodes={nodes.data?.nodes ?? []} value={pick} onChange={setPick} text={text} onText={setText} placeholder="Filter by VM or network name" shown={visible.length} total={all?.length ?? 0} />
          <button className="ghost" onClick={topo.reload}>
            Refresh
          </button>
        </div>
      )}
      {!all && !topo.error && <p className="muted">Loading the network layout.</p>}
      {all && visible.length === 0 && <div className="empty">{filtering ? "Nothing matches this filter." : "There are no nodes to show."}</div>}
      {visible.map((n, idx) => {
        const expanded = single || filtering || pick !== 0 || idx < SHOWN_OPEN || open.has(n.node);
        return (
          <div key={n.node} className="toponode" data-topo-id={`node:${n.node}`}>
            {single ? null : (
              <h3>
                <button className="ghost nicbtn" aria-expanded={expanded} onClick={() => toggle(n.node)} disabled={filtering || pick !== 0 || idx < SHOWN_OPEN}>
                  {expanded ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
                  {n.name}
                </button>
                <Link to={`/node/${n.node}/network`} className="muted topolink">
                  Open node
                </Link>
              </h3>
            )}
            {expanded && <NodeBody n={n} accents={accents} admin={admin} reload={topo.reload} />}
          </div>
        );
      })}
    </div>
  );
}
