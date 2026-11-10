import { FormEvent, useEffect, useState } from "react";
import { bytes, del, get, NodeInfo, NodeStatus, post } from "../api";
import { useLive } from "../live";
import { ErrorBox, Field, Modal, StateBadge, useAction } from "../ui";
import { HostLoad, useHostSeries } from "../charts";

export default function Nodes() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[]; publicKey: string }>("/nodes"), "nodes");
  const [adding, setAdding] = useState(false);
  const act = useAction();
  return (
    <>
      <div className="head">
        <h1>Nodes</h1>
        <button className="primary" onClick={() => setAdding(true)}>
          Add node
        </button>
      </div>
      <ErrorBox error={nodes.error ?? act.error} />
      {nodes.data && nodes.data.nodes.length === 0 ? (
        <div className="empty">A node is any machine running libvirt that this server can reach over SSH. Nothing needs to be installed on it.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Node</th>
              <th>State</th>
              <th>Address</th>
              <th>CPU</th>
              <th>Memory</th>
              <th>libvirt</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {(nodes.data?.nodes ?? []).map((n) => (
              <NodeRow
                key={n.id}
                n={n}
                onRemove={() =>
                  window.confirm(`Remove ${n.hostname || n.addr}? Its virtual machines keep running; they just stop being shown here.`) && act.run(() => del(`/nodes/${n.id}`))
                }
              />
            ))}
          </tbody>
        </table>
      )}
      {adding && nodes.data && <AddNode publicKey={nodes.data.publicKey} onClose={() => setAdding(false)} />}
    </>
  );
}

function NodeRow({ n, onRemove }: { n: NodeStatus; onRemove: () => void }) {
  const [info, setInfo] = useState<NodeInfo | undefined>();
  const series = useHostSeries(n.id, n.state === "online");
  useEffect(() => {
    setInfo(undefined);
    if (n.state === "online") get<NodeInfo>(`/nodes/${n.id}`).then(setInfo, () => {});
  }, [n.id, n.state]);
  return (
    <tr>
      <td>{n.hostname || n.addr}</td>
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
      <td className="actions">
        <button className="danger" onClick={onRemove}>
          Remove
        </button>
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
