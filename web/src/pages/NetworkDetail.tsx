// A network's detail (DHCP leases, reservations, XML) and the actions on a network, used by the Network tab.
import { FormEvent, useEffect, useRef, useState } from "react";
import { del, DhcpHost, get, Network, post, put } from "../api";
import { orEvents, useLive } from "../live";
import { useRole } from "../auth";
import ComboInput from "../ComboInput";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import { StatusLine } from "../Checks";

const modeNames: Record<string, string> = { nat: "NAT", route: "Routed", open: "Open", isolated: "Isolated", bridge: "Bridge" };
/** The libvirt forward mode in words. */
export const modeName = (forward: string) => modeNames[forward] ?? forward;
const modeLabel = (n: Network) => modeName(n.forward) + (n.vlan ? ` · VLAN ${n.vlan}` : "");

/** The network a menu entry acts on: `attached` are the VM interfaces on it, for the warnings. */
export interface NetTarget {
  node: number;
  name: string;
  active: boolean;
  autostart: boolean;
  attached: { vm: string; running: boolean }[];
}

/** Start, stop, autostart and delete of a network, with the same confirmations as before. `dialog` must be rendered. */
export function useNetworkActions(reload: () => void) {
  const act = useAction();
  const confirm = useConfirm();
  const run = async (n: NetTarget, body: object) => {
    if (await act.run(() => post(`/nodes/${n.node}/networks/${encodeURIComponent(n.name)}/action`, body))) reload();
  };
  return {
    busy: act.busy,
    error: act.error,
    dialog: confirm.dialog,
    start: (n: NetTarget) => run(n, { action: "start" }),
    autostart: (n: NetTarget) => run(n, { action: n.autostart ? "noautostart" : "autostart" }),
    stop: async (n: NetTarget) => {
      const running = [...new Set(n.attached.filter((i) => i.running).map((i) => i.vm))];
      const ok = await confirm.ask({
        title: `Stop network ${n.name}`,
        message: running.length
          ? `The network ${n.name} stops. The running VMs below are attached to it and lose their connectivity.`
          : `The network ${n.name} stops. VMs that use it lose their connection until it is started again.`,
        items: running,
        confirm: "Stop network",
      });
      if (ok) return run(n, running.length ? { action: "stop", force: true } : { action: "stop" });
    },
    remove: async (n: NetTarget) => {
      const attached = [...new Set(n.attached.map((i) => i.vm))];
      const ok = await confirm.ask({
        title: `Delete network ${n.name}`,
        message: `The network ${n.name} is deleted.` + (attached.length ? " The VMs below are still attached to it and will not start until you give them another network." : ""),
        items: attached,
        confirm: "Delete network",
      });
      if (ok) return run(n, { action: "delete", force: n.active || attached.length > 0 });
    },
  };
}

/** The detail of one network in a dialog: leases, reservations, the definition. Read only for a viewer. */
export function NetworkDetailDialog({ node, name, onClose }: { node: number; name: string; onClose: () => void }) {
  return (
    <Modal title={`Network ${name}`} onClose={onClose} wide>
      <Detail base={`/nodes/${node}/networks`} name={name} />
    </Modal>
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
      <div className="actions">
        <button onClick={d.reload}>Refresh</button>
      </div>
      <ErrorBox error={d.error} />
      {n && (
        <fieldset className="plain" disabled={viewer}>
        <div className="cards">
          <div className="card">
            <h3>Details</h3>
            <dl>
              <dt>Mode</dt>
              <dd>{modeLabel(n)}</dd>
              <dt>Bridge</dt>
              <dd className="mono">{n.bridge || "-"}</dd>
              <dt>Address</dt>
              <dd className="mono">{n.address || "none (layer 2 only)"}</dd>
              <dt>Starts with host</dt>
              <dd>{n.autostart ? "Yes" : "No"}</dd>
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
            <details className="cloudinit">
              <summary>Details</summary>
              <dl>
                <dt>UUID</dt>
                <dd className="mono">{n.uuid}</dd>
                <dt>Definition</dt>
                <dd>{n.persistent ? "Persistent" : "Transient (gone after it stops)"}</dd>
                {n.ovs && (
                  <>
                    <dt>Switch</dt>
                    <dd>Open vSwitch</dd>
                  </>
                )}
              </dl>
            </details>
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
    <details className="card" style={{ gridColumn: "1 / -1" }}>
      <summary>
        <h3 style={{ display: "inline" }}>Definition (XML)</h3>
      </summary>
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
    </details>
  );
}
