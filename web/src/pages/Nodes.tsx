import { FormEvent, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Alert, bytes, del, get, NodeInfo, NodeStatus, post, put } from "../api";
import { useLive } from "../live";
import { useRole } from "../auth";
import { ErrorBox, Field, Modal, StateBadge, useAction } from "../ui";
import { HostLoad, useHostSeries } from "../charts";
import { AlertMarks, useAlerts } from "./Alerts";
import Evacuate from "./Evacuate";
import HostUpdates from "./HostUpdates";
import MigrationTLS from "./MigrationTLS";

export default function Nodes() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[]; publicKey: string }>("/nodes"), "nodes");
  const [adding, setAdding] = useState(false);
  const act = useAction();
  const admin = useRole() === "admin";
  const alerts = useAlerts();
  const [evacuating, setEvacuating] = useState<NodeStatus | undefined>();
  return (
    <>
      <div className="head">
        <h1>Nodes</h1>
        {admin && (
          <button className="primary" onClick={() => setAdding(true)}>
            Add node
          </button>
        )}
      </div>
      <ErrorBox error={nodes.error ?? act.error} />
      {nodes.data && nodes.data.nodes.length === 0 ? (
        <div className="empty">A node is any machine running libvirt that this server can reach over SSH. Nothing needs to be installed on it.</div>
      ) : (
        <div className="table-scroll"><table>
          <thead>
            <tr>
              <th>Node</th>
              <th>State</th>
              <th>Address</th>
              <th>CPU</th>
              <th>Memory</th>
              <th>libvirt</th>
              <th>Host updates</th>
              <th>Encrypted migration</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {(nodes.data?.nodes ?? []).map((n) => (
              <NodeRow
                key={n.id}
                n={n}
                canRemove={admin}
                alerts={alerts}
                onEvacuate={() => setEvacuating(n)}
                onMaintenance={() => act.run(() => put(`/nodes/${n.id}/maintenance`, { on: !n.maintenance }))}
                onRemove={() =>
                  window.confirm(`Remove ${n.hostname || n.addr}? Its virtual machines keep running; they just stop being shown here.`) && act.run(() => del(`/nodes/${n.id}`))
                }
              />
            ))}
          </tbody>
        </table></div>
      )}
      {evacuating && nodes.data && <Evacuate node={evacuating} nodes={nodes.data.nodes} onClose={() => setEvacuating(undefined)} />}
      {adding && nodes.data && <AddNode publicKey={nodes.data.publicKey} onClose={() => setAdding(false)} />}
    </>
  );
}

function NodeRow({ n, onRemove, canRemove, alerts, onEvacuate, onMaintenance }: { n: NodeStatus; onRemove: () => void; canRemove: boolean; alerts: Alert[]; onEvacuate: () => void; onMaintenance: () => void }) {
  const [info, setInfo] = useState<NodeInfo | undefined>();
  const series = useHostSeries(n.id, n.state === "online");
  useEffect(() => {
    setInfo(undefined);
    if (n.state === "online") get<NodeInfo>(`/nodes/${n.id}`).then(setInfo, () => {});
  }, [n.id, n.state]);
  return (
    <tr>
      <td>
        <Link to={`/node/${n.id}`}>{n.hostname || n.addr}</Link>
        {n.maintenance && (
          <span className="badge maint" title="Never chosen as a migration or evacuation target">
            maintenance
          </span>
        )}
        <AlertMarks alerts={alerts} match={(a) => a.node === n.id && a.kind !== "pool-usage" && a.kind !== "vm-state"} />
      </td>
      <td>
        <StateBadge state={n.state} />
        {n.error && <div className="rowerror">{n.error}</div>}
      </td>
      <td className="mono">
        {n.user}@{n.addr}
        {n.jump ? ` via ${n.jump}` : ""}
      </td>
      <td>
        {info ? `${info.cpus} × ${info.cpuModel}` : ""}
        <HostLoad points={series} kind="cpu" />
      </td>
      <td>
        {info ? `${bytes(info.memoryKiB * 1024)} total` : ""}
        <HostLoad points={series} kind="mem" />
      </td>
      <td>{info?.libvirtVersion}</td>
      <td>
        <HostUpdates node={n.id} online={n.state === "online"} />
      </td>
      <td>
        <MigrationTLS node={n.id} online={n.state === "online"} admin={canRemove} />
      </td>
      <td className="actions nodeactions">
        {canRemove && n.state === "online" && (
          <button onClick={onEvacuate} title="Move the virtual machines of this node to the others">
            Evacuate…
          </button>
        )}
        {canRemove && (
          <button onClick={onMaintenance} title="A node in maintenance is never chosen as a migration or evacuation target">
            {n.maintenance ? "End maintenance" : "Maintenance"}
          </button>
        )}
        {canRemove && (
          <button className="danger" onClick={onRemove}>
            Remove
          </button>
        )}
      </td>
    </tr>
  );
}

function AddNode({ publicKey, onClose }: { publicKey: string; onClose: () => void }) {
  const [addr, setAddr] = useState("");
  const [user, setUser] = useState("root");
  const [jump, setJump] = useState("");
  const [copied, setCopied] = useState(false);
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await act.run(() => post("/nodes", { addr, user, jump }))) onClose();
  };
  return (
    <Modal title="Add node" onClose={onClose}>
      <form onSubmit={submit}>
        <p>
          First allow this server to sign in: add its public key to <code>~/.ssh/authorized_keys</code> for the user below on the node.
        </p>
        <div className="keybox">
          <code>{publicKey}</code>
          <button type="button" className="link" onClick={() => navigator.clipboard?.writeText(publicKey).then(() => setCopied(true))}>
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
        <div className="grid2">
          <Field label="Address" hint="Host name or IP, optionally with :port.">
            <input value={addr} onChange={(e) => setAddr(e.target.value)} required autoFocus placeholder="kvm1.example.lan" />
          </Field>
          <Field label="SSH user" hint="root, or a member of the libvirt group.">
            <input value={user} onChange={(e) => setUser(e.target.value)} required />
          </Field>
        </div>
        <Field label="Jump host (optional)" hint="user@address of a host to connect through.">
          <input value={jump} onChange={(e) => setJump(e.target.value)} placeholder="" />
        </Field>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Add
          </button>
        </footer>
      </form>
    </Modal>
  );
}
