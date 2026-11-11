// Docked Tasks panel under the main content, like "Recent Tasks" in vSphere or Proxmox.
// Resource use: one GET /api/tasks when the dock mounts, then rows are patched from the task events on the
// event stream (no refetch per event). Renders are throttled to a few per second and wait while the tab is
// hidden. A collapsed dock keeps only the running count and renders no table. The only timer ticks every
// few seconds while the dock is open, visible and something is running, so durations count up.
import { ChevronDown, ChevronUp } from "lucide-react";
import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { get, Task } from "./api";
import { Inventory, useInventory } from "./inventory";
import { useEvents } from "./live";
import { setPref, usePref } from "./prefs";
import { absTime, duration, orderTasks, relTime, TaskNode, TaskStatus, TaskTarget } from "./taskView";
import "./tasksdock.css";

const SHOWN = 50; // rows kept in the DOM
const KEPT = 150; // tasks kept in memory
const THROTTLE_MS = 400;
const TICK_MS = 5000;

const hidden = () => document.visibilityState === "hidden";
const isPhone = () => typeof matchMedia === "function" && matchMedia("(max-width: 760px)").matches;

/** Open on desktop and closed on a phone until the user chooses. */
export function useDockOpen(): boolean {
  const v = usePref("tasksDock");
  return v ? v === "open" : !isPhone();
}

interface RowProps {
  t: Task;
  inv: Inventory;
  now: number;
  expanded: boolean;
  toggle: (id: number) => void;
}

const Row = memo(function Row({ t, inv, now, expanded, toggle }: RowProps) {
  return (
    <tr data-status={t.status}>
      <td>
        <TaskStatus status={t.status} />
      </td>
      <td className="c-kind">{t.kind}</td>
      <td className="c-target">
        <TaskTarget task={t} inv={inv} />
      </td>
      <td className="c-node">
        <TaskNode task={t} inv={inv} />
      </td>
      <td className="c-user">{t.user}</td>
      <td className="c-when num" title={absTime(t.started)}>
        {relTime(t.started, now)}
      </td>
      <td className="c-dur num">{duration(t, now)}</td>
      <td className="c-res">
        {t.status === "running" ? (
          <span className="dk-prog" title={t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : "no progress reported"}>
            <span className="dk-bar">
              <span style={{ width: `${Math.round(Math.min(1, t.progress) * 100)}%` }} />
            </span>
            <span className="num">{t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : ""}</span>
          </span>
        ) : t.error ? (
          <button type="button" className={"dk-err" + (expanded ? " open" : "")} title={t.error} aria-expanded={expanded} onClick={() => toggle(t.id)}>
            {t.error}
          </button>
        ) : null}
      </td>
    </tr>
  );
});

export default function TasksDock() {
  const open = useDockOpen();
  const inv = useInventory();
  const tasks = useRef(new Map<number, Task>());
  const [tick, setTick] = useState(0);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [expanded, setExpanded] = useState<Set<number>>(new Set());
  const [visible, setVisible] = useState(!hidden());
  const fetchedAt = useRef(0);
  const timer = useRef<number>();
  const lastRender = useRef(0);
  const stale = useRef(false); // events arrived while hidden
  const needLoad = useRef(false); // the socket reconnected while hidden

  const render = useCallback(() => {
    if (hidden()) {
      stale.current = true;
      return;
    }
    if (timer.current) return;
    const wait = Math.max(0, THROTTLE_MS - (Date.now() - lastRender.current));
    timer.current = window.setTimeout(() => {
      timer.current = undefined;
      lastRender.current = Date.now();
      setTick((n) => n + 1);
    }, wait);
  }, []);

  const load = useCallback(() => {
    fetchedAt.current = Date.now();
    get<Task[]>("/tasks").then(
      (list) => {
        const next = new Map(list.map((t) => [t.id, t]));
        const top = list.reduce((m, t) => Math.max(m, t.id), 0);
        // Events that arrived while the request was in flight can be newer than the answer.
        tasks.current.forEach((t, id) => id > top && next.set(id, t));
        tasks.current = next;
        setError("");
        setLoaded(true);
        render();
      },
      (e) => {
        setError(String(e?.message ?? e));
        setLoaded(true);
      },
    );
  }, [render]);

  useEffect(() => {
    load();
    return () => window.clearTimeout(timer.current);
  }, [load]);

  useEvents((e) => {
    if (e.type === "task" && e.task) {
      const m = tasks.current;
      m.set(e.task.id, e.task);
      if (m.size > KEPT) {
        const drop = orderTasks(m.values()).slice(KEPT);
        drop.forEach((t) => t.status !== "running" && m.delete(t.id));
      }
      render();
    } else if (e.type === "node" && e.detail === "resync" && Date.now() - fetchedAt.current > 3000) {
      // The socket was down: anything may have changed.
      if (hidden()) needLoad.current = true;
      else load();
    }
  });

  useEffect(() => {
    const on = () => {
      const v = !hidden();
      setVisible(v);
      if (!v) return;
      if (needLoad.current) {
        needLoad.current = false;
        stale.current = false;
        load();
      } else if (stale.current) {
        stale.current = false;
        render();
      }
    };
    document.addEventListener("visibilitychange", on);
    return () => document.removeEventListener("visibilitychange", on);
  }, [load, render]);

  let running = 0;
  tasks.current.forEach((t) => t.status === "running" && running++);

  // Durations and relative times only move while something is running.
  const live = open && visible && running > 0;
  useEffect(() => {
    if (!live) return;
    const id = window.setInterval(() => setTick((n) => n + 1), TICK_MS);
    return () => window.clearInterval(id);
  }, [live]);

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const rows = useMemo(() => (open ? orderTasks(tasks.current.values()).slice(0, SHOWN) : []), [open, tick]);
  const now = Math.floor(Date.now() / TICK_MS) * TICK_MS; // coarse, so rows that did not change are not redrawn
  const toggle = useCallback(
    (id: number) =>
      setExpanded((s) => {
        const n = new Set(s);
        n.has(id) ? n.delete(id) : n.add(id);
        return n;
      }),
    [],
  );

  return (
    <section className={"dock " + (open ? "open" : "closed")} aria-label="Tasks panel">
      <div className="dock-bar">
        <span className="dock-title">Tasks</span>
        <span className={"dock-count" + (running ? " busy" : "")}>{running} running</span>
        <Link to="/datacenter/tasks" className="dock-all">
          All tasks
        </Link>
        <span className="dock-spacer" />
        <button type="button" className="dock-toggle" aria-expanded={open} aria-controls="tasks-dock-body" aria-label={open ? "Hide tasks" : "Show tasks"} title={open ? "Hide tasks" : "Show tasks"} onClick={() => setPref("tasksDock", open ? "closed" : "open")}>
          {open ? <ChevronDown size={16} aria-hidden /> : <ChevronUp size={16} aria-hidden />}
        </button>
      </div>
      {open && (
        <div className="dock-body" id="tasks-dock-body">
          {error ? (
            <div className="dock-note err">{error}</div>
          ) : !loaded ? (
            <div className="dock-note">Loading</div>
          ) : rows.length === 0 ? (
            <div className="dock-note">Nothing has been done yet.</div>
          ) : (
            <table className="dock-table">
              <thead>
                <tr>
                  <th>Status</th>
                  <th className="c-kind">Action</th>
                  <th className="c-target">Target</th>
                  <th className="c-node">Node</th>
                  <th className="c-user">By</th>
                  <th className="c-when">Started</th>
                  <th className="c-dur">Duration</th>
                  <th className="c-res">Result</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((t) => (
                  <Row key={t.id} t={t} inv={inv} now={now} expanded={expanded.has(t.id)} toggle={toggle} />
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </section>
  );
}
