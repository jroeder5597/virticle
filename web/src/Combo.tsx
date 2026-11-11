// A themed combobox: a button showing the chosen item and a popup list (role="listbox"), optionally with a search
// box that filters the list. Arrow keys, Home/End, Enter, Escape (closes only the list), type-ahead, no animation.
// The popup is fixed-positioned inside the viewport, so it works inside a dialog and at phone width.
import { ReactNode, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ChevronDown } from "lucide-react";
import "./combo.css";

export interface ComboItem {
  key: string;
  disabled?: boolean;
  /** Items with the same group are listed together under a heading, groups in order of first appearance. */
  group?: string;
  /** Text the search box matches besides the key. */
  search?: string;
}

export interface ComboProps<T extends ComboItem> {
  items: T[];
  value: string;
  onChange: (key: string) => void;
  /** Accessible name of the button and the list. */
  label: string;
  /** What the closed button shows for the chosen item (undefined: nothing chosen). */
  face: (item: T | undefined) => ReactNode;
  option: (item: T) => ReactNode;
  disabled?: boolean;
  /** With this, the popup starts with a search box. */
  searchLabel?: string;
  /** Shown instead of the list when there are no items at all. */
  empty?: ReactNode;
  /** Text under a group's heading. */
  groupHint?: Record<string, string>;
  /** Extra content at the end of the popup (a link, a hint). */
  footer?: ReactNode;
  className?: string;
}

type Pos = { left: number; top?: number; bottom?: number; width: number; maxH: number };

export default function Combo<T extends ComboItem>(p: ComboProps<T>) {
  const { items, value, onChange } = p;
  const id = useId();
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [active, setActive] = useState("");
  const [pos, setPos] = useState<Pos>();
  const btn = useRef<HTMLButtonElement>(null);
  const pop = useRef<HTMLDivElement>(null);
  const search = useRef<HTMLInputElement>(null);
  const typed = useRef({ s: "", t: 0 });

  const ql = q.trim().toLowerCase();
  const shown = items.filter((i) => !ql || i.key.toLowerCase().includes(ql) || (i.search ?? "").toLowerCase().includes(ql));
  // Group order: first appearance; ungrouped items first.
  const groups: string[] = [];
  for (const i of shown) if (i.group && !groups.includes(i.group)) groups.push(i.group);
  const ordered = [...shown.filter((i) => !i.group), ...groups.flatMap((g) => shown.filter((i) => i.group === g))];
  const selectable = ordered.filter((i) => !i.disabled);
  const sel = items.find((i) => i.key === value);

  const openList = () => {
    setQ("");
    setActive(sel && !sel.disabled ? sel.key : "");
    setPos(undefined);
    setOpen(true);
  };
  const choose = (i: T) => {
    if (i.disabled) return;
    onChange(i.key);
    setOpen(false);
    btn.current?.focus();
  };

  useLayoutEffect(() => {
    if (!open || !btn.current) return;
    const r = btn.current.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const width = Math.min(Math.max(r.width, 300), vw - 16);
    const left = Math.max(8, Math.min(r.left, vw - width - 8));
    const below = vh - r.bottom - 8;
    const above = r.top - 8;
    setPos(below >= 220 || below >= above ? { left, top: r.bottom + 2, width, maxH: below } : { left, bottom: vh - r.top + 2, width, maxH: above });
  }, [open]);

  useEffect(() => {
    if (!open) return;
    (search.current ?? pop.current)?.focus();
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!pop.current?.contains(t) && !btn.current?.contains(t)) setOpen(false);
    };
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

  // Keep the active item valid while the filter changes, and in view.
  useEffect(() => {
    if (!open) return;
    if (!selectable.some((i) => i.key === active)) setActive(selectable[0]?.key ?? "");
  });
  useEffect(() => {
    pop.current?.querySelector<HTMLElement>(`[data-k="${CSS.escape(active)}"]`)?.scrollIntoView({ block: "nearest" });
  }, [active, pos]);

  const step = (dir: 1 | -1) => {
    const i = selectable.findIndex((x) => x.key === active);
    const n = selectable[Math.max(0, Math.min(selectable.length - 1, i + dir))];
    if (n) setActive(n.key);
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
        return step(1);
      case "ArrowUp":
        e.preventDefault();
        return step(-1);
      case "Home":
        if (p.searchLabel) return; // the caret moves in the search box
        e.preventDefault();
        return setActive(selectable[0]?.key ?? "");
      case "End":
        if (p.searchLabel) return;
        e.preventDefault();
        return setActive(selectable[selectable.length - 1]?.key ?? "");
      case "Enter":
        e.preventDefault();
        e.stopPropagation(); // the dialog around must not take this Enter as "submit"
        {
          const it = selectable.find((x) => x.key === active);
          if (it) choose(it);
        }
        return;
      case " ":
        if (p.searchLabel) return;
        e.preventDefault();
        {
          const it = selectable.find((x) => x.key === active);
          if (it) choose(it);
        }
        return;
      case "Tab":
        return setOpen(false);
      default:
        if (!p.searchLabel && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
          const now = Date.now();
          const t = typed.current;
          t.s = now - t.t > 700 ? e.key.toLowerCase() : t.s + e.key.toLowerCase();
          t.t = now;
          const it = selectable.find((x) => x.key.toLowerCase().startsWith(t.s));
          if (it) setActive(it.key);
        }
    }
  };

  const row = (i: T) => (
    <div
      key={i.key}
      id={`${id}-${i.key}`}
      data-k={i.key}
      role="option"
      aria-selected={i.key === value}
      aria-disabled={!!i.disabled}
      className={"cb-opt" + (i.key === active ? " active" : "") + (i.key === value ? " sel" : "") + (i.disabled ? " off" : "")}
      onMouseMove={() => !i.disabled && i.key !== active && setActive(i.key)}
      onClick={() => choose(i)}
    >
      {p.option(i)}
    </div>
  );
  const plain = ordered.filter((i) => !i.group);

  return (
    <div className={"cb" + (p.className ? " " + p.className : "")}>
      <button
        ref={btn}
        type="button"
        className="cb-btn"
        role="combobox"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={id}
        aria-label={p.label}
        disabled={p.disabled}
        onClick={() => (open ? setOpen(false) : openList())}
        onKeyDown={onKey}
      >
        {p.face(sel)}
        <ChevronDown size={16} aria-hidden="true" />
      </button>
      {open &&
        createPortal(
        <div
          ref={pop}
          className="cb-pop"
          tabIndex={-1}
          style={pos ? { left: pos.left, top: pos.top, bottom: pos.bottom, width: pos.width, maxHeight: pos.maxH } : { visibility: "hidden" }}
          onKeyDown={onKey}
        >
          {p.searchLabel && items.length > 0 && (
            <input ref={search} className="cb-search" type="text" placeholder={p.searchLabel} role="searchbox" aria-label={p.searchLabel} aria-controls={id} aria-activedescendant={active ? `${id}-${active}` : undefined} value={q} onChange={(e) => setQ(e.target.value)} />
          )}
          {items.length === 0 ? (
            <div className="cb-empty">{p.empty}</div>
          ) : (
            <div id={id} role="listbox" aria-label={p.label + " list"} className="cb-list" aria-activedescendant={active ? `${id}-${active}` : undefined}>
              {plain.map(row)}
              {groups.map((g) => (
                <div key={g} role="group" aria-label={g}>
                  <div className="cb-gh" role="presentation">
                    {g}
                    {p.groupHint?.[g] && <span className="cb-gt">{p.groupHint[g]}</span>}
                  </div>
                  {ordered.filter((i) => i.group === g).map(row)}
                </div>
              ))}
              {shown.length === 0 && <div className="cb-empty">Nothing matches {q}.</div>}
            </div>
          )}
          {p.footer && <div className="cb-foot">{p.footer}</div>}
        </div>,
        document.body,
        )}
    </div>
  );
}
