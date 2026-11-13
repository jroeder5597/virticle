// One performance chart as inline SVG: lines for the averages, a lighter band up to the peaks when a step
// is longer than the finest, gaps where there is no data, markers for starts, stops and moves of a VM.
// All charts of a page share one crosshair (cursor) and one drag selection (sel).
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Line, Panel, PerfEvent, clock, dayLabel, fmt, linesOf, niceTicks, unitFor, valueScale, PerfResult } from "./perfApi";

export interface Sel {
  a: number; // seconds
  b: number;
}

const H = 150;
const M = { l: 64, r: 10, t: 8, b: 22 };
export const DASH = ["", "6 3", "2 3", "9 3 2 3", "12 4"];

function runs(v: (number | null)[]): number[][] {
  const out: number[][] = [];
  let cur: number[] = [];
  v.forEach((x, i) => {
    if (x == null) {
      if (cur.length) out.push(cur);
      cur = [];
    } else cur.push(i);
  });
  if (cur.length) out.push(cur);
  return out;
}

export function useWidth(): [React.RefObject<HTMLDivElement | null>, number] {
  const ref = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(600);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const set = () => setW(Math.max(240, Math.floor(el.clientWidth)));
    set();
    const ro = new ResizeObserver(set);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  return [ref, w];
}

export default function PerfChart(props: {
  panel: Panel;
  res: PerfResult;
  cursor: number | null;
  onCursor: (i: number | null) => void;
  sel: Sel | null;
  onSel: (s: Sel | null) => void;
  onZoom: (s: Sel) => void;
}) {
  const { panel, res, cursor, onCursor, sel, onSel, onZoom } = props;
  const [wrap, W] = useWidth();
  const unit = unitFor(panel);
  const lines: Line[] = linesOf(panel, res).map((l) => ({
    ...l,
    avg: l.avg.map((v) => valueScale(panel, v)),
    max: l.max?.map((v) => valueScale(panel, v)),
  }));
  const t = res.t;
  const x0 = t[0];
  const x1 = t.length > 1 ? t[t.length - 1] : x0 + res.step;
  const pw = W - M.l - M.r;
  const ph = H - M.t - M.b;
  const x = (s: number) => M.l + ((s - x0) / (x1 - x0 || 1)) * pw;
  const showPeak = res.peak;

  let maxV = 0;
  for (const l of lines) for (let i = 0; i < t.length; i++) maxV = Math.max(maxV, (showPeak ? l.max?.[i] : null) ?? l.avg[i] ?? 0, l.avg[i] ?? 0);
  if (unit === "pct") maxV = Math.max(maxV, 5);
  const { top, ticks } = niceTicks(maxV, unit);
  const topV = unit === "pct" ? Math.min(top, 100) : top;
  const y = (v: number) => M.t + ph - (Math.min(v, topV) / topV) * ph;

  // x ticks: clock times, with the day when the range is longer than a day
  const span = x1 - x0;
  const wantTicks = Math.max(2, Math.floor(pw / 90));
  const cand = [60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800];
  const tickStep = cand.find((c) => span / c <= wantTicks) ?? 172800;
  const xt: number[] = [];
  for (let s = Math.ceil(x0 / tickStep) * tickStep; s <= x1; s += tickStep) xt.push(s);
  // Align ticks to local midnight multiples for day-sized steps
  const long = span > 86400 * 1.2;

  const svgRef = useRef<SVGSVGElement>(null);
  const drag = useRef<{ x: number; moved: boolean } | null>(null);
  const toTime = (clientX: number) => {
    const r = svgRef.current!.getBoundingClientRect();
    const f = (clientX - r.left - M.l) / pw;
    return x0 + Math.min(1, Math.max(0, f)) * (x1 - x0);
  };
  const nearest = (s: number) => {
    let best = 0;
    for (let i = 1; i < t.length; i++) if (Math.abs(t[i] - s) < Math.abs(t[best] - s)) best = i;
    return best;
  };
  const down = (e: React.PointerEvent) => {
    drag.current = { x: e.clientX, moved: false };
    (e.currentTarget as Element).setPointerCapture?.(e.pointerId);
    onCursor(nearest(toTime(e.clientX)));
  };
  const move = (e: React.PointerEvent) => {
    const s = toTime(e.clientX);
    onCursor(nearest(s));
    const d = drag.current;
    if (d) {
      if (Math.abs(e.clientX - d.x) > 6) d.moved = true;
      if (d.moved) onSel({ a: toTime(d.x), b: s });
    }
  };
  const up = (e: React.PointerEvent) => {
    const d = drag.current;
    drag.current = null;
    if (d?.moved) {
      const a = toTime(d.x);
      const b = toTime(e.clientX);
      onSel(null);
      if (Math.abs(b - a) >= Math.max(res.step * 2, 30)) onZoom({ a: Math.min(a, b), b: Math.max(a, b) });
    }
  };

  // Escape cancels a selection.
  useEffect(() => {
    if (!sel) return;
    const k = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        drag.current = null;
        onSel(null);
      }
    };
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, [sel, onSel]);

  const cx = cursor != null && cursor < t.length ? x(t[cursor]) : null;

  return (
    <figure className="pchart">
      <figcaption>
        <span className="ptitle">{panel.title}</span>
        <span className="plegend">
          {lines.map((l, k) => (
            <span key={l.id} className="pkey">
              <svg width="26" height="10" aria-hidden="true">
                <line x1="1" x2="25" y1="5" y2="5" stroke={`var(--pf-${k + 1})`} strokeWidth="2" strokeDasharray={DASH[k % DASH.length]} />
              </svg>
              {l.label}
            </span>
          ))}
        </span>
      </figcaption>
      <div ref={wrap as React.RefObject<HTMLDivElement>} className="pframe">
        <svg ref={svgRef} width={W} height={H} role="img" aria-label={panel.title} className="psvg"
          onPointerDown={down} onPointerMove={move} onPointerUp={up} onPointerCancel={() => { drag.current = null; onSel(null); }}
          onPointerLeave={(e) => { if (e.pointerType === "mouse" && !drag.current) onCursor(null); }}>
          {ticks.filter((v) => v <= topV).map((v) => (
            <g key={v}>
              <line x1={M.l} x2={W - M.r} y1={y(v)} y2={y(v)} className="pgrid" />
              <text x={M.l - 6} y={y(v) + 4} textAnchor="end" className="ptick">{unit === "pct" ? `${v}%` : fmt(unit, v)}</text>
            </g>
          ))}
          {xt.map((s) => (
            <g key={s}>
              <line x1={x(s)} x2={x(s)} y1={M.t + ph} y2={M.t + ph + 4} className="paxis" />
              <text x={x(s)} y={H - 6} textAnchor="middle" className="ptick">{long ? `${dayLabel(s).replace(/^\w+ /, "")} ${clock(s)}` : clock(s)}</text>
            </g>
          ))}
          <line x1={M.l} x2={W - M.r} y1={M.t + ph} y2={M.t + ph} className="paxis" />
          {lines.map((l, k) => {
            const col = `var(--pf-${k + 1})`;
            return (
              <g key={l.id}>
                {showPeak && l.max &&
                  runs(l.avg).map((r, j) => {
                    if (r.length < 2) return null;
                    const up = r.map((i) => `${x(t[i]).toFixed(1)},${y(l.max![i] ?? l.avg[i]!).toFixed(1)}`);
                    const dn = [...r].reverse().map((i) => `${x(t[i]).toFixed(1)},${y(l.avg[i]!).toFixed(1)}`);
                    return <path key={j} d={`M${up.join("L")}L${dn.join("L")}Z`} fill={col} opacity="0.18" />;
                  })}
                {runs(l.avg).map((r, j) =>
                  r.length === 1 ? (
                    <circle key={j} cx={x(t[r[0]])} cy={y(l.avg[r[0]]!)} r="2" fill={col} />
                  ) : (
                    <path key={j} d={"M" + r.map((i) => `${x(t[i]).toFixed(1)},${y(l.avg[i]!).toFixed(1)}`).join("L")} fill="none" stroke={col} strokeWidth="2"
                      strokeDasharray={DASH[k % DASH.length]} strokeLinejoin="round" />
                  ),
                )}
              </g>
            );
          })}
          {res.events.filter((e) => e.t >= x0 && e.t <= x1).map((e, i) => (
            <Marker key={i} e={e} x={x(e.t)} y={M.t + ph} h={ph} />
          ))}
          {sel && <rect x={x(Math.min(sel.a, sel.b))} y={M.t} width={Math.abs(x(sel.b) - x(sel.a))} height={ph} className="psel" />}
          {cx != null && (
            <g pointerEvents="none">
              <line x1={cx} x2={cx} y1={M.t} y2={M.t + ph} className="pcross" />
              {lines.map((l, k) => {
                const v = l.avg[cursor!];
                return v == null ? null : <circle key={l.id} cx={cx} cy={y(v)} r="3.5" fill={`var(--pf-${k + 1})`} className="pdot" />;
              })}
            </g>
          )}
          {lines.length === 0 && <text x={M.l + pw / 2} y={M.t + ph / 2} textAnchor="middle" className="ptick">No data in this range</text>}
        </svg>
      </div>
    </figure>
  );
}

const MARK: Record<PerfEvent["kind"], string> = { started: "Started", stopped: "Stopped", migrated: "Moved to another node" };

function Marker({ e, x, y, h }: { e: PerfEvent; x: number; y: number; h: number }) {
  return (
    <g className="pevent">
      <title>{`${MARK[e.kind]} at ${clock(e.t)}`}</title>
      <line x1={x} x2={x} y1={y - h} y2={y} className="pevline" />
      {e.kind === "started" && <path d={`M${x - 4},${y - 8}L${x + 5},${y - 4}L${x - 4},${y}Z`} className="pevshape" />}
      {e.kind === "stopped" && <rect x={x - 4} y={y - 8} width="8" height="8" className="pevshape" />}
      {e.kind === "migrated" && <path d={`M${x},${y - 9}L${x + 5},${y - 4.5}L${x},${y}L${x - 5},${y - 4.5}Z`} className="pevshape" />}
    </g>
  );
}
