import { Pool, Network, NodeStatus } from "../api";

/** Pool and network as returned by GET /api/pools and /api/networks: tagged with the node they live on. */
export type NodePool = Pool & { node: number; nodeName: string };
export type NodeNetwork = Network & { node: number; nodeName: string };
/** A node that is missing from an all-nodes list, and why. */
export interface NodeErr {
  node: number;
  nodeName: string;
  error: string;
}

export const nodeLabel = (n: Pick<NodeStatus, "hostname" | "addr">) => n.hostname || n.addr;

/** Which nodes could not be read; the rest of the list is still valid. */
export function NodeErrors({ errors }: { errors?: NodeErr[] }) {
  if (!errors || errors.length === 0) return null;
  return (
    <div className="error nodeerrs" role="alert">
      {errors.map((e) => (
        <div key={e.node}>
          <strong>{e.nodeName}</strong> is not shown: {e.error}
        </div>
      ))}
    </div>
  );
}

/** Node filter (first select of the filter row) and text filter shared by the unified tabs. */
export function NodeFilter(props: {
  nodes: Pick<NodeStatus, "id" | "hostname" | "addr" | "state">[];
  value: number;
  onChange: (id: number) => void;
  text: string;
  onText: (s: string) => void;
  placeholder: string;
  shown: number;
  total: number;
}) {
  return (
    <div className="filters">
      <select aria-label="Filter by node" value={props.value} onChange={(e) => props.onChange(Number(e.target.value))}>
        <option value={0}>All nodes</option>
        {props.nodes.map((n) => (
          <option key={n.id} value={n.id}>
            {nodeLabel(n)}
            {n.state !== "online" ? ` (${n.state})` : ""}
          </option>
        ))}
      </select>
      <input type="search" placeholder={props.placeholder} aria-label={props.placeholder} value={props.text} onChange={(e) => props.onText(e.target.value)} />
      {(props.text || props.value !== 0) && (
        <span className="muted">
          {props.shown} of {props.total} shown
        </span>
      )}
    </div>
  );
}
