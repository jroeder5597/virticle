// The node and VM lists the resource tree already holds, shared so that other parts of the shell
// (the Tasks dock) can link a task to its VM or node without requesting them again.
import { useSyncExternalStore } from "react";
import type { NodeStatus, VM } from "./api";

export interface Inventory {
  nodes: NodeStatus[];
  vms: VM[];
}
let inv: Inventory = { nodes: [], vms: [] };
const subs = new Set<() => void>();

export function publishInventory(nodes: NodeStatus[] | undefined, vms: VM[] | undefined) {
  if ((nodes ?? inv.nodes) === inv.nodes && (vms ?? inv.vms) === inv.vms) return;
  inv = { nodes: nodes ?? inv.nodes, vms: vms ?? inv.vms };
  subs.forEach((f) => f());
}

export function useInventory(): Inventory {
  return useSyncExternalStore(
    (cb) => {
      subs.add(cb);
      return () => void subs.delete(cb);
    },
    () => inv,
    () => inv,
  );
}

const nodeName = (n: NodeStatus) => n.hostname || n.addr;

/** Where a task's node and target point to, as app paths; undefined when the record does not say. */
export function taskLinks(i: Inventory, node: string, target: string): { node?: string; target?: string } {
  const n = i.nodes.find((x) => nodeName(x) === node || x.addr === node);
  if (!n) return {};
  const out: { node?: string; target?: string } = { node: `/node/${n.id}` };
  if (target === node) out.target = out.node;
  else {
    // Targets look like "vm", "vm@snapshot" or "vm/device".
    const vmName = target.split(/[@/]/)[0];
    const v = i.vms.find((x) => x.node === n.id && x.name === vmName);
    if (v) out.target = `/vm/${n.id}/${v.uuid}`;
  }
  return out;
}
