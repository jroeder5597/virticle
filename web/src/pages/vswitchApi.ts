// Types and small helpers for virtual switches (host bridges). The shapes mirror GET /api/nodes/{node}/hostnet
// (internal/api/hostnet.go) and the plan of the node command (internal/nodeproto/nettypes.go).
import type { Task } from "../api";

export interface NetChange {
  id: string;
  action: string;
  bridge: string;
  state: "armed" | "applying" | "applied" | "confirmed" | "rolled-back" | "failed";
  reason?: string;
  message?: string;
  started: string;
  finished?: string;
  secondsLeft?: number;
}
export interface HostNic {
  name: string;
  state: string;
  type: string;
  kind?: string;
  master?: string;
  speedMbps?: number;
  addresses: string[];
}
export interface HostBridge {
  name: string;
  managed: boolean;
  vlanFiltering: boolean;
  vlans?: string;
  uplink?: string;
  ports: string[];
  movedAddresses?: boolean;
  networks: string[];
  vms: string[];
  readOnly?: string;
}
export interface HostnetState {
  stack: string;
  supported: boolean;
  stackNote?: string;
  libvirtVersion: string;
  vlanCapable: boolean;
  controllerAddresses: string[];
  managementInterface?: string;
  interfaces: HostNic[];
  bridges: HostBridge[];
  status: { pending?: NetChange; recent: NetChange[] };
  nextBridgeName: string;
}
export interface NetPlan {
  summary: string[];
  files?: { path: string; action: string; content?: string }[];
  commands?: string[];
  managementAffected: boolean;
  warnings?: string[];
  timeoutSeconds: number;
}
/** What the API takes for plan and apply. */
export interface NetBody {
  action: "create" | "delete" | "set-vlans";
  bridge: string;
  uplink?: string;
  moveAddresses?: boolean;
  vlanAware?: boolean;
  vlans?: string;
  timeoutSeconds?: number;
}

export const DEFAULT_CONFIRM_SECONDS = 90;
/** The task progress the server reports once the node has started the change and the controller waits for it. */
export const WAITING_PROGRESS = 0.2;

/** Cards that can become the uplink of a new virtual switch: physical, not a port of a bond or a bridge. */
export const freeCards = (st: HostnetState) => st.interfaces.filter((n) => n.type === "physical" && !n.master);

export const addrIP = (cidr: string) => cidr.split("/")[0];
/** The addresses worth showing: without IPv6 link-local ones (fe80::). */
export const shownAddrs = (list: string[]) => list.filter((a) => !/^fe80:/i.test(a));

/** The one sentence that says why Add is not possible on this node, or "" when it is. */
export function addBlocked(st: HostnetState | undefined, error: string | undefined): string {
  if (error) return error;
  if (!st) return "";
  if (!st.supported) return st.stackNote || "This node's network system is not supported yet; configure the bridge on the node itself.";
  if (st.status.pending) return "Another network change is waiting for this node.";
  return "";
}

/** The final outcome of a failed change, in the words the dialog uses. */
export function failureWords(error: string | undefined): string {
  const e = (error ?? "").trim();
  if (e.startsWith("The node rolled the change back by itself because it was not confirmed in time")) return "The node did not come back in time and restored its previous settings.";
  const m = /^(?:Applying the change failed on the node, and it restored its previous network settings\.|The new network settings did not work on the node \(no address\), and it restored its previous network settings\.|The change was rolled back on the node\. Its previous network settings are active again\.)\s*(.*)$/.exec(e);
  if (m) return "The node restored its previous settings" + (m[1] ? ": " + m[1] : ".");
  return e || "The change failed.";
}

/** The line for a change that was rolled back, from net-status. */
export function rolledBackWords(c: NetChange): string {
  const why =
    c.reason === "timer"
      ? "it was not confirmed in time"
      : c.reason === "apply failed"
        ? "applying it failed" + (c.message ? ": " + c.message : "")
        : c.reason === "check failed"
          ? "the node had no address with the new settings" + (c.message ? ": " + c.message : "")
          : "it was rolled back by hand";
  return `The last network change (${verb(c)}) was rolled back and the node restored its previous settings: ${why}.`;
}

function verb(c: NetChange): string {
  return c.action === "create" ? `add virtual switch ${c.bridge}` : c.action === "delete" ? `remove virtual switch ${c.bridge}` : `edit VLANs of ${c.bridge}`;
}
export const changeWords = verb;

export function isTaskDone(t: Task | undefined): boolean {
  return !!t && t.status !== "running";
}
