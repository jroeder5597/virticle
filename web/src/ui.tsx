// Small shared building blocks.
import { createContext, ReactNode, useContext, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CircleHelp, X } from "lucide-react";
import Select from "./Select";
import "./menu.css";
import "./helptip.css";

// Open modals, oldest first: Escape closes only the topmost (a confirmation opened from a dialog).
const modalStack: object[] = [];

export function Modal(props: { title: string; onClose: () => void; children: ReactNode; wide?: boolean }) {
  const box = useRef<HTMLDivElement>(null);
  const me = useRef({}).current;
  useEffect(() => {
    modalStack.push(me);
    return () => {
      modalStack.splice(modalStack.indexOf(me), 1);
    };
  }, [me]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && modalStack[modalStack.length - 1] === me && props.onClose();
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
            <X size={16} aria-hidden="true" />
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

export function NodePicker(props: { nodes: { id: number; hostname?: string; addr: string; state: string }[]; value: number; onChange: (id: number) => void; label?: string; auto?: boolean }) {
  return (
    <Select
      label={props.label ?? "Node"}
      auto={props.auto}
      value={String(props.value)}
      onChange={(v) => props.onChange(Number(v))}
      options={props.nodes.map((n) => ({ value: String(n.id), label: (n.hostname || n.addr) + (n.state !== "online" ? ` (${n.state})` : ""), disabled: n.state !== "online", dot: "n-" + n.state }))}
    />
  );
}

export interface ConfirmSpec {
  title: string;
  /** What exactly will happen to which object. */
  message: ReactNode;
  /** The button label: the action itself ("Halt", "Delete volume"). */
  confirm: string;
  /** Objects affected, one per line (a bulk action lists its VMs). */
  items?: string[];
  /** Not styled as dangerous when false. */
  danger?: boolean;
}

/**
 * Confirmation step for a destructive action. Cancel has the focus, so Enter cancels rather than
 * confirming by accident; Escape cancels.
 */
export function ConfirmDialog(props: ConfirmSpec & { onResult: (ok: boolean) => void }) {
  return (
    <Modal title={props.title} onClose={() => props.onResult(false)}>
      <p>{props.message}</p>
      {props.items && props.items.length > 0 && (
        <ul className="confirm-items mono">
          {props.items.map((x) => (
            <li key={x}>{x}</li>
          ))}
        </ul>
      )}
      <footer>
        <button autoFocus onClick={() => props.onResult(false)}>
          Cancel
        </button>
        <button className={props.danger === false ? "primary" : "danger"} onClick={() => props.onResult(true)}>
          {props.confirm}
        </button>
      </footer>
    </Modal>
  );
}

/**
 * `const confirm = useConfirm()`, render `{confirm.dialog}` once, then
 * `if (!(await confirm.ask({...}))) return;` before the action.
 */
export function useConfirm() {
  const [state, setState] = useState<{ spec: ConfirmSpec; resolve: (ok: boolean) => void }>();
  const ask = (spec: ConfirmSpec) => new Promise<boolean>((resolve) => setState({ spec, resolve }));
  const dialog = state ? (
    <ConfirmDialog
      {...state.spec}
      onResult={(ok) => {
        setState(undefined);
        state.resolve(ok);
      }}
    />
  ) : null;
  return { ask, dialog };
}

export interface PromptSpec {
  title: string;
  label: string;
  hint?: string;
  initial?: string;
  confirm: string;
  type?: "text" | "password" | "number";
}

/** A one-field dialog in place of window.prompt: `const v = await prompt.ask({...})`, undefined when cancelled. */
export function usePrompt() {
  const [state, setState] = useState<{ spec: PromptSpec; resolve: (v: string | undefined) => void }>();
  const [value, setValue] = useState("");
  const ask = (spec: PromptSpec) =>
    new Promise<string | undefined>((resolve) => {
      setValue(spec.initial ?? "");
      setState({ spec, resolve });
    });
  const finish = (v: string | undefined) => {
    state?.resolve(v);
    setState(undefined);
  };
  const dialog = state ? (
    <Modal title={state.spec.title} onClose={() => finish(undefined)}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          finish(value);
        }}
      >
        <Field label={state.spec.label} hint={state.spec.hint}>
          <input autoFocus type={state.spec.type ?? "text"} value={value} onChange={(e) => setValue(e.target.value)} />
        </Field>
        <footer>
          <button type="button" onClick={() => finish(undefined)}>
            Cancel
          </button>
          <button className="primary" disabled={!value.trim()}>
            {state.spec.confirm}
          </button>
        </footer>
      </form>
    </Modal>
  ) : null;
  return { ask, dialog };
}

/**
 * A small circled question mark that explains something. The text appears on hover, keyboard focus and
 * click, stays inside the viewport and closes on Escape (only the tip, not the dialog around it), on blur
 * and when the pointer leaves.
 */
export function HelpTip(props: { label: string; children: ReactNode }) {
  const id = useId();
  const [hover, setHover] = useState(false);
  const [focus, setFocus] = useState(false);
  const [pinned, setPinned] = useState(false);
  const open = hover || focus || pinned;
  const btn = useRef<HTMLButtonElement>(null);
  const [pos, setPos] = useState<{ left: number; top?: number; bottom?: number; width: number; maxH: number }>();
  useLayoutEffect(() => {
    if (!open || !btn.current) return;
    const r = btn.current.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const width = Math.min(380, vw - 16);
    const left = Math.max(8, Math.min(r.left + r.width / 2 - width / 2, vw - width - 8));
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    setPos(below >= above || below >= 260 ? { left, top: r.bottom, width, maxH: below } : { left, bottom: vh - r.top, width, maxH: above });
  }, [open]);
  useEffect(() => {
    if (!open) return;
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      setHover(false);
      setFocus(false);
      setPinned(false);
    };
    window.addEventListener("keydown", onEsc, true);
    return () => window.removeEventListener("keydown", onEsc, true);
  }, [open]);
  return (
    <span
      className="helptip"
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => {
        setHover(false);
        setPinned(false);
      }}
    >
      <button
        ref={btn}
        type="button"
        className="helptip-btn"
        aria-label={props.label}
        aria-describedby={open ? id : undefined}
        aria-expanded={open}
        onFocus={() => setFocus(true)}
        onBlur={() => {
          setFocus(false);
          setPinned(false);
        }}
        onClick={() => setPinned((p) => !p)}
      >
        <CircleHelp size={15} aria-hidden="true" />
      </button>
      {open &&
        createPortal(
          <div id={id} role="tooltip" className="helptip-pop" style={pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, width: pos.width, maxHeight: pos.maxH } : { visibility: "hidden" }}>
            {props.children}
          </div>,
          document.body,
        )}
    </span>
  );
}

/** True inside the Datacenter tabs, where the page itself owns the h1 and tab headings drop to h2. */
export const EmbeddedContext = createContext(false);
export function PageTitle({ children }: { children: ReactNode }) {
  return useContext(EmbeddedContext) ? <h2>{children}</h2> : <h1>{children}</h1>;
}
