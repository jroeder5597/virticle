import { FormEvent, ReactNode, useEffect, useRef, useState } from "react";
import Play from "lucide-react/icons/play";
import Square from "lucide-react/icons/square";
import Trash2 from "lucide-react/icons/trash-2";
import Zap from "lucide-react/icons/zap";
import ZapOff from "lucide-react/icons/zap-off";
import { del, DhcpHost, get, Network, NodeStatus, post, put } from "../api";
import { orEvents, useLive } from "../live";
import { useLiveDebounced } from "../liveDebounced";
import { useRole } from "../auth";
import { netEvents, NodeErr, NodeErrors, NodeFilter, nodeEvents, nodeLabel, NodeNetwork } from "./dcShared";
import ComboInput from "../ComboInput";
import Select from "../Select";
import { ErrorBox, Field, Modal, NodePicker, PageTitle, StateBadge, useAction, useConfirm } from "../ui";
import { StatusLine } from "../Checks";

const modeNames: Record<string, string> = { nat: "NAT", route: "Routed", open: "Open", isolated: "Isolated", bridge: "Bridge" };
const modeLabel = (n: Network) => (modeNames[n.forward] ?? n.forward) + (n.vlan ? ` · VLAN ${n.vlan}` : "");

/**
 * `fixedNode` pins the page to one node (node page tab): no node picker, no node auto-selection.
 * `allNodes` lists the networks of every node in one table (Datacenter tab), one request for all of them.
 */
export default function Networks({ fixedNode, allNodes, after }: { fixedNode?: number; allNodes?: boolean; after?: (nodes: NodeStatus[]) => ReactNode } = {}) {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes", nodeEvents);
  const [pickedNode, setNode] = useState(0);
  const node = fixedNode ?? pickedNode;
  const [selected, setSelected] = useState<{ node: number; name: string } | undefined>();
  const [creating, setCreating] = useState(false);
  const [filterNode, setFilterNode] = useState(0); // all-nodes mode: 0 is every node
  const [text, setText] = useState("");
  const all = nodes.data?.nodes ?? [];
  useEffect(() => {
    if (!allNodes && fixedNode === undefined && !all.some((n) => n.id === node && n.state === "online")) setNode(all.find((n) => n.state === "online")?.id ?? 0);
  }, [all, node]); // eslint-disable-line react-hooks/exhaustive-deps
  const single = useLive(() => (!allNodes && node ? get<Network[]>(`/nodes/${node}/networks`) : Promise.resolve([])), `nets/${allNodes ? "-" : node}`, orEvents("network"));
  const every = useLiveDebounced(
    () => (allNodes ? get<{ networks: NodeNetwork[]; errors: NodeErr[] }>("/networks") : Promise.resolve({ networks: [] as NodeNetwork[], errors: [] as NodeErr[] })),
    `nets/all/${!!allNodes}`,
    netEvents,
  );
  const nets = allNodes ? every : single;
  const act = useAction();
  const confirm = useConfirm();
  const nodeName = nodeLabel(all.find((n) => n.id === node) ?? { addr: String(node) });
  const rows: NodeNetwork[] = allNodes ? every.data?.networks ?? [] : (single.data ?? []).map((n) => ({ ...n, node, nodeName }));
  const needle = text.trim().toLowerCase();
  const shown = rows.filter((n) => (!filterNode || n.node === filterNode) && (!needle || [n.name, n.nodeName, n.address ?? "", n.bridge ?? "", modeLabel(n)].some((s) => s.toLowerCase().includes(needle))));
  const net = shown.find((n) => n.name === selected?.name && (!selected.node || selected.node === n.node)) ?? shown[0];
  const online = all.filter((n) => n.state === "online");
  const createNode = allNodes ? (online.some((n) => n.id === filterNode) ? filterNode : online[0]?.id ?? 0) : node;
  const action = async (n: NodeNetwork, body: object) => {
    if (await act.run(() => post(`/nodes/${n.node}/networks/${encodeURIComponent(n.name)}/action`, body))) nets.reload();
  };

  const stop = async (n: NodeNetwork) => {
    const running = [...new Set(n.interfaces.filter((i) => i.running).map((i) => i.vm))];
    const ok = await confirm.ask({
      title: `Stop network ${n.name}`,
      message: running.length
        ? `The network ${n.name} stops. The running VMs below are attached to it and lose their connectivity.`
        : `The network ${n.name} stops. VMs that use it lose their connection until it is started again.`,
      items: running,
      confirm: "Stop network",
    });
    if (ok) return action(n, running.length ? { action: "stop", force: true } : { action: "stop" });
  };
  const remove = async (n: NodeNetwork) => {
    const attached = [...new Set(n.interfaces.map((i) => i.vm))];
    const ok = await confirm.ask({
      title: `Delete network ${n.name}`,
      message: `The network ${n.name} is deleted.` + (attached.length ? " The VMs below are still attached to it and will not start until you give them another network." : ""),
      items: attached,
      confirm: "Delete network",
    });
    if (ok) return action(n, { action: "delete", force: n.active || attached.length > 0 });
  };

  return (
    <>
      <div className="head">
        {fixedNode === undefined ? <PageTitle>Networks</PageTitle> : <h2>Virtual networks</h2>}
        <div className="actions">
          {fixedNode === undefined && !allNodes && all.length > 0 && <NodePicker nodes={all} value={node} onChange={setNode} />}
          <button className="primary" disabled={!createNode} onClick={() => setCreating(true)}>
            New network
          </button>
        </div>
      </div>
      <ErrorBox error={nets.error ?? act.error} />
      <NodeErrors errors={every.data?.errors} />
      {allNodes && <NodeFilter nodes={all} value={filterNode} onChange={setFilterNode} text={text} onText={setText} placeholder="Search networks by name, node or address" shown={shown.length} total={rows.length} />}
      {nets.data && shown.length === 0 ? (
        <div className="empty">{allNodes ? (rows.length ? "No network matches the filter." : "No virtual networks on the connected nodes.") : node ? "No virtual networks on this node." : "No node is online."}</div>
      ) : (
        <table className={allNodes ? "dctable" : ""}>
          <thead>
            <tr>
              <th>Name</th>
              {allNodes && <th>Node</th>}
              <th>State</th>
              <th>Mode</th>
              {!allNodes && <th>Bridge</th>}
              <th>Address</th>
              <th>DHCP</th>
              <th className="num">VM interfaces</th>
              <th>{allNodes ? "Autostart" : "Starts with host"}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {shown.map((n) => (
              <tr key={n.node + "/" + n.name} className={net?.node === n.node && net?.name === n.name ? "selected" : ""} onClick={() => setSelected({ node: allNodes ? n.node : 0, name: n.name })}>
                <td>{n.name}</td>
                {allNodes && <td>{n.nodeName}</td>}
                <td>
                  <StateBadge state={n.active ? "active" : "inactive"} />
                </td>
                <td>{modeLabel(n)}</td>
                {!allNodes && <td className="mono">{n.bridge}</td>}
                <td className="mono">{n.address}</td>
                <td>{n.dhcp ? `${n.hosts.length} reserved` : ""}</td>
                <td className="num">{n.interfaces.length}</td>
                <td>{n.autostart ? "Yes" : "No"}</td>
                <td className="actions" onClick={(e) => e.stopPropagation()}>
                  {allNodes ? (
                    <>
                      {n.active ? (
                        <button className="iconbtn" disabled={act.busy} onClick={() => stop(n)} aria-label="Stop" title="Stop">
                          <Square size={15} aria-hidden="true" />
                        </button>
                      ) : (
                        <button className="iconbtn" disabled={act.busy} onClick={() => action(n, { action: "start" })} aria-label="Start" title="Start">
                          <Play size={15} aria-hidden="true" />
                        </button>
                      )}
                      <button
                        className="iconbtn"
                        disabled={act.busy}
                        onClick={() => action(n, { action: n.autostart ? "noautostart" : "autostart" })}
                        aria-label={n.autostart ? "No autostart" : "Autostart"}
                        title={n.autostart ? "No autostart" : "Autostart"}
                      >
                        {n.autostart ? <ZapOff size={15} aria-hidden="true" /> : <Zap size={15} aria-hidden="true" />}
                      </button>
                      <button className="iconbtn danger" disabled={act.busy} onClick={() => remove(n)} aria-label="Delete" title="Delete">
                        <Trash2 size={15} aria-hidden="true" />
                      </button>
                    </>
                  ) : (
                    <>
                      {n.active ? (
                        <button disabled={act.busy} onClick={() => stop(n)}>
                          Stop
                        </button>
                      ) : (
                        <button disabled={act.busy} onClick={() => action(n, { action: "start" })}>
                          Start
                        </button>
                      )}
                      <button disabled={act.busy} onClick={() => action(n, { action: n.autostart ? "noautostart" : "autostart" })}>
                        {n.autostart ? "No autostart" : "Autostart"}
                      </button>
                      <button className="danger" disabled={act.busy} onClick={() => remove(n)}>
                        Delete
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {confirm.dialog}
      {after?.(all)}
      {net && <Detail key={net.node + "/" + net.name} base={`/nodes/${net.node}/networks`} name={net.name} />}
      {creating && <CreateNetwork node={createNode} nodes={allNodes ? online : undefined} onClose={() => setCreating(false)} onDone={(n, name) => { setSelected({ node: allNodes ? n : 0, name }); nets.reload(); }} />}
    </>
  );
}

function Detail({ base, name }: { base: string; name: string }) {
  const url = `${base}/${encodeURIComponent(name)}`;
  const d = useLive(() => get<Network>(url), url, orEvents("network"));
  // Leases come and go without any libvirt event, so look again now and then.
  useEffect(() => {
    // Only a running network with a DHCP server has leases to follow.
    const t = setInterval(() => document.visibilityState !== "hidden" && dhcpRunning.current && d.reload(), 10000);
    return () => clearInterval(t);
  }, [d.reload]);
  const dhcpRunning = useRef(false);
  dhcpRunning.current = !!(d.data?.dhcp && d.data.active);
  const n = d.data;
  const viewer = useRole() === "viewer";
  return (
    <>
      <div className="head sub">
        <h2>{name}</h2>
        <button onClick={d.reload}>Refresh</button>
      </div>
      <ErrorBox error={d.error} />
      {n && (
        <fieldset className="plain" disabled={viewer}>
        <div className="cards">
          <div className="card">
            <h3>Details</h3>
            <dl>
              <dt>UUID</dt>
              <dd className="mono">{n.uuid}</dd>
              <dt>Definition</dt>
              <dd>{n.persistent ? "Persistent" : "Transient (gone after it stops)"}</dd>
              <dt>Mode</dt>
              <dd>{modeLabel(n)}</dd>
              {n.ovs && (
                <>
                  <dt>Switch</dt>
                  <dd>Open vSwitch</dd>
                </>
              )}
              <dt>Bridge</dt>
              <dd className="mono">{n.bridge || "-"}</dd>
              <dt>Address</dt>
              <dd className="mono">{n.address || "none (layer 2 only)"}</dd>
              {n.dhcp && (
                <>
                  <dt>DHCP range</dt>
                  <dd className="mono">
                    {n.dhcpStart} - {n.dhcpEnd}
                  </dd>
                </>
              )}
              {n.domain && (
                <>
                  <dt>DNS domain</dt>
                  <dd>{n.domain}</dd>
                </>
              )}
              <dt>Attached</dt>
              <dd>
                {n.interfaces.length === 0
                  ? "No VM interfaces"
                  : n.interfaces.map((i) => (
                      <div key={i.uuid + i.mac}>
                        {i.vm} <span className="mono muted">{i.mac}</span> {i.running ? "" : <span className="muted">(VM stopped)</span>}
                      </div>
                    ))}
              </dd>
            </dl>
          </div>
          <div className="card">
            <h3>DHCP leases</h3>
            {!n.dhcp ? (
              <div className="muted">This network has no DHCP server.</div>
            ) : !n.active ? (
              <div className="muted">Start the network to see leases.</div>
            ) : n.leases.length === 0 ? (
              <div className="muted">No active leases.</div>
            ) : (
              <table className="compact">
                <thead>
                  <tr>
                    <th>IP</th>
                    <th>MAC</th>
                    <th>Host name</th>
                    <th>Expires</th>
                  </tr>
                </thead>
                <tbody>
                  {n.leases.map((l) => (
                    <tr key={l.mac + l.ip}>
                      <td className="mono">{l.ip}</td>
                      <td className="mono">{l.mac}</td>
                      <td>{l.hostname}</td>
                      <td>{new Date(l.expiry * 1000).toLocaleTimeString()}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
          {n.dhcp && <Reservations url={url} n={n} onChange={d.reload} />}
          <XmlEditor url={url} n={n} onSaved={d.reload} />
        </div>
        </fieldset>
      )}
    </>
  );
}

function Reservations({ url, n, onChange }: { url: string; n: Network; onChange: () => void }) {
  const [f, setF] = useState({ mac: "", ip: "", name: "" });
  const [editing, setEditing] = useState(false);
  const confirm = useConfirm();
  const act = useAction();
  const reserved = new Set(n.hosts.map((h) => h.mac.toLowerCase()));
  const free = n.interfaces.filter((i) => !reserved.has(i.mac.toLowerCase()));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const ok = await act.run(() =>
      editing ? put(`${url}/dhcp-hosts/${encodeURIComponent(f.mac)}`, { ip: f.ip, name: f.name }) : post(`${url}/dhcp-hosts`, f),
    );
    if (ok) {
      setF({ mac: "", ip: "", name: "" });
      setEditing(false);
      onChange();
    }
  };
  const edit = (h: DhcpHost) => {
    setF({ mac: h.mac, ip: h.ip, name: h.name ?? "" });
    setEditing(true);
  };
  const remove = async (h: DhcpHost) => {
    const ok = await confirm.ask({
      title: "Remove DHCP reservation",
      message: `The reservation of ${h.ip} for ${h.mac}${h.name ? ` (${h.name})` : ""} on network ${n.name} is removed. The guest may get another address when its lease renews.`,
      confirm: "Remove reservation",
    });
    if (ok && (await act.run(() => del(`${url}/dhcp-hosts/${encodeURIComponent(h.mac)}`)))) onChange();
  };
  return (
    <div className="card">
      {confirm.dialog}
      <h3>DHCP reservations</h3>
      <small>Changes apply immediately, without restarting the network. A VM that already has a lease picks up the reserved address when it renews.</small>
      {n.hosts.length > 0 && (
        <table className="compact">
          <thead>
            <tr>
              <th>MAC</th>
              <th>IP</th>
              <th>Name</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {n.hosts.map((h) => (
              <tr key={h.mac}>
                <td className="mono">{h.mac}</td>
                <td className="mono">{h.ip}</td>
                <td>{h.name}</td>
                <td className="actions">
                  <button disabled={act.busy} onClick={() => edit(h)}>
                    Edit
                  </button>
                  <button className="danger" disabled={act.busy} onClick={() => remove(h)}>
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={submit} style={{ marginTop: 12 }}>
        <div className="grid2">
          <Field label="MAC address" hint={free.length > 0 ? "Attached VM interfaces without a reservation are suggested." : undefined}>
            <ComboInput mono value={f.mac} onChange={(mac) => setF({ ...f, mac })} required readOnly={editing} placeholder="52:54:00:12:34:56" suggestions={free.map((i) => ({ value: i.mac, hint: i.vm }))} />
          </Field>
          <Field label="IP address">
            <input value={f.ip} onChange={(e) => setF({ ...f, ip: e.target.value })} required placeholder={n.address ? n.address.replace(/\.\d+\/\d+$/, ".50") : ""} />
          </Field>
        </div>
        <Field label="Host name (optional)">
          <input value={f.name} onChange={(e) => setF({ ...f, name: e.target.value })} />
        </Field>
        <ErrorBox error={act.error} />
        <div className="actions">
          <button className="primary" disabled={act.busy}>
            {editing ? "Save reservation" : "Add reservation"}
          </button>
          {editing && (
            <button
              type="button"
              onClick={() => {
                setEditing(false);
                setF({ mac: "", ip: "", name: "" });
              }}
            >
              Cancel
            </button>
          )}
        </div>
      </form>
    </div>
  );
}

function XmlEditor({ url, n, onSaved }: { url: string; n: Network; onSaved: () => void }) {
  const orig = n.xml ?? "";
  const [text, setText] = useState(orig);
  const [note, setNote] = useState("");
  const act = useAction();
  const dirty = text !== orig;
  const [wasDirty, setWasDirty] = useState(false);
  useEffect(() => {
    // Follow changes made elsewhere unless there are unsaved edits.
    if (!wasDirty) setText(orig);
  }, [orig, wasDirty]);
  useEffect(() => setWasDirty(dirty), [dirty]);
  const save = async () => {
    setNote("");
    let res: { note?: string } = {};
    if (await act.run(async () => (res = await put(url + "/xml", { xml: text })))) {
      setNote(res.note ?? "Saved.");
      onSaved();
    }
  };
  return (
    <div className="card" style={{ gridColumn: "1 / -1" }}>
      <h3>Definition (XML)</h3>
      <small>
        This is the saved definition. {n.active ? "The network is running, so edits take effect after it is stopped and started again." : "Edits apply when the network is started."} The name and UUID cannot be changed.
      </small>
      <textarea spellCheck={false} rows={16} value={text} onChange={(e) => setText(e.target.value)} style={{ marginTop: 8 }} />
      <ErrorBox error={act.error} />
      {note && <StatusLine status="info">{note}</StatusLine>}
      <div className="actions" style={{ marginTop: 8 }}>
        <button className="primary" disabled={act.busy || !dirty} onClick={save}>
          Save XML
        </button>
        <button disabled={!dirty} onClick={() => setText(orig)}>
          Revert
        </button>
      </div>
    </div>
  );
}

const modes: Record<string, string> = {
  nat: "NAT: VMs reach the outside through the node",
  isolated: "Isolated: VMs can only talk to each other and the node",
  routed: "Routed: VMs reach the LAN without address translation",
  open: "Open: like routed, but libvirt adds no firewall rules",
  bridge: "Bridge: attach to an existing bridge on the node",
  xml: "Other (libvirt XML)",
};

function CreateNetwork({ node: initialNode, nodes, onClose, onDone }: { node: number; nodes?: NodeStatus[]; onClose: () => void; onDone: (node: number, name: string) => void }) {
  const [node, setNode] = useState(initialNode);
  const base = `/nodes/${node}/networks`;
  const [mode, setMode] = useState("nat");
  const [f, setF] = useState({ name: "", bridgeName: "", address: "", dhcp: true, dhcpStart: "", dhcpEnd: "", domain: "", ovs: false, vlan: "", xml: "" });
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const act = useAction();
  const routed = mode === "nat" || mode === "routed" || mode === "open" || mode === "isolated";
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body =
      mode === "xml"
        ? { xml: f.xml }
        : mode === "bridge"
          ? { name: f.name, mode, bridgeName: f.bridgeName, ovs: f.ovs, vlan: f.ovs && f.vlan ? Number(f.vlan) : 0 }
          : { name: f.name, mode, bridgeName: f.bridgeName, address: f.address, dhcp: f.dhcp && f.address !== "", dhcpStart: f.dhcpStart, dhcpEnd: f.dhcpEnd, domain: f.domain };
    let res: { name: string } = { name: "" };
    if (await act.run(async () => (res = await post(base, body)))) {
      onDone(node, res.name);
      onClose();
    }
  };
  return (
    <Modal title="New virtual network" onClose={onClose}>
      <form onSubmit={submit}>
        {nodes && (
          <Field label="Node">
            <NodePicker nodes={nodes} value={node} onChange={setNode} />
          </Field>
        )}
        <Field label="Type">
          <Select label="Type" value={mode} onChange={setMode} options={Object.entries(modes).map(([k, v]) => ({ value: k, label: v }))} />
        </Field>
        {mode === "xml" ? (
          <Field label="Network definition" hint="Checked against libvirt's schema. A network with an existing name is refused.">
            <textarea spellCheck={false} rows={12} value={f.xml} onChange={set("xml")} required placeholder="Network definition (XML)" autoFocus />
          </Field>
        ) : (
          <>
            <Field label="Name">
              <input value={f.name} onChange={set("name")} required autoFocus />
            </Field>
            {routed && (
              <>
                <div className="grid2">
                  <Field label={mode === "isolated" ? "Address (optional)" : "Address of the node on this network"} hint="Gateway address with prefix length. The subnet must not overlap another network.">
                    <input value={f.address} onChange={set("address")} required={mode !== "isolated"} placeholder="10.77.1.1/24" />
                  </Field>
                  <Field label="Bridge name (optional)" hint="Created by libvirt; automatic if empty.">
                    <input value={f.bridgeName} onChange={set("bridgeName")} placeholder="virbr1" maxLength={15} />
                  </Field>
                </div>
                <label className="check">
                  <input type="checkbox" checked={f.dhcp} disabled={f.address === ""} onChange={(e) => setF({ ...f, dhcp: e.target.checked })} /> Hand out addresses with DHCP
                </label>
                {f.dhcp && f.address !== "" && (
                  <div className="grid2">
                    <Field label="DHCP range start" hint="Defaults to the first free address.">
                      <input value={f.dhcpStart} onChange={set("dhcpStart")} placeholder="10.77.1.2" />
                    </Field>
                    <Field label="DHCP range end">
                      <input value={f.dhcpEnd} onChange={set("dhcpEnd")} placeholder="10.77.1.254" />
                    </Field>
                  </div>
                )}
                <Field label="DNS domain (optional)">
                  <input value={f.domain} onChange={set("domain")} placeholder="lab.example" />
                </Field>
              </>
            )}
            {mode === "bridge" && (
              <>
                <Field label="Bridge on the node" hint="An existing bridge made outside libvirt, for example br0. Addresses come from the network behind it.">
                  <input value={f.bridgeName} onChange={set("bridgeName")} required placeholder="br0" maxLength={15} />
                </Field>
                <label className="check">
                  <input type="checkbox" checked={f.ovs} onChange={(e) => setF({ ...f, ovs: e.target.checked })} /> The bridge is an Open vSwitch bridge
                </label>
                {f.ovs ? (
                  <Field label="VLAN tag (optional)" hint="VMs on this network are put on this access VLAN (1-4094).">
                    <input type="number" min={1} max={4094} value={f.vlan} onChange={set("vlan")} />
                  </Field>
                ) : (
                  <div className="note" style={{ marginTop: 10 }}>
                    VLAN tags are applied by libvirt only on Open vSwitch bridges. For a Linux bridge, create a VLAN sub-interface (for example eth0.20) on the node, put it in its own bridge and attach that bridge here. The host's network configuration is not managed by this tool.
                  </div>
                )}
              </>
            )}
          </>
        )}
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Create
          </button>
        </footer>
      </form>
    </Modal>
  );
}
