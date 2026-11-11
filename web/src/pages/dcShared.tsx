import Select from "../Select";
import { Network, NodeStatus, Pool, ServerEvent } from "../api";
import { CheckList } from "../Checks";

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
    <div className="nodeerrs" role="alert">
      <CheckList summary={false} label="Nodes that could not be read" items={errors.map((e) => ({ key: String(e.node), status: "error", title: e.nodeName, detail: `is not shown: ${e.error}` }))} />
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
      <Select
        auto
        label="Filter by node"
        value={String(props.value)}
        onChange={(v) => props.onChange(Number(v))}
        options={[{ value: "0", label: "All nodes" }, ...props.nodes.map((n) => ({ value: String(n.id), label: nodeLabel(n) + (n.state !== "online" ? ` (${n.state})` : ""), dot: "n-" + n.state }))]}
      />
      <input type="search" placeholder={props.placeholder} aria-label={props.placeholder} value={props.text} onChange={(e) => props.onText(e.target.value)} />
      {(props.text || props.value !== 0) && (
        <span className="muted">
          {props.shown} of {props.total} shown
        </span>
      )}
    </div>
  );
}

const taskDone = (e: ServerEvent) => e.type === "task" && e.task?.status !== "running";
/** Only a node connecting or going away changes the node list; domain events are about VMs. */
export const nodeEvents = (e: ServerEvent) => e.type === "node";
/** What changes a pool or the volumes in it: pool events, a finished task, a node (re)connecting. Not VM power events. */
export const poolEvents = (e: ServerEvent) => e.type === "pool" || e.type === "node" || taskDone(e);
/** Networks also show attached VM interfaces, so a VM being defined or removed counts, starting and stopping do not. */
export const netEvents = (e: ServerEvent) => e.type === "network" || e.type === "node" || taskDone(e) || (e.type === "domain" && /defined|updated/.test(e.detail ?? ""));
