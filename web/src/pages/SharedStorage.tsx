import { FormEvent, useState } from "react";
import { bytes, del, get, NodeStatus, post } from "../api";
import { useLiveDebounced } from "../liveDebounced";
import DialogAdvanced from "./DialogAdvanced";
import { nodeLabel, poolEvents } from "./dcShared";
import { ErrorBox, Field, Modal, StateBadge, UsageBar, useAction, useConfirm } from "../ui";

export interface SharedMember {
  node: number;
  nodeName: string;
  state?: string;
  autostart?: boolean;
}
export interface SharedGroup {
  name: string;
  server: string;
  export: string;
  target: string;
  nodes: SharedMember[];
  missing: SharedMember[];
  differ: SharedMember[];
  state: string;
  capacity: number;
  allocation: number;
  available: number;
  detail?: string;
}
interface AddResult {
  complete: boolean;
  message: string;
  note?: string;
}

/**
 * Shared storage: NFS shares that every node mounts at the same path, so a VM can move between nodes without its
 * disks being copied. One table row per share, however many nodes have it.
 */
export function SharedStorage({ nodes, adding, onAddClose }: { nodes: NodeStatus[]; adding: boolean; onAddClose: () => void }) {
  const data = useLiveDebounced(() => get<{ shared: SharedGroup[] }>("/storage/shared"), "storage/shared", poolEvents);
  const act = useAction();
  const confirm = useConfirm();
  const groups = (data.data?.shared ?? []).map((g) => ({ ...g, nodes: g.nodes ?? [], missing: g.missing ?? [], differ: g.differ ?? [] }));
  const remove = async (g: SharedGroup) => {
    const ok = await confirm.ask({
      title: `Remove ${g.name}`,
      message: `${g.name} is unmounted on ${g.nodes.map((n) => n.nodeName).join(", ")} and forgotten there. The files on the server are not deleted. It is refused while a VM still has a disk on it.`,
      confirm: "Remove shared storage",
    });
    if (ok && (await act.run(() => del(`/storage/shared/${encodeURIComponent(g.name)}`)))) data.reload();
  };
  const addTo = (g: SharedGroup) => act.run(() => post<AddResult>("/storage/shared", { name: g.name, server: g.server, path: g.export, nodes: g.missing.map((m) => m.node) })).then(() => data.reload());
  return (
    <>
      {groups.length > 0 && (
        <section aria-label="Shared storage">
          <div className="head sub">
            <h2>Shared storage</h2>
          </div>
          <ErrorBox error={data.error ?? act.error} />
          <table className="dctable">
            <thead>
              <tr>
                <th>Name</th>
                <th>Server and export</th>
                <th>Nodes</th>
                <th>State</th>
                <th>Usage</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => (
                <tr key={g.name}>
                  <td title={g.target}>{g.name}</td>
                  <td className="mono">
                    {g.server}:{g.export}
                  </td>
                  <td>
                    {g.nodes.map((n) => n.nodeName).join(", ")}
                    {g.missing.length > 0 && (
                      <div className="warnline">
                        Not on {g.missing.map((n) => n.nodeName).join(", ")}: VMs cannot move there without copying their disks.
                      </div>
                    )}
                    {g.differ.length > 0 && (
                      <div className="warnline">
                        {g.differ.map((n) => n.nodeName).join(", ")} has a storage named {g.name} for another server or export. Remove {g.name} and add it again, so every node uses the same server name or address.
                      </div>
                    )}
                  </td>
                  <td>
                    <StateBadge state={g.state} />
                    {g.state === "unavailable" && g.detail && <div className="muted">{g.detail}</div>}
                  </td>
                  <td>
                    {g.state === "running" && (
                      <>
                        <UsageBar used={g.allocation} total={g.capacity} />
                        <small className="muted">
                          {bytes(g.available)} free of {bytes(g.capacity)}
                        </small>
                      </>
                    )}
                  </td>
                  <td className="actions">
                    {g.missing.length > 0 && (
                      <button disabled={act.busy} onClick={() => addTo(g)}>
                        Add to {g.missing.map((n) => n.nodeName).join(", ")}
                      </button>
                    )}
                    <button className="danger" disabled={act.busy} onClick={() => remove(g)}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
      {confirm.dialog}
      {adding && <AddShared nodes={nodes} onClose={onAddClose} onDone={data.reload} />}
    </>
  );
}

function AddShared({ nodes, onClose, onDone }: { nodes: NodeStatus[]; onClose: () => void; onDone: () => void }) {
  const [f, setF] = useState({ name: "", server: "", path: "" });
  const online = nodes.filter((n) => n.state === "online");
  const [left, setLeft] = useState<Record<number, boolean>>({}); // nodes the user left out
  const [result, setResult] = useState<AddResult | undefined>();
  const act = useAction();
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value.trim() });
  const chosen = online.filter((n) => !left[n.id]);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let res: AddResult | undefined;
    const ok = await act.run(async () => {
      res = await post<AddResult>("/storage/shared", { ...f, nodes: chosen.length === online.length ? [] : chosen.map((n) => n.id) });
    });
    if (!ok || !res) return;
    onDone();
    if (res.complete && !res.note) onClose();
    else setResult(res);
  };
  return (
    <Modal title="Add shared storage" onClose={onClose}>
      <form onSubmit={submit}>
        <p className="muted">
          Mounts an NFS share on {online.length > 1 ? "every node" : "the node"} under the same name and path, so VMs on it can move between nodes without copying their disks.
        </p>
        <Field label="Name" hint="Also the folder it is mounted in: /mnt/name.">
          <input value={f.name} onChange={set("name")} required autoFocus placeholder="shared" />
        </Field>
        <div className="grid2">
          <Field label="NFS server">
            <input value={f.server} onChange={set("server")} required placeholder="nas.example.lan" />
          </Field>
          <Field label="Export path">
            <input value={f.path} onChange={set("path")} required placeholder="/export/vms" />
          </Field>
        </div>
        {online.length > 1 && (
          <DialogAdvanced>
            <strong>Add to these nodes</strong>
            {online.map((n) => (
              <label key={n.id} className="check">
                <input type="checkbox" checked={!left[n.id]} onChange={(e) => setLeft({ ...left, [n.id]: !e.target.checked })} />
                {nodeLabel(n)}
              </label>
            ))}
            <small className="muted">A VM can only move between nodes that all have the storage.</small>
          </DialogAdvanced>
        )}
        <ErrorBox error={act.error} />
        {result && !result.complete && <ErrorBox error={result.message} />}
        {result?.complete && result.note && (
          <div className="note">
            <strong>{result.message}.</strong> {result.note}
          </div>
        )}
        <footer>
          <button type="button" onClick={onClose}>
            {result ? "Close" : "Cancel"}
          </button>
          {!result?.complete && (
            <button className="primary" disabled={act.busy || chosen.length === 0}>
              {result ? "Add again" : "Add shared storage"}
            </button>
          )}
        </footer>
      </form>
    </Modal>
  );
}
