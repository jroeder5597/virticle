// A themed pool picker: a native <select> cannot draw a capacity bar. Shows, for the selected pool and for
// every option, the name, percent used, "X used of Y" and a thin bar coloured by level (ok, warning, critical).
// Colour is never the only signal: the percentage is always written out.
import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { ChevronDown } from "lucide-react";
import { bytes, get, Pool } from "./api";
import { poolLevel } from "./poolLevel";
import "./poolselect.css";

type Limits = { warn: number; crit: number };
let limits: Limits | undefined;
let limitsAsked: Promise<void> | undefined;

/** The warning and critical limits the controller raises pool alerts at (fetched once per page load). */
function usePoolLimits(): Limits {
  const [v, setV] = useState<Limits>(limits ?? { warn: 85, crit: 95 });
  useEffect(() => {
    if (limits) return;
    limitsAsked ??= get<{ poolWarn?: number; poolCritical?: number }>("/alerts").then(
      (r) => {
        limits = { warn: r.poolWarn || 85, crit: r.poolCritical || 95 };
      },
      () => {
        limitsAsked = undefined;
      },
    );
    let live = true;
    limitsAsked.then(() => live && limits && setV(limits));
    return () => {
      live = false;
    };
  }, []);
  return v;
}

function PoolFace({ p, need, lim }: { p: Pool; need?: number; lim: Limits }) {
  const u = poolLevel(p.allocation, p.capacity, lim.warn, lim.crit);
  const inactive = p.state !== "running";
  const short = !inactive && need !== undefined && p.capacity > 0 && p.available < need;
  return (
    <span className="ps-face">
      <span className="ps-row">
        <span className="ps-name">{p.name}</span>
        <span className="num ps-pct">{inactive ? "inactive" : `${u.pct.toFixed(0)}% used`}</span>
      </span>
      <span className="ps-row ps-sub">
        <span className="num ps-use">
          {bytes(p.allocation)} used of {bytes(p.capacity)}
        </span>
        {short && <span className="ps-warn">not enough free space</span>}
      </span>
      <span className="ps-bar" aria-hidden="true">
        <span className={"ps-fill " + u.level} style={{ width: u.pct + "%" }} />
      </span>
    </span>
  );
}

/**
 * `pools` is the list the dialog already loaded. `needBytes` marks pools with less free space than the
 * requested size; they stay selectable (the server decides).
 */
export default function PoolSelect(props: { pools: Pool[]; value: string; onChange: (name: string) => void; label: string; needBytes?: number; disabled?: boolean }) {
  const { pools, value, onChange } = props;
  const lim = usePoolLimits();
  const id = useId();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const btn = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const typed = useRef({ s: "", t: 0 });
  const [pos, setPos] = useState<{ left: number; top?: number; bottom?: number; width: number; maxH: number }>();
  const sel = pools.find((p) => p.name === value);
  const enabled = (p: Pool) => p.state === "running";

  const openList = () => {
    setActive(Math.max(0, pools.findIndex((p) => p.name === value)));
    setPos(undefined);
    setOpen(true);
  };
  const choose = (p: Pool) => {
    if (!enabled(p)) return;
    onChange(p.name);
    setOpen(false);
    btn.current?.focus();
  };
  // Place the popup under the button, or above it when there is more room there; always inside the viewport.
  useLayoutEffect(() => {
    if (!open || !btn.current) return;
    const r = btn.current.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const width = Math.min(Math.max(r.width, 280), vw - 16);
    const left = Math.max(8, Math.min(r.left, vw - width - 8));
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    setPos(below >= 200 || below >= above ? { left, top: r.bottom + 2, width, maxH: below } : { left, bottom: vh - r.top + 2, width, maxH: above });
  }, [open]);
  useEffect(() => {
    if (!open) return;
    list.current?.focus();
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!list.current?.contains(t) && !btn.current?.contains(t)) setOpen(false);
    };
    // Escape closes only the list, not the dialog around it.
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
        btn.current?.focus();
      }
    };
    const close = () => setOpen(false);
    document.addEventListener("mousedown", onDown, true);
    window.addEventListener("keydown", onEsc, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("mousedown", onDown, true);
      window.removeEventListener("keydown", onEsc, true);
      window.removeEventListener("resize", close);
    };
  }, [open]);
  useEffect(() => {
    list.current?.querySelector<HTMLElement>(`[data-i="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active, open, pos]);

  /** Next enabled pool in direction `dir` from the active one; stays put at the ends. */
  const step = (dir: 1 | -1) => {
    for (let i = active + dir; i >= 0 && i < pools.length; i += dir) if (enabled(pools[i])) return setActive(i);
  };
  const onKey = (e: React.KeyboardEvent) => {
    if (!open) {
      if (["ArrowDown", "ArrowUp", "Enter", " "].includes(e.key)) {
        e.preventDefault();
        openList();
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        step(1);
        break;
      case "ArrowUp":
        e.preventDefault();
        step(-1);
        break;
      case "Home": {
        e.preventDefault();
        const i = pools.findIndex(enabled);
        if (i >= 0) setActive(i);
        break;
      }
      case "End": {
        e.preventDefault();
        for (let i = pools.length - 1; i >= 0; i--)
          if (enabled(pools[i])) {
            setActive(i);
            break;
          }
        break;
      }
      case "Enter":
      case " ":
        e.preventDefault();
        if (pools[active]) choose(pools[active]);
        break;
      case "Tab":
        setOpen(false);
        break;
      default:
        if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
          // Type-ahead: letters typed in quick succession extend the prefix.
          const now = Date.now();
          const t = typed.current;
          t.s = now - t.t > 700 ? e.key.toLowerCase() : t.s + e.key.toLowerCase();
          t.t = now;
          const i = pools.findIndex((p) => enabled(p) && p.name.toLowerCase().startsWith(t.s));
          if (i >= 0) setActive(i);
        }
    }
  };

  return (
    <div className="ps">
      <button
        ref={btn}
        type="button"
        className="ps-btn"
        role="combobox"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={id}
        aria-label={props.label}
        disabled={props.disabled}
        onClick={() => (open ? setOpen(false) : openList())}
        onKeyDown={onKey}
      >
        {sel ? <PoolFace p={sel} need={props.needBytes} lim={lim} /> : <span className="ps-face muted">No pool</span>}
        <ChevronDown size={16} aria-hidden="true" />
      </button>
      {open && (
        <div
          ref={list}
          id={id}
          className="ps-list"
          role="listbox"
          tabIndex={-1}
          aria-label={props.label + " list"}
          aria-activedescendant={`${id}-${active}`}
          style={pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, width: pos.width, maxHeight: pos.maxH } : { visibility: "hidden" }}
          onKeyDown={onKey}
        >
          {pools.map((p, i) => (
            <div
              key={p.name}
              id={`${id}-${i}`}
              data-i={i}
              role="option"
              aria-selected={p.name === value}
              aria-disabled={!enabled(p)}
              className={"ps-opt" + (i === active ? " active" : "") + (p.name === value ? " sel" : "") + (!enabled(p) ? " off" : "")}
              onMouseMove={() => enabled(p) && i !== active && setActive(i)}
              onClick={() => choose(p)}
            >
              <PoolFace p={p} need={props.needBytes} lim={lim} />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
