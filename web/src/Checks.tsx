import { ReactNode } from "react";
import "./checks.css";

/** One design language for results: a dot (green ok, yellow warning, red failed, hollow for info or not run), then plain text. */
export type CheckStatus = "ok" | "warning" | "error" | "info" | "pending";

export interface CheckItem {
  status: CheckStatus;
  title?: ReactNode;
  detail?: ReactNode;
  key?: string;
}

const NAME: Record<CheckStatus, string> = { ok: "ok", warning: "warning", error: "failed", info: "info", pending: "not run" };
const RANK: Record<CheckStatus, number> = { error: 0, warning: 1, info: 2, pending: 3, ok: 4 };

export function StatusDot({ status }: { status: CheckStatus }) {
  return <span className={`cdot c-${status}`} role="img" aria-label={NAME[status]} />;
}

/** One line: dot and text, for a single result. */
export function StatusLine({ status, children }: { status: CheckStatus; children: ReactNode }) {
  return (
    <p className="statusline" role="status">
      <StatusDot status={status} />
      <span>{children}</span>
    </p>
  );
}

/** "3 passed, 1 warning, 1 failed" */
export function checkSummary(items: { status: CheckStatus }[]): string {
  const n = (s: CheckStatus) => items.filter((i) => i.status === s).length;
  const parts: string[] = [];
  if (n("ok")) parts.push(`${n("ok")} passed`);
  if (n("warning")) parts.push(`${n("warning")} warning${n("warning") > 1 ? "s" : ""}`);
  if (n("error")) parts.push(`${n("error")} failed`);
  if (n("pending")) parts.push(`${n("pending")} not run`);
  return parts.join(", ");
}

/**
 * A list of results. Failures come first, then warnings, then the rest, unless
 * `sort` is false (steps that must stay in the order they run).
 */
export function CheckList({ items, sort = true, summary = true, label }: { items: CheckItem[]; sort?: boolean; summary?: boolean; label?: string }) {
  const rows = sort ? items.map((c, i) => [c, i] as const).sort((a, b) => RANK[a[0].status] - RANK[b[0].status] || a[1] - b[1]).map((x) => x[0]) : items;
  const sum = summary && items.length > 1 ? checkSummary(items) : "";
  return (
    <>
      {sum && <p className="checksum">{sum}</p>}
      <ul className="checklist" aria-label={label}>
        {rows.map((c, i) => (
          <li key={c.key ?? i}>
            <StatusDot status={c.status} />
            <span className="ctext">
              {c.title ? <strong>{c.title}</strong> : null}
              {c.title && c.detail ? " " : null}
              {c.detail}
            </span>
          </li>
        ))}
      </ul>
    </>
  );
}

/** The server's pre-flight shape ({check, ok, severity, message}) as list items. */
export function preflightItems(checks: { check: string; ok: boolean; severity: string; message: string }[]): CheckItem[] {
  return checks.map((c) => ({ status: c.ok ? "ok" : c.severity === "error" ? "error" : "warning", title: c.check, detail: c.message }));
}
