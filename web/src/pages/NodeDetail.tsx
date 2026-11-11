import { ReactNode, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import ServerOff from "lucide-react/icons/server-off";
import { bytes, del, get, Network, NodeInfo, NodeStatus, Pool, put, VM } from "../api";
import { useLive } from "../live";
import { useRole } from "../auth";
import { ErrorBox, StateBadge, useAction } from "../ui";
import { HostLoad, useHostSeries } from "../charts";
import { AlertMarks, useAlerts } from "./Alerts";
import Evacuate from "./Evacuate";
import HostUpdates from "./HostUpdates";
import MigrationTLS from "./MigrationTLS";
import Networks from "./Networks";
import Storage from "./Storage";
import VMs from "./VMs";
import "../node.css";

const TABS = [
  ["summary", "Summary"],
  ["vms", "Virtual machines"],
  ["storage", "Storage"],
  ["network", "Network"],
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
  const [evacuating, setEvacuating] = useState(false);
  const n = nodes.data?.nodes.find((x) => x.id === id);
  const tab: Tab = TABS.some(([k]) => k === tabParam) ? (tabParam as Tab) : "summary";

  if (!nodes.data) return <ErrorBox error={nodes.error} />;
  if (!n) return <div className="empty">There is nothing at this address, or your role does not allow it.</div>;

  const online = n.state === "online";
  const name = n.hostname || n.addr;
  const toggleMaintenance = () => act.run(() => put(`/nodes/${n.id}/maintenance`, { on: !n.maintenance }));
  const remove = async () => {
    if (!window.confirm(`Remove ${name}? Its virtual machines keep running; they just stop being shown here.`)) return;
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
              maintenance
            </span>
          )}
          <span className="mono muted">
            {n.user}@{n.addr}
          </span>
          <AlertMarks alerts={alerts} match={(a) => a.node === n.id && a.kind !== "pool-usage" && a.kind !== "vm-state"} />
        </div>
        {admin && (
          <div className="actions">
            {online && (
              <button onClick={() => setEvacuating(true)} title="Move the virtual machines of this node to the others">
                Evacuate…
              </button>
            )}
            <button onClick={toggleMaintenance} disabled={act.busy} title="A node in maintenance is never chosen as a migration or evacuation target">
              {n.maintenance ? "End maintenance" : "Maintenance"}
            </button>
            <button className="danger" onClick={remove} disabled={act.busy}>
              Remove
            </button>
          </div>
        )}
      </div>
      <ErrorBox error={act.error} />
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
          <p>{n.state === "connecting" ? "Connecting to this node…" : "This node is offline, so nothing can be read from or changed on it."}</p>
          {n.error && <p className="rowerror">{n.error}</p>}
        </div>
      ) : (
        <>
          {tab === "summary" && <Summary n={n} alerts={alerts} />}
          {tab === "vms" && <VMs fixedNode={id} />}
          {tab === "storage" && <Storage fixedNode={id} />}
          {tab === "network" && <NetworkTab id={id} />}
          {tab === "updates" && <HostUpdates node={id} online={online} />}
          {tab === "settings" && <Settings n={n} admin={admin} onMaintenance={toggleMaintenance} onRemove={remove} busy={act.busy} />}
        </>
      )}
      {evacuating && nodes.data && <Evacuate node={n} nodes={nodes.data.nodes} onClose={() => setEvacuating(false)} />}
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
              ["Memory", info ? `${bytes(info.memoryKiB * 1024)} total, ${bytes(info.freeKiB * 1024)} free` : "–"],
              ["libvirt", info?.libvirtVersion ?? "–"],
              ["Address", <span className="mono" key="a">{n.user}@{n.addr}{n.jump ? ` via ${n.jump}` : ""}</span>],
            ]}
          />
        </div>
        <div className="card">
          <h3>Load</h3>
          <div className="nodeload">
            <HostLoad points={series} kind="cpu" />
            <HostLoad points={series} kind="mem" />
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
              <th>Bridge / master</th>
              <th>Libvirt network</th>
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
                <td className="mono">{i.master}</td>
                <td>{byBridge.get(i.name)?.join(", ")}</td>
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

function Settings({ n, admin, onMaintenance, onRemove, busy }: { n: NodeStatus; admin: boolean; onMaintenance: () => void; onRemove: () => void; busy: boolean }) {
  const online = n.state === "online";
  return (
    <div className="cards">
      <div className="card">
        <h3>Connection</h3>
        <Facts
          rows={[
            ["Address", <span className="mono" key="a">{n.addr}</span>],
            ["SSH user", <span className="mono" key="u">{n.user}</span>],
            ["Jump host", n.jump ? <span className="mono" key="j">{n.jump}</span> : "none"],
            ["State", <StateBadge key="s" state={n.state} />],
          ]}
        />
        {n.error && <p className="rowerror">{n.error}</p>}
      </div>
      <div className="card">
        <h3>Maintenance</h3>
        <p className="muted">A node in maintenance is never chosen as a migration or evacuation target.</p>
        <p>{n.maintenance ? "This node is in maintenance." : "This node is available as a target."}</p>
        {admin && (
          <button onClick={onMaintenance} disabled={busy}>
            {n.maintenance ? "End maintenance" : "Start maintenance"}
          </button>
        )}
      </div>
      <div className="card">
        <h3>Encrypted migration</h3>
        {online ? <MigrationTLS node={n.id} online={online} admin={admin} /> : <p className="muted">Needs a connection to the node.</p>}
      </div>
      <div className="card">
        <h3>Passthrough and topology</h3>
        <p className="muted">
          Host devices for passthrough and the NUMA layout are listed per virtual machine, under its Hardware and Tuning tabs. Open a VM of this node from the <Link to={`/node/${n.id}/vms`}>VM tab</Link>.
        </p>
      </div>
      {admin && (
        <div className="card">
          <h3>Remove node</h3>
          <p className="muted">Its virtual machines keep running; they just stop being shown here.</p>
          <button className="danger" onClick={onRemove} disabled={busy}>
            Remove node
          </button>
        </div>
      )}
    </div>
  );
}
