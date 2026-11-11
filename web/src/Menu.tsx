// The one menu implementation: a context menu (Tree) and a dropdown button (VM page "Actions"),
// both with submenus. No animation. Opens instantly, stays inside the viewport (submenus flip
// left/up), and at phone width submenus expand inline instead of flying out.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { createPortal } from "react-dom";
import type { LucideIcon } from "lucide-react";
import { ChevronDown, ChevronRight } from "lucide-react";
import "./menu.css";

export type MenuEntry =
  | "sep"
  | {
      label: string;
      icon?: LucideIcon;
      run?: () => void;
      /** A submenu. */
      items?: MenuEntry[];
      danger?: boolean;
      disabled?: boolean;
      /** Tooltip; on a disabled entry it says why. */
      title?: string;
    };

const HOVER_MS = 120;
const phone = () => window.matchMedia("(max-width: 760px)").matches;

function Panel(props: { items: MenuEntry[]; onClose: () => void; onBack?: () => void; inline: boolean; focusFirst: boolean }) {
  const { items, inline } = props;
  const ref = useRef<HTMLDivElement>(null);
  const [sub, setSub] = useState<{ i: number; focus: boolean } | null>(null);
  const timer = useRef<number>();
  const flyout = !!props.onBack && !inline;
  const own = () => [...(ref.current?.querySelectorAll<HTMLButtonElement>(":scope > .mi > button:not(:disabled)") ?? [])];

  useEffect(() => {
    if (props.focusFirst) own()[0]?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.focusFirst]);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  // Keep a flying-out submenu inside the viewport: flip to the left of its parent, shift up.
  useLayoutEffect(() => {
    const el = ref.current;
    if (!flyout || !el) return;
    const pr = el.parentElement!.getBoundingClientRect();
    el.style.left = "100%";
    el.style.right = "auto";
    el.style.top = "-5px";
    let r = el.getBoundingClientRect();
    if (r.right > window.innerWidth - 4) {
      el.style.left = "auto";
      el.style.right = "100%";
      r = el.getBoundingClientRect();
      if (r.left < 4) {
        el.style.right = "auto";
        el.style.left = `${4 - pr.left}px`;
        r = el.getBoundingClientRect();
      }
    }
    let top = -5;
    if (r.bottom > window.innerHeight - 4) top -= r.bottom - (window.innerHeight - 4);
    if (r.top + (top + 5) < 4) top += 4 - (r.top + (top + 5));
    el.style.top = `${top}px`;
  }, [flyout]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    const bs = own();
    const i = bs.indexOf(document.activeElement as HTMLButtonElement);
    let handled = true;
    switch (e.key) {
      case "ArrowDown":
        bs[(i + 1) % bs.length]?.focus();
        break;
      case "ArrowUp":
        bs[(i - 1 + bs.length) % bs.length]?.focus();
        break;
      case "Home":
        bs[0]?.focus();
        break;
      case "End":
        bs[bs.length - 1]?.focus();
        break;
      case "ArrowRight": {
        const idx = Number((document.activeElement as HTMLElement | null)?.dataset.i);
        const it = items[idx];
        if (it && it !== "sep" && it.items && !it.disabled) setSub({ i: idx, focus: true });
        break;
      }
      case "ArrowLeft":
        if (props.onBack) props.onBack();
        else handled = false;
        break;
      case "Escape":
        if (props.onBack) props.onBack();
        else props.onClose();
        break;
      case "Tab":
        props.onClose();
        handled = false;
        break;
      default:
        handled = false;
    }
    if (handled) {
      e.preventDefault();
      e.stopPropagation();
    }
  };

  const intent = (fn: () => void) => {
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(fn, HOVER_MS);
  };

  return (
    <div ref={ref} className={"ctxmenu" + (props.onBack ? " sub" : "") + (inline ? " inline" : "")} role="menu" onKeyDown={onKeyDown}>
      {items.map((it, i) => {
        if (it === "sep") return <div key={i} className="sep" role="separator" />;
        const open = sub?.i === i;
        const Icon = it.icon;
        return (
          <div
            key={it.label}
            className="mi"
            onMouseEnter={() => {
              if (inline) return;
              if (it.items && !it.disabled) intent(() => setSub((s) => (s?.i === i ? s : { i, focus: false })));
              else intent(() => setSub(null));
            }}
            onMouseLeave={() => window.clearTimeout(timer.current)}
          >
            <button
              type="button"
              role="menuitem"
              data-i={i}
              tabIndex={-1}
              disabled={it.disabled}
              title={it.title}
              className={it.danger ? "danger" : ""}
              aria-haspopup={it.items ? "menu" : undefined}
              aria-expanded={it.items ? open : undefined}
              onClick={() => {
                if (it.items) {
                  setSub(open && (inline || sub?.focus) ? null : { i, focus: true });
                  return;
                }
                props.onClose();
                it.run?.();
              }}
            >
              {Icon && <Icon size={14} aria-hidden />}
              <span className="lbl">{it.label}</span>
              {it.items && (inline && open ? <ChevronDown size={14} className="arr" aria-hidden /> : <ChevronRight size={14} className="arr" aria-hidden />)}
            </button>
            {open && it.items && (
              <Panel
                items={it.items}
                inline={inline}
                focusFirst={sub!.focus}
                onClose={props.onClose}
                onBack={() => {
                  setSub(null);
                  ref.current?.querySelector<HTMLElement>(`:scope > .mi > button[data-i="${i}"]`)?.focus();
                }}
              />
            )}
          </div>
        );
      })}
    </div>
  );
}

/** A menu at a screen position. `up` is the anchor's top edge (opens upwards from there when there is no room below). */
export function MenuPopup(props: { x: number; y: number; up?: number; alignRight?: boolean; items: MenuEntry[]; onClose: () => void; ignore?: HTMLElement | null }) {
  const wrap = useRef<HTMLDivElement>(null);
  const [inline] = useState(phone);
  const { onClose } = props;
  useLayoutEffect(() => {
    const m = wrap.current!;
    const w = m.offsetWidth,
      h = m.offsetHeight;
    if (inline) {
      // Phone: the menu spans the screen, so submenus that expand inline cannot widen it past the edge.
      m.style.left = "8px";
      m.style.right = "8px";
      m.style.top = `${Math.max(4, Math.min(props.y, window.innerHeight - h - 4))}px`;
      m.style.visibility = "visible";
      return;
    }
    let left = props.alignRight ? props.x - w : props.x;
    if (left + w > window.innerWidth - 4) left = props.x - w;
    const top = props.y + h > window.innerHeight - 4 ? (props.up ?? props.y) - h : props.y;
    m.style.left = `${Math.max(4, Math.min(left, window.innerWidth - w - 4))}px`;
    m.style.top = `${Math.max(4, Math.min(top, window.innerHeight - h - 4))}px`;
    m.style.visibility = "visible";
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    const away = (e: Event) => !wrap.current?.contains(e.target as Node) && !props.ignore?.contains(e.target as Node) && onClose();
    // A menu anchored to a button closes when the button moves (the page scrolled under it); a late scroll
    // event from before it opened changes nothing. A menu at a point closes on any scroll.
    const top0 = props.ignore?.getBoundingClientRect().top;
    const scroll = (e: Event) => {
      if (wrap.current?.contains(e.target as Node)) return;
      if (props.ignore && top0 !== undefined && Math.abs(props.ignore.getBoundingClientRect().top - top0) < 1) return;
      onClose();
    };
    window.addEventListener("mousedown", away);
    window.addEventListener("scroll", scroll, true);
    window.addEventListener("resize", onClose);
    return () => {
      window.removeEventListener("mousedown", away);
      window.removeEventListener("scroll", scroll, true);
      window.removeEventListener("resize", onClose);
    };
  }, [onClose, props.ignore]);
  return createPortal(
    <div ref={wrap} className={"menu-pos" + (inline ? " inline" : "")} style={{ visibility: "hidden" }}>
      <Panel items={props.items} inline={inline} focusFirst onClose={onClose} />
    </div>,
    document.body,
  );
}

/** A button that opens a menu below it. */
export function DropdownMenu(props: { label: ReactNode; items: MenuEntry[]; className?: string; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState<{ x: number; y: number; up: number }>({ x: 0, y: 0, up: 0 });
  const btn = useRef<HTMLButtonElement>(null);
  const close = () => {
    setOpen(false);
    btn.current?.focus();
  };
  return (
    <>
      <button
        ref={btn}
        type="button"
        className={props.className}
        disabled={props.disabled}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => {
          if (open) return close();
          const b = btn.current!.getBoundingClientRect();
          setAt({ x: b.right, y: b.bottom + 2, up: b.top - 2 });
          setOpen(true);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" && !open) {
            e.preventDefault();
            btn.current?.click();
          }
        }}
      >
        {props.label}
        <ChevronDown size={14} aria-hidden />
      </button>
      {open && <MenuPopup {...at} alignRight items={props.items} onClose={close} ignore={btn.current} />}
    </>
  );
}
