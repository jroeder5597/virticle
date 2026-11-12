import { useState } from "react";
import { get, NodeStatus, put, VM } from "../api";
import { ErrorBox, Modal, useAction } from "../ui";
import Evacuate from "./Evacuate";

/**
 * The one Maintenance action of a node. Switching it on offers, in the same step, to move the node's virtual machines to other nodes
 * (when it has any). A node that is already in maintenance shows End maintenance, and Evacuate while it is online.
 */
export default function MaintenanceActions({ n, nodes }: { n: NodeStatus; nodes: NodeStatus[] }) {
  const act = useAction();
  const [asking, setAsking] = useState<number | undefined>(); // number of VMs, set while the dialog is open
  const [move, setMove] = useState(true);
  const [evacuating, setEvacuating] = useState(false);
  const name = n.hostname || n.addr;
  const hasTarget = nodes.some((o) => o.id !== n.id && o.state === "online" && !o.maintenance);
  const set = (on: boolean) => act.run(() => put(`/nodes/${n.id}/maintenance`, { on }));
  const start = async () => {
    // Without VMs (or when they cannot be read) there is nothing to offer.
    let count = 0;
    if (n.state === "online" && hasTarget) {
      try {
        count = (await get<VM[]>("/vms")).filter((v) => v.node === n.id && !v.template).length;
      } catch {
        count = 0;
      }
    }
    if (count === 0) {
      await set(true);
      return;
    }
    setMove(true);
    setAsking(count);
  };
  const confirm = async () => {
    if (await set(true)) {
      setAsking(undefined);
      if (move) setEvacuating(true);
    }
  };
  return (
    <>
      {n.maintenance && n.state === "online" && (
        <button onClick={() => setEvacuating(true)} title="Move the virtual machines of this node to the others">
          Evacuate
        </button>
      )}
      <button onClick={() => (n.maintenance ? set(false) : start())} disabled={act.busy} title="A node in maintenance is never chosen as a migration or evacuation target">
        {n.maintenance ? "End maintenance" : "Maintenance"}
      </button>
      {act.error && asking === undefined && <ErrorBox error={act.error} />}
      {asking !== undefined && (
        <Modal title={`Maintenance on ${name}`} onClose={() => setAsking(undefined)}>
          <p>A node in maintenance is never chosen as a migration or evacuation target.</p>
          <label className="check">
            <input type="checkbox" checked={move} onChange={(e) => setMove(e.target.checked)} /> Move its virtual machines to other nodes ({asking})
          </label>
          <ErrorBox error={act.error} />
          <footer>
            <button onClick={() => setAsking(undefined)}>Cancel</button>
            <button className="primary" disabled={act.busy} onClick={confirm}>
              Start maintenance
            </button>
          </footer>
        </Modal>
      )}
      {evacuating && <Evacuate node={n} nodes={nodes} onClose={() => setEvacuating(false)} />}
    </>
  );
}
