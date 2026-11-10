import { useEffect, useState } from "react";
import { api, del, get, VMDetail } from "../api";
import { ErrorBox, Modal, useAction } from "../ui";

interface Dev {
  kind: "pci" | "usb";
  id: string;
  nodedev: string;
  class?: string;
  category?: string;
  driver?: string;
  vendor: string;
  product: string;
  vendorId: string;
  productId: string;
  iommuGroup?: number;
  groupPeers?: { id: string; name?: string }[];
  usedBy: { uuid: string; name: string; state: string }[];
  unsafe: boolean;
  unsafeReason?: string;
  warnings?: string[];
}
interface Listing {
  devices: Dev[];
  attached: { key: string; kind: string; label: string; present: boolean }[];
  iommu: boolean;
  factsKnown: boolean;
  vm: string;
  warning: string;
}
interface Result {
  note?: string;
  needsRestart: boolean;
}

/** Host PCI and USB devices of the node, and those passed through to this VM. */
export default function PassthroughCard({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const [l, setL] = useState<Listing | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [picking, setPicking] = useState(false);
  const [msg, setMsg] = useState<string | undefined>();
  const act = useAction();
  const load = () => get<Listing>(`${base}/hostdevs`).then((x) => { setL(x); setErr(undefined); }, (e) => setErr(String(e.message ?? e)));
  useEffect(() => {
    load();
  }, [base, d.state]);
  const remove = async (key: string) => {
    if (!window.confirm("Remove this device from the VM? A running guest has to let go of it first.")) return;
    await act.run(async () => {
      const r = await del<Result>(`${base}/hostdevs/${encodeURIComponent(key)}`);
      setMsg(r.note);
      await load();
      onChanged();
    });
  };
  return (
    <section className="card" style={{ gridColumn: "1 / -1" }}>
      <h3>Passthrough devices (PCI, GPU, USB)</h3>
      <div className="note">{l?.warning ?? "A VM with a passed-through device cannot be live-migrated and is tied to this node until the device is removed."}</div>
      <ErrorBox error={err ?? act.error} />
      {msg && <div className="muted">{msg}</div>}
      {l && l.attached.length === 0 && <p className="muted">No host device is passed through to this VM.</p>}
      {l && l.attached.length > 0 && (
        <table className="compact">
          <tbody>
            {l.attached.map((a) => (
              <tr key={a.key}>
                <td>{a.kind.toUpperCase()}</td>
                <td>{a.label}</td>
                <td>{!a.present && <span className="badge s-offline">not on this node now</span>}</td>
                <td className="actions">
                  <button className="danger" disabled={act.busy} onClick={() => remove(a.key)}>
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <footer style={{ justifyContent: "flex-start" }}>
        <button onClick={() => { setPicking(true); load(); }}>Add host device…</button>
      </footer>
      {picking && l && (
        <Picker
          l={l}
          vm={l.vm}
          busy={act.busy}
          onClose={() => setPicking(false)}
          onAttach={async (id) => {
            const ok = await act.run(async () => {
              const r = await api<Result>("POST", `${base}/hostdevs`, { id });
              setMsg(r.note);
              await load();
              onChanged();
            });
            if (ok) setPicking(false);
          }}
          error={act.error}
        />
      )}
    </section>
  );
}

function Picker({ l, vm, busy, onClose, onAttach, error }: { l: Listing; vm: string; busy: boolean; onClose: () => void; onAttach: (id: string) => void; error?: string }) {
  const [filter, setFilter] = useState("all");
  // Bridges are never passable and there are many; they go last.
  const rank = (x: Dev) => (x.category === "bridge" ? 2 : x.unsafe ? 1 : 0);
  const shown = l.devices
    .filter((x) => filter === "all" || (filter === "usb" ? x.kind === "usb" : filter === "pci" ? x.kind === "pci" : x.category === filter))
    .sort((a, b) => rank(a) - rank(b));
  const status = (x: Dev) => {
    const other = x.usedBy.find((u) => u.uuid !== vm);
    if (x.usedBy.some((u) => u.uuid === vm)) return { text: "attached to this VM", ok: false };
    if (other) return { text: `in use by ${other.name}`, ok: false };
    if (x.unsafe) return { text: "refused: " + x.unsafeReason, ok: false, bad: true };
    if (x.kind === "pci" && x.iommuGroup === undefined) return { text: "no IOMMU group on this node", ok: false };
    return { text: "available", ok: true };
  };
  return (
    <Modal title="Host devices of this node" onClose={onClose} wide>
      <div className="note">{l.warning}</div>
      {!l.iommu && (
        <div className="error">
          This node has no active IOMMU, so no PCI device can be passed through (enable intel_iommu=on or amd_iommu=on and reboot). USB devices are not affected.
        </div>
      )}
      {!l.factsKnown && <div className="error">The node's network and storage layout could not be read; devices that might belong to the host are refused.</div>}
      <div className="tabs" style={{ margin: "8px 0" }}>
        {[["all", "All"], ["gpu", "GPU"], ["network", "Network"], ["storage", "Storage"], ["usb-controller", "USB controllers"], ["usb", "USB devices"]].map(([k, name]) => (
          <button key={k} className={filter === k ? "on" : ""} onClick={() => setFilter(k)}>
            {name}
          </button>
        ))}
      </div>
      <div className="devlist">
        <table className="compact">
          <thead>
            <tr>
              <th>Device</th>
              <th>Class</th>
              <th>Driver</th>
              <th>IOMMU group</th>
              <th>Status</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {shown.map((x) => {
              const st = status(x);
              return (
                <tr key={x.id} className={st.bad ? "blockedrow" : ""}>
                  <td>
                    <div>
                      {x.vendor} {x.product}
                    </div>
                    <code>{x.id}</code> <small style={{ display: "inline" }}>{x.vendorId}:{x.productId}</small>
                  </td>
                  <td>{x.class}</td>
                  <td>{x.driver}</td>
                  <td>
                    {x.iommuGroup !== undefined ? x.iommuGroup : "—"}
                    {x.groupPeers && x.groupPeers.length > 0 && (
                      <small title={x.groupPeers.map((p) => `${p.id} ${p.name ?? ""}`).join("\n")}>with {x.groupPeers.map((p) => p.id).join(", ")}</small>
                    )}
                  </td>
                  <td>
                    <span className={st.bad ? "rowerror" : st.ok ? "" : "muted"}>{st.text}</span>
                    {x.warnings?.map((w, i) => (
                      <small key={i}>{w}</small>
                    ))}
                  </td>
                  <td className="actions">
                    <button disabled={busy || !st.ok} onClick={() => onAttach(x.id)}>
                      Attach
                    </button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      <ErrorBox error={error} />
      <footer>
        <button onClick={onClose}>Close</button>
      </footer>
    </Modal>
  );
}
