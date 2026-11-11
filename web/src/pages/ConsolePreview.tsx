// The "Console" card on the VM overview: a still preview that refreshes only
// while it is on screen, and opens the console in a new tab or inside the card.
// No noVNC connection is held for the preview itself.
import { useCallback, useEffect, useRef, useState } from "react";
import { Minimize2, RefreshCw, Settings2 } from "lucide-react";
import type { VMDetail } from "../api";
import { useRole } from "../auth";
import { CLICK_LABELS, ClickMode, useClickMode } from "../consolePrefs";
import { SerialConsole, VNCConsole } from "./Console";
import "./console.css";

const REFRESH_MS = 10000;

/** True while the element intersects the viewport. */
function useOnScreen(ref: React.RefObject<Element | null>, on: boolean): boolean {
  const [seen, setSeen] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!on || !el || typeof IntersectionObserver === "undefined") {
      setSeen(false);
      return;
    }
    const io = new IntersectionObserver((es) => setSeen(es[es.length - 1].isIntersecting));
    io.observe(el);
    return () => io.disconnect();
  }, [ref, on]);
  return seen;
}

function useTabVisible(): boolean {
  const [v, setV] = useState(document.visibilityState !== "hidden");
  useEffect(() => {
    const f = () => setV(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", f);
    return () => document.removeEventListener("visibilitychange", f);
  }, []);
  return v;
}

interface Preview {
  url?: string;
  error?: string;
  at?: Date;
  busy: boolean;
  refresh: () => void;
}

/** Fetches the still image once when active, then every 10 s while active and (if repeat) the VM runs. */
function usePreview(node: number, uuid: string, active: boolean, repeat: boolean, boxRef: React.RefObject<HTMLElement | null>): Preview {
  const [url, setUrl] = useState<string>();
  const [error, setError] = useState<string>();
  const [at, setAt] = useState<Date>();
  const [busy, setBusy] = useState(false);
  const urlRef = useRef<string>();
  const ctl = useRef<AbortController>();
  const stopped = useRef(false); // the server said there is no screen (409): do not keep asking
  const last = useRef(0);

  const load = useCallback(
    async (force: boolean) => {
      if (!force && (stopped.current || Date.now() - last.current < 3000)) return;
      ctl.current?.abort();
      const c = (ctl.current = new AbortController());
      last.current = Date.now();
      setBusy(true);
      try {
        const px = (boxRef.current?.clientWidth ?? 640) * Math.min(window.devicePixelRatio || 1, 2);
        const w = Math.min(1280, Math.max(320, Math.ceil(px / 160) * 160));
        const res = await fetch(`/api/nodes/${node}/vms/${uuid}/screenshot?w=${w}&fmt=jpeg`, { signal: c.signal });
        if (!res.ok) {
          const msg = (await res.json().catch(() => null))?.error ?? `${res.status} ${res.statusText}`;
          if (res.status === 409) stopped.current = true;
          throw new Error(msg);
        }
        const next = URL.createObjectURL(await res.blob());
        if (urlRef.current) URL.revokeObjectURL(urlRef.current);
        urlRef.current = next;
        stopped.current = false;
        setUrl(next);
        setError(undefined);
        setAt(new Date());
      } catch (e) {
        if ((e as Error).name !== "AbortError") setError((e as Error).message);
      } finally {
        if (ctl.current === c) setBusy(false);
      }
    },
    [node, uuid, boxRef],
  );

  useEffect(() => {
    if (!active) return;
    stopped.current = false;
    load(false);
    if (!repeat) return () => ctl.current?.abort();
    const h = setInterval(() => load(false), REFRESH_MS);
    return () => {
      clearInterval(h);
      ctl.current?.abort();
    };
  }, [active, repeat, load]);

  useEffect(
    () => () => {
      if (urlRef.current) URL.revokeObjectURL(urlRef.current);
    },
    [],
  );
  const refresh = useCallback(() => {
    stopped.current = false;
    load(true);
  }, [load]);
  return { url, error, at, busy, refresh };
}

function ModeMenu() {
  const [mode, setMode] = useClickMode();
  const [open, setOpen] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => !box.current?.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", key);
    };
  }, [open]);
  return (
    <div className="menu" ref={box}>
      <button className="ghost" aria-haspopup="menu" aria-expanded={open} aria-label="Console preview click behaviour" title="What a click on the preview does" onClick={() => setOpen(!open)}>
        <Settings2 size={16} aria-hidden />
      </button>
      {open && (
        <div className="menu-list cc-menu" role="menu">
          <div className="muted" style={{ padding: "4px 10px" }}>Preview opens</div>
          {(Object.keys(CLICK_LABELS) as ClickMode[]).map((m) => (
            <button
              key={m}
              role="menuitemradio"
              aria-checked={mode === m}
              onClick={() => {
                setMode(m);
                setOpen(false);
              }}
            >
              {mode === m ? "● " : "○ "}
              {CLICK_LABELS[m]}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/** The same setting on the Account page. */
export function ConsolePrefCard() {
  const [mode, setMode] = useClickMode();
  return (
    <section className="card" aria-label="Console preview">
      <h2>Console preview</h2>
      <p className="muted">What a click on the console preview of a VM does. Kept in this browser only.</p>
      <div role="radiogroup" aria-label="Console preview opens">
        <strong>Console preview opens:</strong>
        {(Object.keys(CLICK_LABELS) as ClickMode[]).map((m) => (
          <label key={m} className="pref-opt">
            <input type="radio" name="console-click" checked={mode === m} onChange={() => setMode(m)} />
            {CLICK_LABELS[m]}
          </label>
        ))}
      </div>
    </section>
  );
}

export function ConsoleCard({ d }: { d: VMDetail }) {
  const viewer = useRole() === "viewer";
  const [mode] = useClickMode();
  const [inline, setInline] = useState(false);
  const boxRef = useRef<HTMLDivElement>(null);
  const alive = d.state === "running" || d.state === "paused";
  const kind: "vnc" | "serial" | null = d.hasVNC ? "vnc" : d.hasSerial ? "serial" : null;
  const canPreview = !viewer && alive && d.hasVNC;
  const onScreen = useOnScreen(boxRef, canPreview && !inline);
  const tabVisible = useTabVisible();
  const p = usePreview(d.node, d.uuid, canPreview && !inline && onScreen && tabVisible, d.state === "running", boxRef);
  useEffect(() => {
    if (!alive || viewer) setInline(false);
  }, [alive, viewer]);

  const open = () => {
    if (!kind) return;
    if (mode === "inline") setInline(true);
    else window.open(`/console/${d.node}/${d.uuid}${kind === "serial" ? "?kind=serial" : ""}`, "_blank", "noopener");
  };

  let note = "";
  if (viewer) note = "The console is available to operators and administrators.";
  else if (!alive) note = "The VM is shut off. Start it to see its console.";
  else if (!kind) note = "This VM has no display and no serial console.";
  else if (kind === "serial") note = "This VM has a serial console only. Click to open it.";

  if (inline && kind && !viewer) {
    const back = (
      <button onClick={() => setInline(false)} title="Disconnect and show the still preview" aria-label="Collapse">
        <Minimize2 size={15} aria-hidden />
        <span className="lbl">Collapse</span>
      </button>
    );
    return (
      <section className="card span cc" aria-label="Console">
        <div className="cc-head">
          <h3>Console</h3>
          <ModeMenu />
        </div>
        {kind === "vnc" ? <VNCConsole node={d.node} uuid={d.uuid} name={d.name} variant="inline" extra={back} /> : <SerialConsole node={d.node} uuid={d.uuid} variant="inline" extra={back} />}
      </section>
    );
  }

  const clickable = !!kind && !viewer && alive;
  return (
    <section className="card span cc" aria-label="Console">
      <div className="cc-head">
        <h3>Console</h3>
        {!viewer && <ModeMenu />}
      </div>
      <div className="cc-box" ref={boxRef}>
        {p.url && canPreview ? (
          <button className="cc-open" onClick={open} aria-label={`Open the console of ${d.name}`} title={mode === "inline" ? "Open the console here" : "Open the console in a new tab"}>
            <img src={p.url} alt={`Current screen of ${d.name}`} />
          </button>
        ) : clickable ? (
          <button className="cc-open" onClick={open} aria-label={`Open the console of ${d.name}`}>
            <span className="cc-note">{note || (p.error ? p.error : "Loading preview")}</span>
          </button>
        ) : (
          <div className="cc-note">{note}</div>
        )}
      </div>
      <div className="cc-foot">
        {canPreview ? (
          <>
            <button onClick={p.refresh} disabled={p.busy} aria-label="Refresh preview" title="Refresh preview">
              <RefreshCw size={14} aria-hidden /> Refresh
            </button>
            <span className="muted">
              {p.error ? `Preview failed: ${p.error}` : p.at ? `Updated ${p.at.toLocaleTimeString()}${d.state === "running" ? ", every 10 s while visible" : ""}` : " "}
            </span>
          </>
        ) : (
          <span className="muted">&nbsp;</span>
        )}
      </div>
    </section>
  );
}
