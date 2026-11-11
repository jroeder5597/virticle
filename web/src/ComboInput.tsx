// A text input with a themed suggestion list beneath it (the themed replacement for a datalist).
// Free text stays allowed. Typing filters; ArrowDown/ArrowUp move; Enter takes the highlighted suggestion
// (with none highlighted Enter submits the form as usual); Escape closes only the list.
import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import "./combo.css";

export interface Suggestion {
  value: string;
  /** Shown beside the value (the VM name for a MAC address). */
  hint?: string;
}

type Pos = { left: number; top?: number; bottom?: number; width: number; maxH: number };

export default function ComboInput(p: {
  value: string;
  onChange: (v: string) => void;
  suggestions: Suggestion[];
  placeholder?: string;
  required?: boolean;
  readOnly?: boolean;
  mono?: boolean;
  ariaLabel?: string;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const [pos, setPos] = useState<Pos>();
  const inp = useRef<HTMLInputElement>(null);
  const pop = useRef<HTMLDivElement>(null);

  const q = p.value.trim().toLowerCase();
  const shown = p.suggestions.filter((s) => !q || s.value.toLowerCase().includes(q) || (s.hint ?? "").toLowerCase().includes(q));
  const visible = open && !p.readOnly && shown.length > 0;

  useLayoutEffect(() => {
    if (!visible || !inp.current) return;
    const r = inp.current.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const width = Math.min(Math.max(r.width, 240), vw - 16);
    const left = Math.max(8, Math.min(r.left, vw - width - 8));
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    setPos(below >= 160 || below >= above ? { left, top: r.bottom + 2, width, maxH: below } : { left, bottom: vh - r.top + 2, width, maxH: above });
  }, [visible]);

  useEffect(() => {
    if (!visible) return;
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        setOpen(false);
      }
    };
    const close = (e: Event) => {
      if (!pop.current?.contains(e.target as Node)) setOpen(false);
    };
    window.addEventListener("keydown", onEsc, true);
    window.addEventListener("resize", close);
    window.addEventListener("scroll", close, true);
    return () => {
      window.removeEventListener("keydown", onEsc, true);
      window.removeEventListener("resize", close);
      window.removeEventListener("scroll", close, true);
    };
  }, [visible]);

  useEffect(() => {
    pop.current?.querySelector<HTMLElement>(`[data-i="${active}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active, pos]);

  const choose = (s: Suggestion) => {
    p.onChange(s.value);
    setOpen(false);
    setActive(-1);
  };
  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (!shown.length) return;
      e.preventDefault();
      if (!visible) {
        setOpen(true);
        return;
      }
      const d = e.key === "ArrowDown" ? 1 : -1;
      setActive((a) => Math.max(0, Math.min(shown.length - 1, a < 0 ? (d > 0 ? 0 : shown.length - 1) : a + d)));
    } else if (e.key === "Enter" && visible && active >= 0 && shown[active]) {
      e.preventDefault();
      e.stopPropagation();
      choose(shown[active]);
    } else if (e.key === "Tab") setOpen(false);
  };

  return (
    <div className="ci">
      <input
        ref={inp}
        className={p.mono ? "mono" : undefined}
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={visible}
        aria-controls={id}
        aria-activedescendant={visible && active >= 0 ? `${id}-${active}` : undefined}
        aria-label={p.ariaLabel}
        autoComplete="off"
        value={p.value}
        placeholder={p.placeholder}
        required={p.required}
        readOnly={p.readOnly}
        onChange={(e) => {
          p.onChange(e.target.value);
          setActive(-1);
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        onClick={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={onKey}
      />
      {visible &&
        createPortal(
          <div
            ref={pop}
            id={id}
            role="listbox"
            className="cb-pop"
            style={pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, width: pos.width, maxHeight: pos.maxH, overflow: "auto" } : { visibility: "hidden" }}
            onMouseDown={(e) => e.preventDefault()}
          >
            {shown.map((s, i) => (
              <div key={s.value} id={`${id}-${i}`} data-i={i} role="option" aria-selected={i === active} className={"cb-opt" + (i === active ? " active" : "")} onMouseMove={() => i !== active && setActive(i)} onClick={() => choose(s)}>
                <span className="sel-lab mono">{s.value}</span>
                {s.hint && <span className="cb-hint">{s.hint}</span>}
              </div>
            ))}
          </div>,
          document.body,
        )}
    </div>
  );
}
