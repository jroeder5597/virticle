import { get } from "./api";
import { useLive } from "./live";

/** GET /api/cluster/cpu-baseline: the best CPU model every online node can run. */
export interface CpuBaseline {
  model?: string;
  vendor?: string;
  features: { policy: string; name: string }[];
  source?: string;
  nodes: { id: number; name: string; hostModel: string }[];
  skipped: { id: number; name: string; reason: string }[];
  computed: string;
  error?: string;
}

/** The baseline, reloaded only when the server says it changed. */
export function useCpuBaseline() {
  return useLive(() => get<CpuBaseline>("/cluster/cpu-baseline"), "cpu-baseline", (e) => e.type === "cpubaseline" || (e.type === "node" && e.detail === "resync")).data;
}

export const baselineLabel = (b?: CpuBaseline) => (b?.model && !b.error ? `Cluster baseline (${b.model})` : "Cluster baseline (not available)");

/** One sentence for a VM whose CPU differs from the current baseline; "" when it matches. */
export function relationText(r?: string): string {
  switch (r) {
    case "newer":
      return "Newer than the current cluster baseline. Migration to a node with an older CPU may be refused.";
    case "older":
      return "Older than the current cluster baseline.";
    case "differs":
      return "Differs from the current cluster baseline.";
  }
  return "";
}
