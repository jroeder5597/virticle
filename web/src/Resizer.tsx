// A drag handle between two panes. While dragging it writes a CSS variable on the app element directly (no
// React render per mouse move); the final size is saved as a per-user preference. Arrow keys move it in
// 16px steps, double-click or Home puts the default back.
import { useRef } from "react";
import { setPref } from "./prefs";

export interface ResizerProps {
  /** "x": the handle moves sideways and sizes the pane on its left. "y": it moves up and down and sizes the pane below it. */
  axis: "x" | "y";
  /** CSS variable on .app that holds the size. */
  cssVar: string;
  /** Preference key the size is saved under ("" when reset to the default). */
  pref: string;
  min: number;
  /** Largest size for the current window. */
  max: () => number;
  /** The pane's size now, in px. */
  current: () => number;
  label: string;
}

export const clampSize = (v: number, min: number, max: number) => Math.round(Math.max(min, Math.min(Math.max(min, max), v)));

export default function Resizer(p: ResizerProps) {
  const drag = useRef<{ start: number; size: number; last: number } | null>(null);
  const app = () => document.querySelector<HTMLElement>(".app");
  const apply = (v: number) => {
    const size = clampSize(v, p.min, p.max());
    app()?.style.setProperty(p.cssVar, size + "px");
    return size;
  };
  const reset = () => {
    app()?.style.removeProperty(p.cssVar);
    setPref(p.pref, "");
  };
  const pos = (e: React.PointerEvent) => (p.axis === "x" ? e.clientX : e.clientY);
  return (
    <div
      className={"resizer rz-" + p.axis}
      role="separator"
      tabIndex={0}
      aria-orientation={p.axis === "x" ? "vertical" : "horizontal"}
      aria-label={p.label}
      title="Drag to resize. Double-click for the default size."
      onPointerDown={(e) => {
        if (e.button !== 0) return;
        e.preventDefault();
        e.currentTarget.setPointerCapture(e.pointerId);
        const size = p.current();
        drag.current = { start: pos(e), size, last: size };
        document.body.classList.add("resizing-" + p.axis);
      }}
      onPointerMove={(e) => {
        const d = drag.current;
        if (!d) return;
        const delta = pos(e) - d.start;
        d.last = apply(p.axis === "x" ? d.size + delta : d.size - delta);
      }}
      onPointerUp={(e) => {
        const d = drag.current;
        if (!d) return;
        drag.current = null;
        e.currentTarget.releasePointerCapture(e.pointerId);
        document.body.classList.remove("resizing-" + p.axis);
        if (d.last !== d.size) setPref(p.pref, String(d.last));
      }}
      onPointerCancel={() => {
        drag.current = null;
        document.body.classList.remove("resizing-" + p.axis);
      }}
      onDoubleClick={reset}
      onKeyDown={(e) => {
        const grow = p.axis === "x" ? "ArrowRight" : "ArrowUp";
        const shrink = p.axis === "x" ? "ArrowLeft" : "ArrowDown";
        if (e.key === grow || e.key === shrink) {
          e.preventDefault();
          setPref(p.pref, String(apply(p.current() + (e.key === grow ? 16 : -16))));
        } else if (e.key === "Home") {
          e.preventDefault();
          reset();
        }
      }}
    />
  );
}
