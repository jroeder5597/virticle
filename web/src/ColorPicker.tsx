// A small built-in colour picker (not <input type="color">, which opens a browser widget): a hue ring with a
// saturation/brightness square inside it, ten presets and a hex field. Pointer and keyboard operable.
// Dragging only changes the local preview; ONE request is made when the pointer is released, on Enter or
// on Apply. No animation. Stays inside the viewport.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { HEX_RE, PRESETS, hexToHsv, hsvToHex } from "./tagColor";
import { TagChip } from "./TagChip";
import "./tags.css";

const SIZE = 176; // wheel diameter
const RING = 20; // ring thickness
const SQ = 92; // inner square side (its diagonal fits inside the ring's hole)
const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v));

export default function ColorPicker(props: {
  name: string;
  current?: string;
  /** The chip it belongs to: the popover opens under it and clicks on it do not count as "outside". */
  anchor: HTMLElement;
  onApply: (hex: string) => Promise<void>;
  onRemove: () => Promise<void>;
  onClose: () => void;
}) {
  const { onClose } = props;
  const [hsv, setHsv] = useState<[number, number, number]>(() => hexToHsv(props.current ?? "#0091ff"));
  const [text, setText] = useState(props.current ?? "#0091ff");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  const hexIn = useRef<HTMLInputElement>(null);
  const hsvRef = useRef(hsv);
  hsvRef.current = hsv;
  const candidate = hsvToHex(...hsv);

  const set = (h: number, s: number, v: number) => {
    const n: [number, number, number] = [((h % 360) + 360) % 360, clamp(s, 0, 1), clamp(v, 0, 1)];
    hsvRef.current = n;
    setHsv(n);
    setText(hsvToHex(...n));
  };

  const run = async (fn: () => Promise<void>, close: boolean) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      if (close) onClose();
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };
  const apply = (hex: string, close = true) => run(() => props.onApply(hex.toLowerCase()), close);
  const applyHex = () => {
    const t = text.trim().startsWith("#") ? text.trim() : "#" + text.trim();
    if (!HEX_RE.test(t)) return setError("Enter a colour as six hex digits, for example #1a2b3c.");
    apply(t);
  };

  // Place under the chip, flipped above when there is no room, always inside the window.
  useLayoutEffect(() => {
    const el = box.current!;
    const a = props.anchor.getBoundingClientRect();
    const w = el.offsetWidth,
      h = el.offsetHeight;
    const top = a.bottom + 6 + h > window.innerHeight - 4 ? a.top - 6 - h : a.bottom + 6;
    el.style.left = `${clamp(a.left, 4, window.innerWidth - w - 4)}px`;
    el.style.top = `${clamp(top, 4, window.innerHeight - h - 4)}px`;
    el.style.visibility = "visible";
    hexIn.current?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  useEffect(() => {
    const away = (e: Event) => {
      const t = e.target as Node;
      if (!box.current?.contains(t) && !props.anchor.contains(t)) onClose();
    };
    const scroll = (e: Event) => !box.current?.contains(e.target as Node) && onClose();
    window.addEventListener("mousedown", away);
    window.addEventListener("scroll", scroll, true);
    window.addEventListener("resize", onClose);
    return () => {
      window.removeEventListener("mousedown", away);
      window.removeEventListener("scroll", scroll, true);
      window.removeEventListener("resize", onClose);
    };
  }, [onClose, props.anchor]);

  // ---- hue ring ----
  const hueFrom = (e: React.PointerEvent, el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    const dx = e.clientX - (r.left + r.width / 2),
      dy = e.clientY - (r.top + r.height / 2);
    return { deg: ((Math.atan2(dx, -dy) * 180) / Math.PI + 360) % 360, dist: Math.hypot(dx, dy) };
  };
  const ringDown = (e: React.PointerEvent<HTMLDivElement>) => {
    const p = hueFrom(e, e.currentTarget);
    if (p.dist < SIZE / 2 - RING - 2 || p.dist > SIZE / 2 + 2) return;
    e.currentTarget.setPointerCapture(e.pointerId);
    set(p.deg, hsvRef.current[1], hsvRef.current[2]);
  };
  const ringMove = (e: React.PointerEvent<HTMLDivElement>) => {
    if (e.currentTarget.hasPointerCapture(e.pointerId)) set(hueFrom(e, e.currentTarget).deg, hsvRef.current[1], hsvRef.current[2]);
  };
  const release = (e: React.PointerEvent<HTMLDivElement>) => {
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId);
      apply(hsvToHex(...hsvRef.current), false);
    }
  };
  const ringKey = (e: React.KeyboardEvent) => {
    const [h, s, v] = hsvRef.current;
    const step = e.shiftKey ? 10 : 1;
    if (e.key === "ArrowRight" || e.key === "ArrowUp") set(h + step, s, v);
    else if (e.key === "ArrowLeft" || e.key === "ArrowDown") set(h - step, s, v);
    else if (e.key === "Home") set(0, s, v);
    else if (e.key === "End") set(359, s, v);
    else if (e.key === "Enter") apply(hsvToHex(h, s, v));
    else return;
    e.preventDefault();
  };

  // ---- saturation / brightness square ----
  const svFrom = (e: React.PointerEvent, el: HTMLElement) => {
    const r = el.getBoundingClientRect();
    set(hsvRef.current[0], (e.clientX - r.left) / r.width, 1 - (e.clientY - r.top) / r.height);
  };
  const svDown = (e: React.PointerEvent<HTMLDivElement>) => {
    e.currentTarget.setPointerCapture(e.pointerId);
    svFrom(e, e.currentTarget);
  };
  const svMove = (e: React.PointerEvent<HTMLDivElement>) => e.currentTarget.hasPointerCapture(e.pointerId) && svFrom(e, e.currentTarget);
  const svKey = (e: React.KeyboardEvent) => {
    const [h, s, v] = hsvRef.current;
    const step = e.shiftKey ? 0.1 : 0.02;
    if (e.key === "ArrowRight") set(h, s + step, v);
    else if (e.key === "ArrowLeft") set(h, s - step, v);
    else if (e.key === "ArrowUp") set(h, s, v + step);
    else if (e.key === "ArrowDown") set(h, s, v - step);
    else if (e.key === "Enter") apply(hsvToHex(h, s, v));
    else return;
    e.preventDefault();
  };

  const [h, s, v] = hsv;
  const mid = SIZE / 2 - RING / 2;
  const ang = (h * Math.PI) / 180;
  const off = (SIZE - SQ) / 2;
  return createPortal(
    <div
      ref={box}
      className="cpick"
      role="group"
      aria-label={`Colour for tag ${props.name}`}
      style={{ visibility: "hidden" }}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}
    >
      <div className="cp-head">
        <TagChip name={props.name} color={candidate} />
      </div>
      <div className="cw" style={{ width: SIZE, height: SIZE }}>
        <div
          className="cw-ring"
          role="slider"
          tabIndex={0}
          aria-label="Hue"
          aria-valuemin={0}
          aria-valuemax={359}
          aria-valuenow={Math.round(h)}
          onPointerDown={ringDown}
          onPointerMove={ringMove}
          onPointerUp={release}
          onPointerCancel={release}
          onKeyDown={ringKey}
        />
        <span className="cw-thumb" style={{ left: SIZE / 2 + mid * Math.sin(ang), top: SIZE / 2 - mid * Math.cos(ang), background: hsvToHex(h, 1, 1) }} />
        <div
          className="cw-sv"
          role="slider"
          tabIndex={0}
          aria-label="Saturation and brightness"
          aria-valuetext={`saturation ${Math.round(s * 100)} percent, brightness ${Math.round(v * 100)} percent`}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(v * 100)}
          style={{ left: off, top: off, width: SQ, height: SQ, background: `linear-gradient(to top, #000, transparent), linear-gradient(to right, #fff, ${hsvToHex(h, 1, 1)})` }}
          onPointerDown={svDown}
          onPointerMove={svMove}
          onPointerUp={release}
          onPointerCancel={release}
          onKeyDown={svKey}
        />
        <span className="cw-thumb" style={{ left: off + s * SQ, top: off + (1 - v) * SQ, background: candidate }} />
      </div>
      <div className="cp-presets" role="group" aria-label="Preset colours">
        {PRESETS.map((c) => (
          <button key={c} type="button" className="cp-sw" aria-label={`Colour ${c}`} title={c} aria-pressed={props.current?.toLowerCase() === c} style={{ background: c }} disabled={busy} onClick={() => apply(c)} />
        ))}
      </div>
      <div className="cp-hex">
        <input
          ref={hexIn}
          aria-label="Hex colour"
          value={text}
          maxLength={7}
          spellCheck={false}
          className="mono"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              applyHex();
            }
          }}
          onBlur={() => HEX_RE.test(text.startsWith("#") ? text : "#" + text) && set(...hexToHsv(text.startsWith("#") ? text : "#" + text))}
        />
        <button type="button" className="primary" disabled={busy} onClick={applyHex}>
          Apply
        </button>
      </div>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <div className="cp-foot">
        <button type="button" disabled={busy || !props.current} onClick={() => run(props.onRemove, true)}>
          Remove colour
        </button>
        <button type="button" onClick={onClose}>
          Close
        </button>
      </div>
    </div>,
    document.body,
  );
}
