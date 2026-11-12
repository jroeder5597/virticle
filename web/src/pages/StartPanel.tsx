import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { get } from "../api";
import { useRole } from "../auth";
import { StatusLine } from "../Checks";
import PrepareNode from "./PrepareNode";

interface LocalNode {
  capable: boolean;
  kvmDevice: boolean;
  sshReachable: boolean;
  address: string;
  hostname: string;
  registered: boolean;
  reason?: string;
}

/**
 * Shown on the Datacenter summary while no node is registered. Many people install the controller on the machine that will run their
 * virtual machines, so an administrator is offered to set up this machine as the first node. Not a modal, no setting: it is simply here
 * until there is a node.
 */
export default function StartPanel() {
  const admin = useRole() === "admin";
  const nav = useNavigate();
  const [local, setLocal] = useState<LocalNode | undefined>();
  const [preparing, setPreparing] = useState(false);
  useEffect(() => {
    if (admin) get<LocalNode>("/local-node").then(setLocal, () => {});
  }, [admin]);
  if (!admin) return <p className="muted">No nodes yet. An administrator can add one.</p>;
  if (!local) return null;
  const offer = local.capable && !local.registered;
  return (
    <div className="startpanel">
      <p className="muted">No nodes yet.</p>
      {offer ? (
        <>
          <div className="actions">
            <button className="primary" disabled={!local.sshReachable} onClick={() => setPreparing(true)}>
              Set up this machine as a node
            </button>
            <button onClick={() => nav("/datacenter/nodes")}>Add another machine</button>
          </div>
          <p className="muted">Virtual machines will run on this machine ({local.hostname}).</p>
          {!local.sshReachable && (
            <StatusLine status="warning">SSH is not running on this machine. Start it (for example: sudo systemctl enable --now sshd) and reload.</StatusLine>
          )}
        </>
      ) : (
        <>
          {local.reason && <p className="muted">{local.reason}</p>}
          <div className="actions">
            <button className="primary" onClick={() => nav("/datacenter/nodes")}>
              Add a node
            </button>
          </div>
        </>
      )}
      {preparing && <PrepareNode own addr={local.address} jump="" onClose={() => setPreparing(false)} onRegistered={() => setPreparing(false)} />}
    </div>
  );
}
