// Performance history: the query answer, the panels that draw it and number formatting.
import { bytes } from "../api";

export interface PerfDevice {
  id: string;
  name: string;
  kind: string;
}
export interface PerfEvent {
  t: number;
  kind: "started" | "stopped" | "migrated";
  node?: number;
}
export interface PerfResult {
  kind: "vm" | "node";
  id: string;
  from: number;
  to: number;
  step: number;
  peak: boolean;
  t: number[];
  series: Record<string, (number | null)[]>;
  max?: Record<string, (number | null)[]>;
  devices: Record<string, PerfDevice[]>;
  events: PerfEvent[];
}
export interface Busy {
  uuid: string;
  name: string;
  node: number;
  avg: number;
  max: number;
}
export interface PerfOverview {
  from: number;
  to: number;
  step: number;
  cpu: Busy[];
  memory: Busy[];
  disk: Busy[];
  network: Busy[];
}

export type Unit = "pct" | "kib" | "bytes" | "bps" | "ops" | "ms" | "num" | "mib" | "celsius";

export type Group = "cpu" | "memory" | "disk" | "network" | "storage" | "gpu";
export const GROUP_LABEL: Record<Group, string> = { cpu: "CPU", memory: "Memory", disk: "Disk", network: "Network", storage: "Storage", gpu: "GPU" };

export interface PanelSeries {
  key: string; // series name; "key/device" series are folded in
  label: string;
}
export interface Panel {
  group: Group;
  title: string;
  unit: Unit;
  series: PanelSeries[];
}

const vmPanels: Panel[] = [
  { group: "cpu", title: "CPU (percent of the VM's virtual CPUs)", unit: "pct", series: [
    { key: "cpu", label: "Used" },
    { key: "cpu_delay", label: "Waiting for a host CPU" },
  ] },
  { group: "memory", title: "Memory", unit: "kib", series: [
    { key: "mem_assigned_kib", label: "Assigned" },
    { key: "mem_used_kib", label: "Used in the guest" },
    { key: "mem_rss_kib", label: "On the host" },
  ] },
  { group: "disk", title: "Disk throughput", unit: "bytes", series: [
    { key: "disk_rd_bps", label: "Read" },
    { key: "disk_wr_bps", label: "Write" },
  ] },
  { group: "disk", title: "Disk operations per second", unit: "ops", series: [
    { key: "disk_rd_ops", label: "Reads" },
    { key: "disk_wr_ops", label: "Writes" },
  ] },
  { group: "disk", title: "Disk latency per operation", unit: "ms", series: [{ key: "disk_lat_ms", label: "Latency" }] },
  { group: "network", title: "Network", unit: "bps", series: [
    { key: "net_rx_bps", label: "Received" },
    { key: "net_tx_bps", label: "Sent" },
  ] },
  { group: "network", title: "Dropped packets per second", unit: "ops", series: [
    { key: "net_rx_drop", label: "Received" },
    { key: "net_tx_drop", label: "Sent" },
  ] },
];

const nodePanels: Panel[] = [
  { group: "cpu", title: "CPU (percent of all cores)", unit: "pct", series: [
    { key: "cpu", label: "Busy" },
    { key: "cpu_user", label: "User" },
    { key: "cpu_system", label: "System" },
    { key: "cpu_iowait", label: "Waiting for disk" },
    { key: "cpu_steal", label: "Stolen by the host below" },
  ] },
  { group: "cpu", title: "Load average", unit: "num", series: [
    { key: "load1", label: "1 minute" },
    { key: "load5", label: "5 minutes" },
    { key: "load15", label: "15 minutes" },
  ] },
  { group: "memory", title: "Memory", unit: "kib", series: [
    { key: "mem_used_kib", label: "Used" },
    { key: "mem_cached_kib", label: "Cache" },
    { key: "mem_avail_kib", label: "Available" },
    { key: "swap_used_kib", label: "Swap used" },
  ] },
  { group: "disk", title: "Disk throughput", unit: "bytes", series: [
    { key: "disk_rd_bps", label: "Read" },
    { key: "disk_wr_bps", label: "Write" },
  ] },
  { group: "disk", title: "Disk operations per second", unit: "ops", series: [
    { key: "disk_rd_ops", label: "Reads" },
    { key: "disk_wr_ops", label: "Writes" },
  ] },
  { group: "disk", title: "Disk busy (percent of time)", unit: "pct", series: [{ key: "disk_busy", label: "Busy" }] },
  { group: "disk", title: "Disk latency per operation", unit: "ms", series: [{ key: "disk_lat_ms", label: "Latency" }] },
  { group: "network", title: "Network", unit: "bps", series: [
    { key: "net_rx_bps", label: "Received" },
    { key: "net_tx_bps", label: "Sent" },
  ] },
  { group: "network", title: "Errors and drops per second", unit: "ops", series: [
    { key: "net_rx_err", label: "Receive errors" },
    { key: "net_tx_err", label: "Send errors" },
    { key: "net_rx_drop", label: "Received dropped" },
    { key: "net_tx_drop", label: "Sent dropped" },
  ] },
  { group: "storage", title: "Storage pool use", unit: "bytes", series: [{ key: "pool_used_bytes", label: "Used" }] },
  { group: "gpu", title: "GPU utilisation", unit: "pct", series: [{ key: "gpu_util", label: "Busy" }] },
  { group: "gpu", title: "GPU memory used", unit: "mib", series: [{ key: "gpu_mem_used_mib", label: "Used" }] },
  { group: "gpu", title: "GPU temperature", unit: "celsius", series: [{ key: "gpu_temp", label: "Temperature" }] },
];

export const panelsFor = (kind: "vm" | "node") => (kind === "vm" ? vmPanels : nodePanels);

/** A panel's lines in the answer: its series, or one line per device for pools and GPUs. */
export interface Line {
  id: string;
  label: string;
  avg: (number | null)[];
  max?: (number | null)[];
}
export function linesOf(p: Panel, r: PerfResult): Line[] {
  const out: Line[] = [];
  for (const s of p.series) {
    if (r.series[s.key]) out.push({ id: s.key, label: s.label, avg: r.series[s.key], max: r.max?.[s.key] });
    for (const k of Object.keys(r.series)) {
      if (k.startsWith(s.key + "/")) {
        const dev = k.slice(s.key.length + 1);
        out.push({ id: k, label: dev.replace(/^\d+ /, ""), avg: r.series[k], max: r.max?.[k] });
      }
    }
  }
  return out.filter((l) => l.avg.some((v) => v != null));
}

// --- number formatting ---

function si(v: number, units: string[], base: number): string {
  let i = 0;
  while (Math.abs(v) >= base && i < units.length - 1) {
    v /= base;
    i++;
  }
  const a = Math.abs(v);
  return `${a >= 100 || (i === 0 && Number.isInteger(v)) ? v.toFixed(0) : a >= 10 ? v.toFixed(1) : v.toFixed(2)} ${units[i]}`;
}

export function fmt(unit: Unit, v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return "–";
  switch (unit) {
    case "pct":
      return `${v < 10 ? v.toFixed(1) : v.toFixed(0)}%`;
    case "kib":
      return bytes(v * 1024);
    case "mib":
      return bytes(v * 1048576);
    case "bytes":
      return `${bytes(v)}/s`;
    case "bps":
      return si(v, ["bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"], 1000);
    case "ops":
      return v >= 100 ? `${v.toFixed(0)}/s` : v >= 10 ? `${v.toFixed(1)}/s` : `${v.toFixed(2).replace(/\.?0+$/, "") || "0"}/s`;
    case "ms":
      return v >= 100 ? `${v.toFixed(0)} ms` : `${v.toFixed(1)} ms`;
    case "celsius":
      return `${v.toFixed(0)} °C`;
    default:
      return v >= 100 ? v.toFixed(0) : v.toFixed(2).replace(/\.?0+$/, "") || "0";
  }
}

/** Pool use is a size, not a rate: the bytes panel for pools should not say "/s". */
export function unitFor(p: Panel): Unit {
  return p.group === "storage" ? "kib" : p.unit;
}
export function valueScale(p: Panel, v: number | null): number | null {
  return v != null && p.group === "storage" ? v / 1024 : v;
}

/** Axis ticks: a round step so that about `n` ticks cover 0..max. */
export function niceTicks(max: number, unit: Unit, n = 4): { top: number; ticks: number[] } {
  if (!(max > 0)) max = 1;
  const base = unit === "kib" || unit === "bytes" || unit === "mib" ? 1024 : 10;
  let step: number;
  if (base === 1024) {
    const e = Math.floor(Math.log(max / n) / Math.log(1024));
    const unitSize = Math.pow(1024, Math.max(0, e));
    const f = max / n / unitSize;
    step = (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : f <= 10 ? 10 : f <= 20 ? 20 : f <= 50 ? 50 : f <= 100 ? 100 : f <= 200 ? 200 : f <= 500 ? 500 : 1024) * unitSize;
  } else {
    const raw = max / n;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const f = raw / mag;
    step = (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * mag;
  }
  const ticks: number[] = [];
  for (let v = 0; v < max + step * 0.999; v += step) ticks.push(v);
  return { top: ticks[ticks.length - 1], ticks };
}

// --- time ---

const two = (n: number) => String(n).padStart(2, "0");
export function clock(sec: number): string {
  const d = new Date(sec * 1000);
  return `${two(d.getHours())}:${two(d.getMinutes())}`;
}
export function dayLabel(sec: number): string {
  return new Date(sec * 1000).toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short" });
}
export function stamp(sec: number, withSeconds = false): string {
  const d = new Date(sec * 1000);
  return `${dayLabel(sec)} ${clock(sec)}${withSeconds ? ":" + two(d.getSeconds()) : ""}`;
}
export function stepWords(step: number): string {
  return step < 60 ? `${step} seconds` : step % 60 === 0 && step < 3600 ? `${step / 60} ${step === 60 ? "minute" : "minutes"}` : `${Math.round(step / 60)} minutes`;
}

/** The fields of a datetime-local input, in local time. */
export function toLocalInput(sec: number): string {
  const d = new Date(sec * 1000);
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())}T${two(d.getHours())}:${two(d.getMinutes())}`;
}
export function fromLocalInput(s: string): number | null {
  const ms = new Date(s).getTime();
  return Number.isFinite(ms) ? Math.floor(ms / 1000) : null;
}

export const RANGES = [
  ["1h", "Last hour", 3600],
  ["6h", "Last 6 hours", 6 * 3600],
  ["24h", "Last 24 hours", 24 * 3600],
  ["7d", "Last 7 days", 7 * 24 * 3600],
] as const;
