import { Link, useNavigate, useParams } from "react-router-dom";
import { bytes, get, NodeStatus, VM } from "../api";
import { useLive } from "../live";
import { useLiveDebounced } from "../liveDebounced";
import { Meter, pct, useAllStats } from "../charts";
import { EmbeddedContext, ErrorBox, StateBadge } from "../ui";
import { AlertMarks, useAlerts } from "./Alerts";
import { BackupsSection } from "./Backups";
import { NodeErr, NodeErrors, nodeLabel, NodePool } from "./dcShared";
import HostInterfaces from "./HostNics";
import Networks from "./Networks";
import Nodes from "./Nodes";
import Storage from "./Storage";
import Tasks from "./Tasks";
import VMs from "./VMs";
import "../datacenter.css";

const TABS = [
  ["summary", "Summary"],
  ["vms", "Virtual machines"],
  ["storage", "Storage"],
  ["network", "Network"],
  ["backups", "Backups"],
  ["nodes", "Nodes"],
  ["tasks", "Tasks"],
] as const;
type Tab = (typeof TABS)[number][0];

/** Everything across all nodes: totals, and the lists of VMs, storage, networks, backups, nodes and tasks. */
export default function Datacenter() {
  const { tab: tabParam } = useParams();
  const nav = useNavigate();
  const tab: Tab = TABS.some(([k]) => k === tabParam) ? (tabParam as Tab) : "summary";
  return (
    <EmbeddedContext.Provider value={true}>
      <div className="head">
        <h1>Datacenter</h1>
      </div>
      <div className="tabs" role="tablist">
        {TABS.map(([k, label]) => (
          <button key={k} role="tab" aria-selected={tab === k} className={tab === k ? "on" : ""} onClick={() => nav(k === "summary" ? "/datacenter" : `/datacenter/${k}`)}>
            {label}
          </button>
        ))}
      </div>
      {tab === "summary" && <Summary />}
      {tab === "vms" && <VMs />}
      {tab === "storage" && <Storage allNodes />}
      {tab === "network" && <NetworkTab />}
      {tab === "backups" && <BackupsSection />}
      {tab === "nodes" && <Nodes />}
      {tab === "tasks" && <Tasks />}
    </EmbeddedContext.Provider>
  );
}

function NetworkTab() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  return (
    <>
      <Networks allNodes />
      {nodes.data && nodes.data.nodes.length > 0 && <HostInterfaces nodes={nodes.data.nodes} />}
    </>
  );
}

function Tile(props: { label: string; value: string; note?: string; meter?: { value: number; max: number } }) {
  return (
    <div className="card dctile">
      <div className="muted">{props.label}</div>
      <div className="dcval">{props.value}</div>
      {props.meter && <Meter value={props.meter.value} max={props.meter.max} label={props.note ?? ""} />}
      {!props.meter && props.note && <small className="muted">{props.note}</small>}
    </div>
  );
}

/** One request per data source: nodes, VMs, pools, stats and alerts, however many nodes there are. */
function Summary() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const vms = useLive(() => get<VM[]>("/vms"), "vms");
  const pools = useLiveDebounced(() => get<{ pools: NodePool[]; errors: NodeErr[] }>("/pools"), "pools/all/true", (e) => e.type === "pool" || e.type === "node");
  const stats = useAllStats();
  const alerts = useAlerts();
  const all = nodes.data?.nodes ?? [];
  const online = all.filter((n) => n.state === "online");
  const list = vms.data ?? [];
  const running = list.filter((v) => v.state === "running");
  const vcpus = running.reduce((a, v) => a + v.vcpus, 0);
  const hostCpus = all.reduce((a, n) => a + (stats?.nodes[n.id]?.cpus ?? 0), 0);
  const mem = all.reduce(
    (a, n) => {
      const h = stats?.nodes[n.id]?.host;
      return h?.memUsedKiB != null && h.memTotalKiB ? { used: a.used + h.memUsedKiB, total: a.total + h.memTotalKiB } : a;
    },
    { used: 0, total: 0 },
  );
  const live = (pools.data?.pools ?? []).filter((p) => p.state === "running");
  const sto = { used: live.reduce((a, p) => a + p.allocation, 0), total: live.reduce((a, p) => a + p.capacity, 0) };
  const dash = "–";
  return (
    <>
      <ErrorBox error={nodes.error ?? vms.error ?? pools.error} />
      <NodeErrors errors={pools.data?.errors} />
      <div className="dctiles">
        <Tile label="Nodes online" value={nodes.data ? `${online.length} of ${all.length}` : dash} />
        <Tile label="Virtual machines running" value={vms.data ? `${running.length} of ${list.length}` : dash} />
        <Tile label="vCPUs of running VMs" value={vms.data && hostCpus ? `${vcpus} of ${hostCpus}` : dash} note="host CPUs on all nodes" />
        <Tile label="Memory in use" value={mem.total ? `${bytes(mem.used * 1024)} of ${bytes(mem.total * 1024)}` : dash} meter={mem.total ? { value: mem.used, max: mem.total } : undefined} note={mem.total ? pct((mem.used / mem.total) * 100) : undefined} />
        <Tile label="Storage in use" value={pools.data ? `${bytes(sto.used)} of ${bytes(sto.total)}` : dash} meter={sto.total ? { value: sto.used, max: sto.total } : undefined} note={pools.data ? "sum of pools" : undefined} />
      </div>
      <div className="cards dccards">
        <div className="card">
          <h3>Alerts</h3>
          {alerts.length === 0 ? (
            <p className="muted">Nothing needs attention.</p>
          ) : (
            <ul className="nodealerts">
              {alerts.map((a) => (
                <li key={a.id}>
                  <AlertMarks alerts={[a]} match={() => true} />
                  <div className="muted">{a.message}</div>
                </li>
              ))}
            </ul>
          )}
        </div>
        <div className="card">
          <h3>Nodes</h3>
          {nodes.data && all.length === 0 ? (
            <p className="muted">
              No nodes yet. <Link to="/datacenter/nodes">Add a node</Link>.
            </p>
          ) : (
            <table className="compact dcnodes">
              <thead>
                <tr>
                  <th>Node</th>
                  <th>State</th>
                  <th>CPU</th>
                  <th>Memory</th>
                  <th className="num">VMs</th>
                </tr>
              </thead>
              <tbody>
                {all.map((n) => {
                  const h = stats?.nodes[n.id]?.host;
                  const mine = list.filter((v) => v.node === n.id);
                  return (
                    <tr key={n.id}>
                      <td>
                        <Link to={`/node/${n.id}`}>{nodeLabel(n)}</Link>
                      </td>
                      <td>
                        <StateBadge state={n.state} />
                      </td>
                      <td className="num">{n.state === "online" && h?.cpu != null ? pct(h.cpu) : dash}</td>
                      <td className="num">{n.state === "online" && h?.memUsedKiB != null && h.memTotalKiB ? pct((h.memUsedKiB / h.memTotalKiB) * 100) : dash}</td>
                      <td className="num">{vms.data ? `${mine.filter((v) => v.state === "running").length} of ${mine.length}` : dash}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </>
  );
}
