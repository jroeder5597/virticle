// "N networks differ between nodes": one collapsed line at the top of the Datacenter Network tab, only
// shown when the nodes that can exchange VMs do not all have the same networks. Opens to a small table; a network
// that is missing on a node and can be copied as it is gets a "Create on <node>" button.
import { useState } from "react";
import ChevronDown from "lucide-react/icons/chevron-down";
import ChevronRight from "lucide-react/icons/chevron-right";
import { get, post } from "../api";
import { atLeast, useRole } from "../auth";
import { useLiveDebounced } from "../liveDebounced";
import { ErrorBox, useAction } from "../ui";
import { netEvents } from "./dcShared";
import "./hw.css";

interface DriftRow {
  network: string;
  kind: "missing" | "mode" | "vlan" | "bridge";
  what: string;
  nodes: string[];
  used: boolean;
  fix?: { label: string; network: string; from: number; to: number };
}
interface Drift {
  networks: number;
  rows: DriftRow[];
  blocked: string[];
  unread: string[];
}

export default function NetworkDrift() {
  const operator = atLeast(useRole(), "operator");
  const drift = useLiveDebounced(() => get<Drift>("/cluster/network-drift"), "network-drift", netEvents, 3000);
  const [open, setOpen] = useState(false);
  const act = useAction();
  const d = drift.data;
  if (!d || d.networks === 0) return null;
  const Chevron = open ? ChevronDown : ChevronRight;

  // The definition is copied as it is, minus the id, which belongs to the network it came from.
  const create = (f: NonNullable<DriftRow["fix"]>) =>
    act.run(async () => {
      const src = await get<{ xml: string }>(`/nodes/${f.from}/networks/${encodeURIComponent(f.network)}`);
      await post(`/nodes/${f.to}/networks`, { xml: src.xml.replace(/\s*<uuid>[^<]*<\/uuid>/, "") });
      drift.reload();
    });

  return (
    <section className="drift">
      <button className="disc-head" aria-expanded={open} aria-controls="drift-body" onClick={() => setOpen(!open)}>
        <Chevron size={16} aria-hidden="true" />
        <span className="disc-title">{d.networks === 1 ? "1 network differs between nodes." : `${d.networks} networks differ between nodes.`}</span>
      </button>
      {open && (
        <div id="drift-body" className="disc-body">
          <div className="table-scroll">
            <table className="compact">
              <thead>
                <tr>
                  <th>Network</th>
                  <th>What differs</th>
                  <th>Nodes</th>
                  <th className="fit" />
                </tr>
              </thead>
              <tbody>
                {d.rows.map((r, i) => (
                  <tr key={i}>
                    <td>{r.network}</td>
                    <td>{r.what}</td>
                    <td>{r.nodes.join(", ")}</td>
                    <td className="fit">
                      {r.fix && operator && (
                        <button disabled={act.busy} onClick={() => create(r.fix!)}>
                          {r.fix.label}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <ErrorBox error={act.error} />
        </div>
      )}
    </section>
  );
}
