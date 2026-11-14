import { ReactNode, useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import ServerOff from "lucide-react/icons/server-off";
import { bytes, del, get, Network, NodeInfo, NodeStatus, Pool, VM } from "../api";
import { useLive } from "../live";
import { useRole } from "../auth";
import { ErrorBox, StateBadge, useAction, useConfirm } from "../ui";
import { HostLoad, useHostSeries } from "../charts";
import { AlertMarks, useAlerts } from "./Alerts";
import MaintenanceActions from "./Maintenance";
import HostUpdates from "./HostUpdates";
import ConnectionCard from "./NodeConnection";
import MigrationTLS from "./MigrationTLS";
import Networks from "./Networks";
import Storage from "./Storage";
import Topology from "./Topology";
import { PerformanceTab } from "./Performance";
import VMs from "./VMs";
import "../node.css";
import { StatusLine } from "../Checks";

const TABS = [
  ["summary", "Summary"],
  ["performance", "Performance"],
  ["vms", "Virtual machines"],
  ["storage", "Storage"],
  ["network", "Network"],
  ["topology", "Topology"],
  ["updates", "Updates"],
  ["settings", "Settings"],
] as const;
type Tab = (typeof TABS)[number][0];

export default function NodeDetail() {
  const { id: idParam, tab: tabParam } = useParams();
  const id = Number(idParam);
  const nav = useNavigate();
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const admin = useRole() === "admin";
  const alerts = useAlerts();
  const act = useAction();
  const confirm = useConfirm();
  const n = nodes.data?.nodes.find((x) => x.id === id);
  const tab: Tab = TABS.some(([k]) => k === tabParam) ? (tabParam as Tab) : "summary";

  if (!nodes.data) return <ErrorBox error={nodes.error} />;
  if (!n) return <div className="empty">There is nothing at this address, or your role does not allow it.</div>;

  const online = n.state === "online";
  const name = n.hostname || n.addr;
  const remove = async () => {
    if (!(await confirm.ask({ title: `Remove node ${name}`, message: `The node ${name} is removed from this controller. Its virtual machines keep running; they just stop being shown here. Replication of other virtual machines to this node is switched off, and the other nodes stop trusting it for migration and file copies. Nothing is uninstalled from the node.`, confirm: "Remove node" }))) return;
    if (await act.run(() => del(`/nodes/${n.id}`))) nav("/nodes");
  };

  return (
    <>
      <div className="head nodehead">
        <div className="nodetitle">
          <h1>{name}</h1>
          <StateBadge state={n.state} />
          {n.maintenance && (
            <span className="badge maint" title="Never chosen as a migration or evacuation target">
              Maintenance
            </span>
          )}
          <span className="mono muted">
            {n.user}@{n.addr}
          </span>
          <AlertMarks alerts={alerts} match={(a) => a.node === n.id && a.kind !== "pool-usage" && a.kind !== "vm-state" && a.kind !== "storage-unavailable"} />
        </div>
        {admin && (
          <div className="actions">
            <MaintenanceActions n={n} nodes={nodes.data.nodes} />
            <button className="danger" onClick={remove} disabled={act.busy}>
              Remove
            </button>
          </div>
        )}
      </div>
      <ErrorBox error={act.error} />
      {confirm.dialog}
      <div className="tabs" role="tablist">
        {TABS.map(([k, label]) => (
          <button key={k} role="tab" aria-selected={tab === k} className={tab === k ? "on" : ""} onClick={() => nav(`/node/${id}/${k}`)}>
            {label}
          </button>
        ))}
      </div>
      {!online && tab !== "settings" ? (
        <div className="empty nodeoffline">
          <ServerOff size={28} aria-hidden="true" />
          <p>{n.state === "connecting" ? "Connecting to this node" : "This node is offline, so nothing can be read from or changed on it."}</p>
          {n.error && <StatusLine status="error">{n.error}</StatusLine>}
        </div>
      ) : (
        <>
          {tab === "summary" && <Summary n={n} alerts={alerts} />}
          {tab === "performance" && <PerformanceTab kind="node" id={String(id)} />}
          {tab === "vms" && <VMs fixedNode={id} />}
          {tab === "storage" && <Storage fixedNode={id} />}
          {tab === "network" && <NetworkTab id={id} />}
          {tab === "topology" && <Topology node={id} />}
          {tab === "updates" && <HostUpdates node={id} online={online} />}
          {tab === "settings" && <Settings n={n} admin={admin} />}
        </>
      )}
    </>
  );
}

function Facts({ rows }: { rows: [string, ReactNode][] }) {
  return (
    <dl>
      {rows.map(([k, v]) => (
        <FactRow key={k} k={k} v={v} />
      ))}
    </dl>
  );
}
const FactRow = ({ k, v }: { k: string; v: ReactNode }) => (
  <>
    <dt>{k}</dt>
    <dd>{v}</dd>
  </>
);

function Summary({ n, alerts }: { n: NodeStatus; alerts: ReturnType<typeof useAlerts> }) {
  const [info, setInfo] = useState<NodeInfo | undefined>();
  const [err, setErr] = useState<string | undefined>();
  // Polling stops by itself when the tab is hidden or another tab is selected (usePoll, unmount).
  const series = useHostSeries(n.id, true);
  const vms = useLive(() => get<VM[]>("/vms"), "vms");
  const pools = useLive(() => get<Pool[]>(`/nodes/${n.id}/pools`), `pools/${n.id}`, (e) => e.type === "pool");
  const nets = useLive(() => get<Network[]>(`/nodes/${n.id}/networks`), `nets/${n.id}`, (e) => e.type === "network");
  useEffect(() => {
    get<NodeInfo>(`/nodes/${n.id}`).then(setInfo, (e) => setErr(String(e.message ?? e)));
  }, [n.id]);
  const mine = (vms.data ?? []).filter((v) => v.node === n.id);
  const count = (x: unknown[] | undefined, f?: (i: never) => boolean) => (x ? String(f ? x.filter(f as never).length : x.length) : "–");
  const mineAlerts = alerts.filter((a) => a.node === n.id);
  return (
    <>
      <ErrorBox error={err} />
      <div className="cards">
        <div className="card">
          <h3>Host</h3>
          <Facts
            rows={[
              ["Hostname", info?.hostname ?? "–"],
              ["CPU", info ? `${info.cpus} × ${info.cpuModel}` : "–"],
              ["Memory", info ? `${bytes(info.memoryKiB * 1024)} total, ${bytes(info.availableKiB * 1024)} available` : "–"],
              ["libvirt", info?.libvirtVersion ?? "–"],
              ["Address", <span className="mono" key="a">{n.user}@{n.addr}{n.jump ? ` via ${n.jump}` : ""}</span>],
            ]}
          />
        </div>
        <div className="card">
          <h3>Load</h3>
          <div className="nodeload">
            <HostLoad points={series} kind="cpu" named />
            <HostLoad points={series} kind="mem" named />
          </div>
        </div>
        <div className="card">
          <h3>Inventory</h3>
          <Facts
            rows={[
              ["VMs running", `${count(vms.data && mine, (v: VM) => v.state === "running")} of ${count(vms.data && mine)}`],
              ["Storage pools", count(pools.data)],
              ["Networks", count(nets.data)],
            ]}
          />
        </div>
        <div className="card">
          <h3>Alerts</h3>
          {mineAlerts.length === 0 ? (
            <p className="muted">No alerts for this node.</p>
          ) : (
            <ul className="nodealerts">
              {mineAlerts.map((a) => (
                <li key={a.id}>
                  <AlertMarks alerts={[a]} match={() => true} />
                  <div className="muted">{a.message}</div>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
    </>
  );
}

interface HostNIC {
  name: string;
  mac?: string;
  state: string;
  type: string;
  master?: string;
  mtu: number;
  speedMbps?: number;
  addresses: string[];
}

function NetworkTab({ id }: { id: number }) {
  const nets = useLive(() => get<Network[]>(`/nodes/${id}/networks`), `nets/${id}`, (e) => e.type === "network");
  // Once per visit and on request; interfaces change rarely and cost an SSH command.
  const nics = useLive(() => get<{ interfaces: HostNIC[] }>(`/nodes/${id}/interfaces`), `nics/${id}`, () => false);
  const byBridge = new Map<string, string[]>();
  (nets.data ?? []).forEach((x) => x.bridge && byBridge.set(x.bridge, [...(byBridge.get(x.bridge) ?? []), x.name]));
  return (
    <>
      <Networks fixedNode={id} />
      <div className="head sub">
        <h2>Host interfaces</h2>
        <button className="ghost" onClick={nics.reload}>
          Refresh
        </button>
      </div>
      <ErrorBox error={nics.error} />
      <div className="table-scroll nicbox">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Type</th>
              <th>State</th>
              <th>MAC</th>
              <th>Addresses</th>
              <th>Attached to</th>
              <th className="num">MTU</th>
              <th className="num">Speed</th>
            </tr>
          </thead>
          <tbody>
            {(nics.data?.interfaces ?? []).map((i) => (
              <tr key={i.name}>
                <td className="mono">{i.name}</td>
                <td>{i.type}</td>
                <td>
                  <StateBadge state={i.state} />
                </td>
                <td className="mono">{i.mac}</td>
                <td className="mono">
                  {i.addresses.map((a) => (
                    <div key={a}>{a}</div>
                  ))}
                </td>
                <td>
                  {i.master && <span className="mono">{i.master}</span>}
                  {byBridge.has(i.name) && <span>network {byBridge.get(i.name)!.join(", ")}</span>}
                </td>
                <td className="num">{i.mtu}</td>
                <td className="num">{i.speedMbps ? `${i.speedMbps} Mb/s` : ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {nics.data && nics.data.interfaces.length === 0 && <p className="muted">No interfaces reported.</p>}
      </div>
    </>
  );
}

function Settings({ n, admin }: { n: NodeStatus; admin: boolean }) {
  const online = n.state === "online";
  return (
    <div className="cards">
      <ConnectionCard n={n} admin={admin} />
      <div className="card">
        <h3>Encrypted migration</h3>
        {online ? <MigrationTLS node={n.id} online={online} admin={admin} /> : <p className="muted">Needs a connection to the node.</p>}
      </div>
    </div>
  );
}
