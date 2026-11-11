// Pieces of a task row shared by the Tasks page and the Tasks dock.
import { Link } from "react-router-dom";
import type { Task } from "./api";
import { Inventory, taskLinks } from "./inventory";

export const absTime = (unix: number) => new Date(unix * 1000).toLocaleString();

/** "just now", "5 s ago", "3 min ago", "2 h ago", "4 d ago". */
export function relTime(unix: number, nowMs: number): string {
  const s = Math.max(0, Math.round(nowMs / 1000 - unix));
  if (s < 5) return "just now";
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** "12 s", "3 min 05 s", "1 h 02 min". A running task counts up to `nowMs`. */
export function duration(t: Task, nowMs: number): string {
  const end = t.status === "running" || !t.finished ? nowMs / 1000 : t.finished;
  const s = Math.max(0, Math.round(end - t.started));
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, "0")} s`;
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")} min`;
}

const WORD = { running: "Running", ok: "Ok", failed: "Error" } as const;

/** Dot and word; the dot colour comes from the shared s-* classes. */
export function TaskStatus({ status }: { status: Task["status"] }) {
  return (
    <span className={"tstatus s-" + status}>
      {WORD[status] ?? status}
    </span>
  );
}

/** The target, linked to its VM or node when the task record and the known inventory allow it. */
export function TaskTarget({ task, inv }: { task: Task; inv: Inventory }) {
  const to = taskLinks(inv, task.node, task.target).target;
  return to ? <Link to={to}>{task.target}</Link> : <>{task.target}</>;
}

export function TaskNode({ task, inv }: { task: Task; inv: Inventory }) {
  const to = taskLinks(inv, task.node, task.target).node;
  return to ? <Link to={to}>{task.node}</Link> : <>{task.node}</>;
}

/** Running first, then newest first. */
export function orderTasks(list: Iterable<Task>): Task[] {
  return [...list].sort((a, b) => Number(b.status === "running") - Number(a.status === "running") || b.started - a.started || b.id - a.id);
}
