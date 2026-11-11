// The snapshots of a VM as a tree: connector lines, a marker for the current snapshot and a "You are here" node below it.
// Used for the Snapshots tab (with Revert and Delete per row) and inside the delete dialog (with a check box per row).
import { useRef, useState } from "react";
import { Clock, GitBranchPlus, RotateCcw, Trash2 } from "lucide-react";
import { StateBadge } from "../ui";
import { branchOf, buildSnapRows, TreeRow, TreeSnap } from "../snapTree";
import "./snaptree.css";
import { StatusDot } from "../Checks";

export interface SnapNode extends TreeSnap {
  description?: string;
  state: string;
  hasMemory: boolean;
  kind: "internal" | "external";
  files: string[];
  scheduled?: boolean;
  deletable?: boolean;
  reason?: string;
}

const when = (d: Date) => d.toLocaleString();

/** "3 hours ago", "in 2 days" for a title attribute. */
export function relative(unixSec: number, now = Date.now()): string {
  const s = Math.round((unixSec * 1000 - now) / 1000);
  const f = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const a = Math.abs(s);
  if (a < 90) return f.format(s, "second");
  if (a < 5400) return f.format(Math.round(s / 60), "minute");
  if (a < 129600) return f.format(Math.round(s / 3600), "hour");
  return f.format(Math.round(s / 86400), "day");
}

const typeText = (s: SnapNode) => `${s.kind}, ${s.hasMemory ? "with memory" : "disk only"}`;

export interface SnapTreeProps<T extends SnapNode = SnapNode> {
  list: T[];
  vmState: string;
  /** Row actions (Snapshots tab) or check boxes (delete dialog). */
  mode: "view" | "select";
  onRevert?: (s: T) => void;
  onDelete?: (s: T) => void;
  selected?: Set<string>;
  onToggle?: (s: T, on: boolean) => void;
  onBranch?: (names: string[]) => void;
  /** Rows that are shown but cannot be ticked, with the reason. */
  blocked?: (s: T) => string | undefined;
  ariaLabel: string;
}

export default function SnapTree<T extends SnapNode>(p: SnapTreeProps<T>) {
  const rows = buildSnapRows(p.list, true);
  const [active, setActive] = useState(0);
  const box = useRef<HTMLDivElement>(null);
  const cur = Math.min(active, rows.length - 1);
  const focusRow = (i: number) => {
    const j = Math.max(0, Math.min(rows.length - 1, i));
    setActive(j);
    box.current?.querySelector<HTMLElement>(`[data-i="${j}"]`)?.focus();
  };
  const onKey = (e: React.KeyboardEvent, i: number) => {
    if ((e.target as HTMLElement).closest("button, input")) return;
    const r = rows[i];
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        return focusRow(i + 1);
      case "ArrowUp":
        e.preventDefault();
        return focusRow(i - 1);
      case "Home":
        e.preventDefault();
        return focusRow(0);
      case "End":
        e.preventDefault();
        return focusRow(rows.length - 1);
      case "ArrowLeft":
        e.preventDefault();
        return focusRow(r.parent >= 0 ? r.parent : i);
      case "ArrowRight":
        e.preventDefault();
        return focusRow(i + 1 < rows.length && rows[i + 1].depth > r.depth ? i + 1 : i);
      case " ":
        if (p.mode === "select" && r.snap && !p.blocked?.(r.snap)) {
          e.preventDefault();
          p.onToggle?.(r.snap, !p.selected?.has(r.name));
        }
        return;
      case "b":
      case "B":
        if (p.mode === "select" && r.snap && r.descendants > 0) {
          e.preventDefault();
          p.onBranch?.(branchOf(rows, r.name));
        }
    }
  };

  return (
    <div className="streewrap">
      <div className="stree-scroll" ref={box}>
        <div className="stree" role="tree" aria-label={p.ariaLabel} aria-multiselectable={p.mode === "select"}>
          {rows.map((r, i) => (
            <Row key={r.kind + r.name} r={r} i={i} focused={i === cur} p={p} onFocus={() => setActive(i)} onKey={(e) => onKey(e, i)} rows={rows} />
          ))}
        </div>
      </div>
    </div>
  );
}

function Row<T extends SnapNode>({ r, i, focused, p, onFocus, onKey, rows }: { r: TreeRow<T>; i: number; focused: boolean; p: SnapTreeProps<T>; onFocus: () => void; onKey: (e: React.KeyboardEvent) => void; rows: TreeRow<T>[] }) {
  const gut = (
    <span className="sgut" aria-hidden="true">
      {r.depth > 0 && r.trunk.map((t, k) => <span key={k} className={"gc" + (t ? " t" : "")} />)}
      {r.depth > 0 && <span className={"gc e" + (r.isLast ? "" : " more")} />}
    </span>
  );
  if (r.kind === "now") {
    return (
      <div className="srow now" role="treeitem" aria-level={r.depth + 1} aria-label="You are here: the running state" tabIndex={focused ? 0 : -1} data-i={i} onFocus={onFocus} onKeyDown={onKey}>
        {gut}
        <span className="snode"><span className="dot" aria-hidden="true" /></span>
        <span className="smain">
          <span className="sline">
            <span className="sname">You are here</span>
            <StateBadge state={p.vmState} />
          </span>
          <span className="sdesc">Running state</span>
        </span>
      </div>
    );
  }
  const s = r.snap!;
  const block = p.mode === "select" ? p.blocked?.(s) : undefined;
  const on = p.selected?.has(s.name) ?? false;
  const tab = focused ? 0 : -1;
  return (
    <div
      className={"srow" + (s.isCurrent ? " cur" : "") + (block ? " blocked" : "")}
      role="treeitem"
      aria-level={r.depth + 1}
      aria-selected={p.mode === "select" ? on : undefined}
      aria-label={`${s.name}${s.isCurrent ? ", current" : ""}`}
      data-sel={p.mode === "select" ? on : undefined}
      tabIndex={tab}
      data-i={i}
      onFocus={onFocus}
      onKeyDown={onKey}
    >
      {gut}
      <span className="snode"><span className="dot" aria-hidden="true" /></span>
      <span className="smain">
        <span className="sline">
          <span className="sname" title={s.name}>{s.name}</span>
          {s.isCurrent && <span className="badge s-running">Current</span>}
          {s.scheduled && <Clock size={13} aria-label="Made by the schedule" />}
        </span>
        {block ? <span className="sreason" title={block}><StatusDot status="warning" />{block}</span> : s.description && !s.scheduled ? <span className="sdesc" title={s.description}>{s.description}</span> : <span className="sdesc" title={s.files.join("\n")}>{s.scheduled ? "Taken by the schedule" : s.files.length ? s.files.join(", ") : " "}</span>}
      </span>
      <span className="smeta">
        <span className="when" title={relative(s.created)}>{when(new Date(s.created * 1000))}</span>
        <span title={s.files.join("\n")}>{typeText(s)}</span>
        <StateBadge state={s.state} />
      </span>
      <span className="sact">
        {p.mode === "view" ? (
          <>
            <button className="iconbtn" tabIndex={tab} aria-label={`Revert to ${s.name}`} title="Revert" onClick={() => p.onRevert?.(s)}>
              <RotateCcw size={15} aria-hidden="true" />
            </button>
            <button className="iconbtn" tabIndex={tab} aria-label={`Delete ${s.name}`} title="Delete" onClick={() => p.onDelete?.(s)}>
              <Trash2 size={15} aria-hidden="true" />
            </button>
          </>
        ) : (
          <>
            {r.descendants > 0 && (
              <button className="iconbtn" tabIndex={tab} aria-label={`Select ${s.name} and everything below it`} title="Select this and all below" onClick={() => p.onBranch?.(branchOf(rows, s.name))}>
                <GitBranchPlus size={15} aria-hidden="true" />
              </button>
            )}
            <input type="checkbox" tabIndex={tab} aria-label={`Select ${s.name}`} checked={on} disabled={!!block} onChange={(e) => p.onToggle?.(s, e.target.checked)} />
          </>
        )}
      </span>
    </div>
  );
}
