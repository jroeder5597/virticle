import { useEffect, useState } from "react";
import { get, Network, NodeStatus } from "../api";
import { useLive } from "../live";
import { ErrorBox, NodePicker, StateBadge } from "../ui";

export default function Networks() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const [node, setNode] = useState(0);
  const all = nodes.data?.nodes ?? [];
  useEffect(() => {
    if (!all.some((n) => n.id === node && n.state === "online")) setNode(all.find((n) => n.state === "online")?.id ?? 0);
  }, [all, node]);
  const nets = useLive(() => (node ? get<Network[]>(`/nodes/${node}/networks`) : Promise.resolve([])), `nets/${node}`);
  return (
    <>
      <div className="head">
        <h1>Networks</h1>
        {all.length > 0 && <NodePicker nodes={all} value={node} onChange={setNode} />}
      </div>
      <ErrorBox error={nets.error} />
      {nets.data && nets.data.length === 0 ? (
        <div className="empty">{node ? "No virtual networks on this node." : "No node is online."}</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>State</th>
              <th>Mode</th>
              <th>Bridge</th>
              <th>Address</th>
              <th>Starts with host</th>
            </tr>
          </thead>
          <tbody>
            {(nets.data ?? []).map((n) => (
              <tr key={n.name}>
                <td>{n.name}</td>
                <td>
                  <StateBadge state={n.active ? "active" : "inactive"} />
                </td>
                <td>{n.forward}</td>
                <td className="mono">{n.bridge}</td>
                <td className="mono">{n.address}</td>
                <td>{n.autostart ? "Yes" : "No"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
