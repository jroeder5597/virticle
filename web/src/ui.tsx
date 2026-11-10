// Small shared building blocks.
import { ReactNode, useEffect, useRef, useState } from "react";

export function Modal(props: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && props.onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
  // Put focus inside the dialog unless a field already took it (autoFocus).
  useEffect(() => {
    const m = box.current;
    if (!m || m.contains(document.activeElement)) return;
    m.querySelector<HTMLElement>("input:not([type=hidden]):not([type=checkbox]):not([disabled]), select:not([disabled]), textarea:not([disabled])")?.focus();
  }, []);
  // Dialogs that are not <form>s still submit on Enter from a text field.
  const onEnter = (e: React.KeyboardEvent) => {
    const t = e.target as HTMLElement;
    if (e.key !== "Enter" || t.tagName !== "INPUT" || (t as HTMLInputElement).type === "checkbox" || t.closest("form")) return;
    const go = box.current?.querySelector<HTMLButtonElement>("footer button.primary, .actions button.primary");
    if (go && !go.disabled) {
      e.preventDefault();
      go.click();
    }
  };
  return (
    <div className="backdrop" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div ref={box} onKeyDown={onEnter} className={"modal" + (props.wide ? " wide" : "")} role="dialog" aria-label={props.title}>
        <header>
          <h2>{props.title}</h2>
          <button className="ghost" onClick={props.onClose} aria-label="Close dialog">
            ✕
          </button>
        </header>
        {props.children}
      </div>
    </div>
  );
}

export function ErrorBox({ error }: { error?: string }) {
  return error ? <div className="error">{error}</div> : null;
}

export function StateBadge({ state }: { state: string }) {
  return <span className={"badge s-" + state}>{state}</span>;
}

/** A labelled form row. `group` is for rows of buttons: a <label> around buttons would swallow the first one's name. */
export function Field(props: { label: string; hint?: string; children: ReactNode; group?: boolean }) {
  if (props.group) {
    return (
      <div className="field" role="group" aria-label={props.label}>
        <span>{props.label}</span>
        {props.children}
        {props.hint && <small>{props.hint}</small>}
      </div>
    );
  }
  return (
    <label className="field">
      <span>{props.label}</span>
      {props.children}
      {props.hint && <small>{props.hint}</small>}
    </label>
  );
}

/** Runs an async action, exposing busy and error state for a button or form. */
export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const run = async (fn: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true);
    setError(undefined);
    try {
      await fn();
      return true;
    } catch (e: any) {
      setError(String(e.message ?? e));
      return false;
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, run, clear: () => setError(undefined) };
}

export function UsageBar({ used, total }: { used: number; total: number }) {
  const pct = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="bar" title={`${pct.toFixed(0)}% used`}>
      <div className={pct > 90 ? "full" : ""} style={{ width: pct + "%" }} />
    </div>
  );
}

/** Task progress from 0 to 1. Unlike UsageBar, a full bar is good news and never turns red. */
export function Progress({ value, failed }: { value: number; failed?: boolean }) {
  const pct = Math.max(0, Math.min(1, value)) * 100;
  return (
    <div className="bar" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(pct)}>
      <div className={failed ? "full" : "done"} style={{ width: pct + "%" }} />
    </div>
  );
}

export function NodePicker(props: { nodes: { id: number; hostname?: string; addr: string; state: string }[]; value: number; onChange: (id: number) => void }) {
  return (
    <select value={props.value} onChange={(e) => props.onChange(Number(e.target.value))}>
      {props.nodes.map((n) => (
        <option key={n.id} value={n.id} disabled={n.state !== "online"}>
          {n.hostname || n.addr}
          {n.state !== "online" ? ` (${n.state})` : ""}
        </option>
      ))}
    </select>
  );
}
