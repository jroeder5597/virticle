// Small shared building blocks.
import { ReactNode, useEffect, useState } from "react";

export function Modal(props: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && props.onClose();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
  return (
    <div className="backdrop" onMouseDown={(e) => e.target === e.currentTarget && props.onClose()}>
      <div className={"modal" + (props.wide ? " wide" : "")} role="dialog" aria-label={props.title}>
        <header>
          <h2>{props.title}</h2>
          <button className="ghost" onClick={props.onClose} aria-label="Close">
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

export function Field(props: { label: string; hint?: string; children: ReactNode }) {
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
