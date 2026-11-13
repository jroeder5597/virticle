import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Alert, bytes, get, NodeInfo, NodeStatus, RollRun } from "../api";
import { orEvents, useLive } from "../live";
import { useRole } from "../auth";
import { ErrorBox, PageTitle, StateBadge } from "../ui";
import { HostLoad, useHostSeries } from "../charts";
import { AlertMarks, useAlerts } from "./Alerts";
import MaintenanceActions from "./Maintenance";
import { StatusLine } from "../Checks";
import PrepareNode from "./PrepareNode";
import RollingUpdate, { needsAttention } from "./RollingUpdate";

export default function Nodes() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[]; publicKey: string }>("/nodes"), "nodes");
  const [adding, setAdding] = useState(false);
  const [updating, setUpdating] = useState(false);
  const admin = useRole() === "admin";
  const alerts = useAlerts();
  const roll = useLive(() => get<{ run: RollRun | null }>("/rolling-update"), "rolling", orEvents("rolling"));
  const attention = needsAttention(roll.data?.run);
  return (
    <>
      <div className="head">
        <PageTitle>Nodes</PageTitle>
        {admin && (
          <div className="actions">
            <button onClick={() => setUpdating(true)} disabled={(nodes.data?.nodes.length ?? 0) === 0} title="Update every node, one at a time, without stopping the virtual machines">
              Update all nodes
            </button>
            <button className="primary" onClick={() => setAdding(true)}>
              Add node
            </button>
          </div>
        )}
      </div>
      {attention && roll.data?.run && (
        <div className="rollbanner">
          <StatusLine status={roll.data.run.status === "running" ? "warning" : "error"}>
            {roll.data.run.status === "running" ? "A rolling update is running." : "The rolling update stopped. " + (roll.data.run.condition ?? "")}
          </StatusLine>
          {admin && <button onClick={() => setUpdating(true)}>Show</button>}
        </div>
      )}
      <ErrorBox error={nodes.error} />
      {nodes.data && nodes.data.nodes.length === 0 ? (
        <div className="empty">A node is a machine that runs your virtual machines. Add one to get started.</div>
      ) : (
        <div className="table-scroll"><table>
          <thead>
            <tr>
              <th>Node</th>
              <th>State</th>
              <th>CPU</th>
              <th>Memory</th>
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
                nodes={nodes.data?.nodes ?? []}
              />
            ))}
          </tbody>
        </table></div>
      )}
      {updating && <RollingUpdate title="Update all nodes" onClose={() => setUpdating(false)} />}
      {adding && <PrepareNode addr="" jump="" onClose={() => setAdding(false)} onRegistered={() => setAdding(false)} />}
    </>
  );
}

function NodeRow({ n, canRemove, alerts, nodes }: { n: NodeStatus; canRemove: boolean; alerts: Alert[]; nodes: NodeStatus[] }) {
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
            Maintenance
          </span>
        )}
        <AlertMarks alerts={alerts} match={(a) => a.node === n.id && a.kind !== "pool-usage" && a.kind !== "vm-state" && a.kind !== "storage-unavailable"} />
        <div className="mono muted nodeaddr">
          {n.user}@{n.addr}
          {n.jump && <div>via {n.jump}</div>}
        </div>
      </td>
      <td>
        <StateBadge state={n.state} />
        {n.error && <StatusLine status="error">{n.error}</StatusLine>}
      </td>
      <td>
        {info ? `${info.cpus} × ${info.cpuModel}` : ""}
        <HostLoad points={series} kind="cpu" />
      </td>
      <td>
        {info ? `${bytes(info.memoryKiB * 1024)} total` : ""}
        <HostLoad points={series} kind="mem" />
      </td>
      <td className="actions nodeactions">
        {canRemove && <MaintenanceActions n={n} nodes={nodes} />}
      </td>
    </tr>
  );
}
