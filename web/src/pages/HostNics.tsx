import { useState } from "react";
import ChevronDown from "lucide-react/icons/chevron-down";
import ChevronRight from "lucide-react/icons/chevron-right";
import { get, NodeStatus } from "../api";
import { useLive } from "../live";
import { ErrorBox, StateBadge } from "../ui";
import { nodeLabel } from "./dcShared";

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

/** Host interfaces of every node, each collapsed until asked for: a closed node costs no request. */
export default function HostInterfaces({ nodes }: { nodes: NodeStatus[] }) {
  const [open, setOpen] = useState<Set<number>>(new Set());
  const toggle = (id: number) =>
    setOpen((s) => {
      const n = new Set(s);
      n.has(id) ? n.delete(id) : n.add(id);
      return n;
    });
  return (
    <>
      <div className="head sub">
        <h2>Host interfaces</h2>
      </div>
      <div className="nicnodes">
        {nodes.map((n) => (
          <div key={n.id} className="nicnode">
            <button className="ghost nicbtn" aria-expanded={open.has(n.id)} disabled={n.state !== "online"} onClick={() => toggle(n.id)}>
              {open.has(n.id) ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
              {nodeLabel(n)}
              {n.state !== "online" && <span className="muted"> ({n.state})</span>}
            </button>
            {open.has(n.id) && <NodeNics id={n.id} />}
          </div>
        ))}
      </div>
    </>
  );
}

function NodeNics({ id }: { id: number }) {
  // Once on opening and on request; interfaces change rarely and cost an SSH command.
  const nics = useLive(() => get<{ interfaces: HostNIC[] }>(`/nodes/${id}/interfaces`), `nics/${id}`, () => false);
  return (
    <>
      <ErrorBox error={nics.error} />
      <div className="actions nicrefresh">
        <button className="ghost" onClick={nics.reload}>
          Refresh
        </button>
      </div>
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
