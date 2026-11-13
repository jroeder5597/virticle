// Live statistics: polling hooks and small inline-SVG charts. The server keeps
// the history (10 minutes at 5 s); the UI just polls and draws it.
import { useEffect, useState } from "react";
import { bytes, get } from "./api";

export interface VMPoint {
  t: number;
  cpu: number | null;
  memUsedKiB: number | null;
  memTotalKiB: number | null;
  memGuest: boolean;
  diskRead: number | null;
  diskWrite: number | null;
  netRx: number | null;
  netTx: number | null;
}
export interface HostPoint {
  t: number;
  cpu: number | null;
  memUsedKiB: number | null;
  memTotalKiB: number | null;
}
export interface AllStats {
  intervalSec: number;
  nodes: Record<string, { host: HostPoint | null; vms: number; running: number; stale: boolean; cpus?: number }>;
  vms: Record<string, VMPoint>; // "node/uuid"
}

// The collector samples every 15 seconds (internal/stats), so polling faster only repeats the same values.
const POLL_MS = 15000;

/** Polls `path` while mounted and the tab is visible. Keeps the old data on a failed poll so charts do not blink. */
export function usePoll<T>(path: string | null, ms = POLL_MS): T | undefined {
  const [data, setData] = useState<T | undefined>();
  useEffect(() => {
    setData(undefined);
    if (!path) return;
    let live = true;
    const tick = () => {
      if (document.visibilityState === "hidden") return;
      get<T>(path).then(
        (d) => live && setData(d),
        () => {},
      );
    };
    // No timer runs while the tab is hidden; becoming visible fetches once and restarts it.
    let id: ReturnType<typeof setInterval> | undefined;
    const sync = () => {
      clearInterval(id);
      id = undefined;
      if (document.visibilityState === "hidden") return;
      tick();
      id = setInterval(tick, ms);
    };
    sync();
    document.addEventListener("visibilitychange", sync);
    return () => {
      live = false;
      clearInterval(id);
      document.removeEventListener("visibilitychange", sync);
    };
  }, [path, ms]);
  return data;
}

// One shared poller for /stats however many list components are mounted.
let latest: AllStats | undefined;
const watchers = new Set<(s: AllStats) => void>();
let timer: ReturnType<typeof setInterval> | undefined;
function pollAll() {
  if (document.visibilityState === "hidden") return;
  get<AllStats>("/stats").then(
    (s) => {
      latest = s;
      watchers.forEach((w) => w(s));
    },
    () => {},
  );
}

// No timer runs while the tab is hidden.
function syncAll() {
  clearInterval(timer);
  timer = undefined;
  if (document.visibilityState === "hidden") return;
  pollAll();
  timer = setInterval(pollAll, POLL_MS);
}

/** Newest sample for every node and running VM. */
export function useAllStats(): AllStats | undefined {
  const [s, setS] = useState<AllStats | undefined>(latest);
  useEffect(() => {
    watchers.add(setS);
    if (watchers.size === 1) {
      syncAll();
      document.addEventListener("visibilitychange", syncAll);
    } else pollAll();
    return () => {
      watchers.delete(setS);
      if (watchers.size === 0) {
        clearInterval(timer);
        timer = undefined;
        document.removeEventListener("visibilitychange", syncAll);
      }
    };
  }, []);
  return s;
}

// --- formatting ---

export const pct = (v: number | null | undefined) => (v == null ? "–" : `${v < 10 ? v.toFixed(1) : v.toFixed(0)}%`);
export const rate = (v: number | null | undefined) => (v == null ? "–" : `${bytes(v)}/s`);
const kib = (v: number | null | undefined) => (v == null ? "–" : bytes(v * 1024));

// --- charts ---

export interface Line {
  name: string;
  color: string; // a CSS variable such as var(--chart-1)
  values: (number | null)[];
}

const W = 300;
const WINDOW_MS = 600_000;

function paths(t: number[], values: (number | null)[], max: number, h: number) {
  const end = t[t.length - 1];
  const x = (i: number) => W - ((end - t[i]) / WINDOW_MS) * W;
  const y = (v: number) => h - 1 - (Math.min(v, max) / max) * (h - 2);
  let line = "";
  let area = "";
  let run: number[] = [];
  const flush = () => {
    if (run.length === 1) {
      // A lone sample between gaps still gets a visible tick.
      const i = run[0];
      line += `M${(x(i) - 1.5).toFixed(1)},${y(values[i]!).toFixed(1)}L${(x(i) + 1.5).toFixed(1)},${y(values[i]!).toFixed(1)}`;
    } else if (run.length > 1) {
      const pts = run.map((i) => `${x(i).toFixed(1)},${y(values[i]!).toFixed(1)}`);
      line += "M" + pts.join("L");
      area += `M${x(run[0]).toFixed(1)},${h}L` + pts.join("L") + `L${x(run[run.length - 1]).toFixed(1)},${h}Z`;
    }
    run = [];
  };
  values.forEach((v, i) => (v == null ? flush() : run.push(i)));
  flush();
  return { line, area };
}

/** The drawing alone: lines and a soft area under the first one, scaled to `max`. */
export function Plot({ t, lines, max, height, grid }: { t: number[]; lines: Line[]; max: number; height: number; grid?: boolean }) {
  return (
    <svg className="plot" viewBox={`0 0 ${W} ${height}`} preserveAspectRatio="none" role="img" aria-hidden="true" style={{ height }}>
      {grid && (
        <>
          <line x1="0" x2={W} y1={height - 0.5} y2={height - 0.5} className="gridline" vectorEffect="non-scaling-stroke" />
          <line x1="0" x2={W} y1={height / 2} y2={height / 2} className="gridline dashed" vectorEffect="non-scaling-stroke" />
          <line x1="0" x2={W} y1={0.5} y2={0.5} className="gridline dashed" vectorEffect="non-scaling-stroke" />
        </>
      )}
      {t.length > 0 &&
        lines.map((l, k) => {
          const p = paths(t, l.values, max, height);
          return (
            <g key={l.name} style={{ color: l.color }}>
              {k === 0 && <path d={p.area} className="area" />}
              <path d={p.line} className="stroke" vectorEffect="non-scaling-stroke" />
            </g>
          );
        })}
    </svg>
  );
}

const last = <T,>(a: T[]): T | undefined => a[a.length - 1];

/** Small meter with a number, for tables. */
export function Meter({ value, max, label }: { value: number | null | undefined; max: number; label: string }) {
  const p = value == null || max <= 0 ? 0 : Math.min(100, (value / max) * 100);
  return (
    <div className="meter" title={label}>
      <div className="meter-bar">
        <div className={p > 90 ? "full" : ""} style={{ width: p + "%" }} />
      </div>
      <span className="meter-label">{label}</span>
    </div>
  );
}

/** A sparkline for one host metric (no axes). */
export function Spark({ t, values, max }: { t: number[]; values: (number | null)[]; max: number }) {
  return (
    <div className="spark">
      <Plot t={t} lines={[{ name: "v", color: "var(--chart-1)", values }]} max={max} height={26} />
    </div>
  );
}

/** The buffered host series of one node. */
export function useHostSeries(node: number, enabled: boolean): HostPoint[] {
  return usePoll<{ points: HostPoint[] }>(enabled ? `/nodes/${node}/stats` : null)?.points ?? [];
}

/** CPU or memory of one node: a bar with a sparkline, for the Nodes page. */
export function HostLoad({ points: pts, kind }: { points: HostPoint[]; kind: "cpu" | "mem" }) {
  const cur = last(pts);
  if (!cur) return null;
  const t = pts.map((p) => p.t);
  if (kind === "cpu") {
    return (
      <div className="load">
        <Meter value={cur.cpu} max={100} label={`${pct(cur.cpu)} busy`} />
        <Spark t={t} values={pts.map((p) => p.cpu)} max={100} />
      </div>
    );
  }
  const total = cur.memTotalKiB ?? 0;
  return (
    <div className="load">
      <Meter value={cur.memUsedKiB} max={total} label={`${kib(cur.memUsedKiB)} of ${kib(total)} used`} />
      <Spark t={t} values={pts.map((p) => p.memUsedKiB)} max={Math.max(total, 1)} />
    </div>
  );
}
