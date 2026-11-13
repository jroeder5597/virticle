// The Performance tab of a VM or a node, and the "busiest virtual machines" table.
// The tab loads its range once; only a range that ends now reloads, at most once a minute and never while
// the page is hidden. There is no polling of the live statistics here.
import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { get } from "../api";
import type { NodeStatus } from "../api";
import Select from "../Select";
import { ErrorBox } from "../ui";
import PerfChart, { Sel } from "./PerfChart";
import {
  Busy, GROUP_LABEL, Group, PerfOverview, PerfResult, RANGES, fmt, fromLocalInput, linesOf, panelsFor, stamp, stepWords, toLocalInput, unitFor, valueScale,
} from "./perfApi";
import "./perf.css";

interface Range {
  key: string; // 1h | 6h | 24h | 7d | custom
  from: number;
  to: number;
}

/** The range in the address: ?range=6h, or ?range=custom&from=..&to=.. in unix seconds. */
function useRange(): [Range, (r: { key: string; from?: number; to?: number }) => void, () => Range] {
  const [params, setParams] = useSearchParams();
  const key = params.get("range") ?? "1h";
  const f = Number(params.get("from"));
  const t = Number(params.get("to"));
  const resolve = useCallback((): Range => {
    if (key === "custom" && f > 0 && t > f) return { key, from: f, to: t };
    const secs = (RANGES.find((r) => r[0] === key) ?? RANGES[0])[2];
    const now = Math.floor(Date.now() / 1000);
    return { key: RANGES.some((r) => r[0] === key) ? key : "1h", from: now - secs, to: now };
  }, [key, f, t]);
  const set = (r: { key: string; from?: number; to?: number }) => {
    const p = new URLSearchParams(params);
    p.set("range", r.key);
    if (r.key === "custom") {
      p.set("from", String(r.from));
      p.set("to", String(r.to));
    } else {
      p.delete("from");
      p.delete("to");
    }
    setParams(p, { replace: true });
  };
  return [useMemo(resolve, [resolve]), set, resolve];
}

/** The query string that carries a range to another Performance tab. */
export function rangeQuery(r: Range): string {
  return r.key === "custom" ? `range=custom&from=${r.from}&to=${r.to}` : `range=${r.key}`;
}

export function PerformanceTab(props: { kind: "vm" | "node"; id: string; passthrough?: boolean; vmName?: string }) {
  const { kind, id } = props;
  const [, setRange, resolve] = useRange();
  const [params] = useSearchParams();
  const key = params.get("range") ?? "1h";
  const customFrom = params.get("from");
  const customTo = params.get("to");
  const [res, setRes] = useState<PerfResult>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [off, setOff] = useState<Set<Group>>(new Set());
  const [diskDev, setDiskDev] = useState("");
  const [netDev, setNetDev] = useState("");
  const [cursor, setCursor] = useState<number | null>(null);
  const [sel, setSel] = useState<Sel | null>(null);
  const [back, setBack] = useState<{ key: string; from?: number; to?: number }[]>([]);
  const [shown, setShown] = useState<Range>();

  const device = [diskDev, netDev].filter(Boolean).join(",");
  const relative = key !== "custom";

  const load = useCallback(
    (quiet: boolean) => {
      const r = resolve();
      if (!quiet) setBusy(true);
      get<PerfResult>(`/metrics/query?kind=${kind}&id=${encodeURIComponent(id)}&from=${r.from}&to=${r.to}${device ? `&device=${encodeURIComponent(device)}` : ""}`)
        .then(
          (d) => {
            setRes(d);
            setShown(r);
            setError(undefined);
          },
          (e) => setError(e instanceof Error ? e.message : String(e)),
        )
        .finally(() => setBusy(false));
    },
    [kind, id, device, resolve],
  );

  useEffect(() => {
    setCursor(null);
    load(false);
  }, [load, key, customFrom, customTo]);

  // A range that ends now refreshes once a minute while the page is visible.
  useEffect(() => {
    if (!relative) return;
    let timer: ReturnType<typeof setInterval> | undefined;
    const sync = () => {
      clearInterval(timer);
      timer = undefined;
      if (document.visibilityState === "hidden") return;
      timer = setInterval(() => load(true), 60_000);
    };
    sync();
    document.addEventListener("visibilitychange", sync);
    return () => {
      clearInterval(timer);
      document.removeEventListener("visibilitychange", sync);
    };
  }, [relative, load]);

  const current = (): { key: string; from?: number; to?: number } =>
    key === "custom" ? { key, from: Number(customFrom), to: Number(customTo) } : { key };
  const zoom = (s: Sel) => {
    setBack((b) => [...b, current()]);
    setRange({ key: "custom", from: Math.floor(s.a), to: Math.ceil(s.b) });
  };
  const goBack = () => {
    const prev = back[back.length - 1];
    if (!prev) return;
    setBack(back.slice(0, -1));
    setRange(prev);
  };
  const pick = (v: string) => {
    setBack([]);
    if (v === "custom") {
      const r = resolve();
      setRange({ key: "custom", from: r.from, to: r.to });
    } else setRange({ key: v });
  };

  const groups: Group[] = useMemo(() => {
    const all: Group[] = kind === "vm" ? ["cpu", "memory", "disk", "network"] : ["cpu", "memory", "disk", "network", "storage", "gpu"];
    return all.filter((g) => (g !== "storage" && g !== "gpu") || (res?.devices[g]?.length ?? 0) > 0);
  }, [kind, res]);
  const toggle = (g: Group) => {
    const n = new Set(off);
    if (n.has(g)) n.delete(g);
    else n.add(g);
    setOff(n);
  };
  const panels = panelsFor(kind).filter((p) => groups.includes(p.group) && !off.has(p.group));

  const csv = shown
    ? `/api/metrics/query?kind=${kind}&id=${encodeURIComponent(id)}&from=${shown.from}&to=${shown.to}${device ? `&device=${encodeURIComponent(device)}` : ""}&format=csv`
    : "#";

  const diskDevs = res?.devices.disk ?? [];
  const netDevs = (res?.devices.network ?? []).filter((d) => d.kind !== "net" || kind === "node");
  const devOptions = (list: typeof diskDevs, all: string) => [
    { value: "", label: all },
    ...list.map((d) => ({ value: d.id, label: d.kind === "net" ? `${d.name} (bridge)` : d.name })),
  ];

  const hasData = res ? res.t.length > 0 && Object.values(res.series).some((s) => s.some((v) => v != null)) : false;

  return (
    <div className="perf">
      <div className="pctl">
        <label className="pfield">
          <span>Time range</span>
          <Select
            label="Time range"
            auto
            value={key === "custom" ? "custom" : RANGES.some((r) => r[0] === key) ? key : "1h"}
            onChange={pick}
            options={[...RANGES.map((r) => ({ value: r[0] as string, label: r[1] as string })), { value: "custom", label: "Custom" }]}
          />
        </label>
        {key === "custom" && customFrom && customTo && (
          <>
            <label className="pfield">
              <span>From</span>
              <input type="datetime-local" value={toLocalInput(Number(customFrom))} max={toLocalInput(Number(customTo))}
                onChange={(e) => { const s = fromLocalInput(e.target.value); if (s && s < Number(customTo)) setRange({ key: "custom", from: s, to: Number(customTo) }); }} />
            </label>
            <label className="pfield">
              <span>To</span>
              <input type="datetime-local" value={toLocalInput(Number(customTo))} min={toLocalInput(Number(customFrom))}
                onChange={(e) => { const s = fromLocalInput(e.target.value); if (s && s > Number(customFrom)) setRange({ key: "custom", from: Number(customFrom), to: s }); }} />
            </label>
          </>
        )}
        {diskDevs.length > 1 && !off.has("disk") && (
          <label className="pfield">
            <span>Disk</span>
            <Select label="Disk" auto value={diskDev} onChange={setDiskDev} options={devOptions(diskDevs, "All (sum)")} />
          </label>
        )}
        {netDevs.length > 1 && !off.has("network") && (
          <label className="pfield">
            <span>Network card</span>
            <Select label="Network card" auto value={netDev} onChange={setNetDev} options={devOptions(netDevs, "All (sum)")} />
          </label>
        )}
        <span className="pspacer" />
        {back.length > 0 && <button className="ghost" onClick={goBack}>Back</button>}
        <a className="ghost pdl" href={csv} download>Download CSV</a>
      </div>

      <div className="pchips" role="group" aria-label="Metrics">
        {groups.map((g) => (
          <button key={g} className={"ghost pchip" + (off.has(g) ? "" : " on")} aria-pressed={!off.has(g)} onClick={() => toggle(g)}>
            {GROUP_LABEL[g]}
          </button>
        ))}
      </div>

      <ErrorBox error={error} />
      {kind === "vm" && props.passthrough && (
        <p className="hint">This VM has passed-through devices. If one is a GPU, its usage is only visible inside the guest.</p>
      )}

      {res && (
        <>
          <p className="pnote">
            {shown && `${stamp(shown.from)} to ${stamp(shown.to)}. `}
            1 point = {stepWords(res.step)}
            {res.peak ? "; the band shows the peak." : "."} Gaps mean no data
            {kind === "vm" ? ": the VM was not running, or the node or the controller could not be read." : ": the node or the controller could not be read."} Drag across a chart to zoom.
            {res.events.length > 0 && " Markers on the axis: triangle started, square stopped, diamond moved to another node."}
          </p>
          <Readout res={res} panels={panels} cursor={cursor} />
          {!hasData && !busy && <div className="empty">No data in this range yet. The controller records every 15 seconds while it runs.</div>}
          {hasData &&
            panels.map((p) => (
              <PerfChart key={p.title} panel={p} res={res} cursor={cursor} onCursor={setCursor} sel={sel} onSel={setSel} onZoom={zoom} />
            ))}
        </>
      )}
      {!res && !error && <div className="empty">Loading</div>}
      {kind === "node" && <BusiestVMs node={Number(id)} />}
    </div>
  );
}

function Readout({ res, panels, cursor }: { res: PerfResult; panels: ReturnType<typeof panelsFor>; cursor: number | null }) {
  if (cursor == null || cursor >= res.t.length) {
    return <div className="preadout muted">Move over a chart, or tap it, to read the values at that time.</div>;
  }
  const t = res.t[cursor];
  const ev = res.events.filter((e) => Math.abs(e.t - t) <= res.step);
  return (
    <div className="preadout" aria-live="off">
      <b>{stamp(t, res.step < 60)}</b>
      {ev.map((e, i) => (
        <span key={i} className="pevtxt">{e.kind === "started" ? "Started" : e.kind === "stopped" ? "Stopped" : "Moved to another node"}</span>
      ))}
      <div className="prgrid">
        {panels.map((p) => {
          const unit = unitFor(p);
          const lines = linesOf(p, res);
          if (!lines.length) return null;
          return (
            <div key={p.title} className="prp">
              <span className="muted">{p.title.replace(/ \(.*$/, "")}</span>
              {lines.map((l) => (
                <span key={l.id}>
                  {l.label} <b>{fmt(unit, valueScale(p, l.avg[cursor]))}</b>
                  {res.peak && l.max && l.max[cursor] != null && <span className="muted"> (peak {fmt(unit, valueScale(p, l.max[cursor]))})</span>}
                </span>
              ))}
            </div>
          );
        })}
      </div>
    </div>
  );
}

// --- busiest virtual machines ---

const BUSY: ["cpu" | "memory" | "disk" | "network", string, (v: number) => string][] = [
  ["cpu", "CPU", (v) => fmt("pct", v)],
  ["memory", "Memory used", (v) => fmt("kib", v)],
  ["disk", "Disk", (v) => fmt("bytes", v)],
  ["network", "Network", (v) => fmt("bps", v)],
];

/** Top 5 per metric for a range; node 0 means every node. Each row opens that VM's Performance tab with the same range. */
export function BusiestVMs({ node, title }: { node: number; title?: string }) {
  const [range] = useRange();
  const [o, setO] = useState<PerfOverview>();
  const [error, setError] = useState<string>();
  const [nodes, setNodes] = useState<Record<number, string>>({});
  useEffect(() => {
    if (node !== 0) return;
    get<{ nodes: NodeStatus[] }>("/nodes").then(
      (r) => setNodes(Object.fromEntries(r.nodes.map((n) => [n.id, n.hostname || n.addr]))),
      () => {},
    );
  }, [node]);
  useEffect(() => {
    get<PerfOverview>(`/metrics/overview?from=${range.from}&to=${range.to}${node ? `&node=${node}` : ""}&top=5`).then(
      (d) => { setO(d); setError(undefined); },
      (e) => setError(e instanceof Error ? e.message : String(e)),
    );
  }, [range, node]);
  const q = rangeQuery(range);
  return (
    <section className="pbusy">
      <h2>{title ?? "Busiest virtual machines in this range"}</h2>
      <ErrorBox error={error} />
      {o && (
        <div className="pbgrid">
          {BUSY.map(([k, label, f]) => {
            const rows: Busy[] = o[k];
            return (
              <div key={k} className="pbcol">
                <h3>{label}</h3>
                {rows.length === 0 && <div className="muted">No activity</div>}
                {rows.length > 0 && (
                  <table className="compact">
                    <thead><tr><th>Virtual machine</th><th className="num">Average</th><th className="num">Peak</th></tr></thead>
                    <tbody>
                      {rows.map((r) => (
                        <tr key={r.uuid}>
                          <td>
                            <Link to={`/vm/${r.node}/${r.uuid}?tab=performance&${q}`}>{r.name}</Link>
                            {node === 0 && nodes[r.node] && <div className="muted small">{nodes[r.node]}</div>}
                          </td>
                          <td className="num">{f(r.avg)}</td>
                          <td className="num">{f(r.max)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

/** The Datacenter tab: the same ranking for all nodes, under the same range controls but without charts. */
export function DatacenterPerformance() {
  const [, setRange] = useRange();
  const [params] = useSearchParams();
  const key = params.get("range") ?? "1h";
  return (
    <div className="perf">
      <div className="pctl">
        <label className="pfield">
          <span>Time range</span>
          <Select
            label="Time range"
            auto
            value={RANGES.some((r) => r[0] === key) ? key : "1h"}
            onChange={(v) => setRange({ key: v })}
            options={RANGES.map((r) => ({ value: r[0] as string, label: r[1] as string }))}
          />
        </label>
      </div>
      <BusiestVMs node={0} title="Busiest virtual machines in this range" />
    </div>
  );
}
