// The toolbar shared by the console tab, the standalone console page and the
// inline console in the overview card.
import { ReactNode, RefObject, useEffect, useRef, useState } from "react";
import { Camera, ClipboardPaste, Copy, Eraser, Keyboard, Maximize2, Minimize2, Scaling } from "lucide-react";
import type RFB from "@novnc/novnc/lib/rfb";
import { DropdownMenu } from "../Menu";
import { Modal } from "../ui";
import { CODE, strokesFor, XK } from "./pasteKeys";
import "./console.css";
import { StatusLine as ResultLine } from "../Checks";

export type Status = "connecting" | "connected" | "closed";

export function StatusLine({ status, detail, onReconnect }: { status: Status; detail?: string; onReconnect: () => void }) {
  return (
    <span className="muted status">
      {status === "connecting" && "Connecting"}
      {status === "connected" && "Connected"}
      {status === "closed" && (
        <>
          Disconnected{detail ? `: ${detail}` : ""}{" "}
          <button className="link" onClick={onReconnect}>
            Reconnect
          </button>
        </>
      )}
    </span>
  );
}

/** Fullscreen on the console element: instant, no animation. */
function FullscreenButton({ box }: { box: RefObject<HTMLElement | null> }) {
  const [full, setFull] = useState(false);
  useEffect(() => {
    const on = () => setFull(document.fullscreenElement === box.current);
    document.addEventListener("fullscreenchange", on);
    return () => document.removeEventListener("fullscreenchange", on);
  }, [box]);
  const toggle = () => (document.fullscreenElement ? document.exitFullscreen() : box.current?.requestFullscreen())?.catch(() => {});
  return (
    <button onClick={toggle} title={full ? "Leave full screen" : "Full screen"} aria-label={full ? "Leave full screen" : "Full screen"}>
      {full ? <Minimize2 size={15} aria-hidden /> : <Maximize2 size={15} aria-hidden />}
      <span className="lbl">{full ? "Exit full screen" : "Full screen"}</span>
    </button>
  );
}

type Combo = { label: string; mods: [number, string][]; key: [number, string] };
const ctrlAlt: [number, string][] = [[XK.Control_L, CODE.Control_L], [XK.Alt_L, CODE.Alt_L]];
const CAD: Combo = { label: "Ctrl+Alt+Del", mods: ctrlAlt, key: [0xffff, "Delete"] }; // what noVNC sendCtrlAltDel sends
const COMBOS: Combo[] = [
  ...[1, 2, 3, 4, 5, 6].map((n) => ({ label: `Ctrl+Alt+F${n}`, mods: ctrlAlt, key: [XK.F(n), `F${n}`] as [number, string] })),
  { label: "Alt+Tab", mods: [[XK.Alt_L, CODE.Alt_L]], key: [XK.Tab, CODE.Tab] },
  { label: "Super (Windows key)", mods: [], key: [XK.Super_L, CODE.Super_L] },
  { label: "Print Screen", mods: [], key: [XK.Print, CODE.Print] },
];

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const KEY_DELAY_MS = 25; // per character; slow enough for a guest with a busy console

/** Paste dialog, then types the text into the guest key by key. */
function PasteDialog({ onClose, onType }: { onClose: () => void; onType: (text: string) => void }) {
  const [text, setText] = useState("");
  const { strokes, skipped } = strokesFor(text);
  return (
    <Modal title="Paste text into the guest" onClose={onClose}>
      <p className="muted">
        The text is typed into the guest as key presses on a US keyboard layout, about {Math.round(1000 / KEY_DELAY_MS)} characters per second. Keys the guest maps
        differently (other layouts) come out wrong, and characters with no US key are skipped. Click into the right field in the guest first.
      </p>
      <textarea className="paste-text" value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} aria-label="Text to type" autoFocus />
      {skipped > 0 && <ResultLine status="warning">{skipped} character{skipped > 1 ? "s" : ""} cannot be typed and will be skipped.</ResultLine>}
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="primary"
          disabled={strokes.length === 0}
          onClick={() => {
            onType(text);
            onClose();
          }}
        >
          Type {strokes.length} character{strokes.length === 1 ? "" : "s"}
        </button>
      </footer>
    </Modal>
  );
}

export interface VncBarProps {
  status: Status;
  onReconnect: () => void;
  rfb: () => RFB | null;
  box: RefObject<HTMLElement | null>;
  node: number;
  uuid: string;
  name?: string;
  fit: boolean;
  onFit: (fit: boolean) => void;
  children?: ReactNode; // extra buttons, e.g. collapse
}

export function VncBar(p: VncBarProps) {
  const [paste, setPaste] = useState(false);
  const [typing, setTyping] = useState<{ done: number; total: number } | null>(null);
  const stop = useRef(false);
  const [error, setError] = useState("");
  const live = p.status === "connected";
  useEffect(() => () => void (stop.current = true), []);

  // Events are spaced out: sent back to back, a guest that is busy booting can miss the combination.
  const press = async (c: Combo) => {
    const r = p.rfb();
    if (!r) return;
    const steps: [number, string, boolean][] = [
      ...c.mods.map(([k, code]) => [k, code, true] as [number, string, boolean]),
      [c.key[0], c.key[1], true],
      [c.key[0], c.key[1], false],
      ...[...c.mods].reverse().map(([k, code]) => [k, code, false] as [number, string, boolean]),
    ];
    for (const [k, code, down] of steps) {
      r.sendKey(k, code, down);
      await sleep(40);
    }
    r.focus();
  };

  const type = async (text: string) => {
    const r = p.rfb();
    if (!r) return;
    const { strokes } = strokesFor(text);
    stop.current = false;
    r.focus();
    for (let i = 0; i < strokes.length && !stop.current; i++) {
      const s = strokes[i];
      setTyping({ done: i, total: strokes.length });
      if (s.shift) r.sendKey(XK.Shift_L, CODE.Shift_L, true);
      r.sendKey(s.keysym, s.code, true);
      await sleep(KEY_DELAY_MS / 2);
      r.sendKey(s.keysym, s.code, false);
      if (s.shift) r.sendKey(XK.Shift_L, CODE.Shift_L, false);
      await sleep(KEY_DELAY_MS / 2);
    }
    setTyping(null);
  };

  const shot = async () => {
    setError("");
    try {
      const res = await fetch(`/api/nodes/${p.node}/vms/${p.uuid}/screenshot`);
      if (!res.ok) throw new Error((await res.json().catch(() => null))?.error ?? `${res.status} ${res.statusText}`);
      const url = URL.createObjectURL(await res.blob());
      const a = document.createElement("a");
      const t = new Date();
      const pad = (n: number) => String(n).padStart(2, "0");
      a.href = url;
      a.download = `${p.name ?? p.uuid.slice(0, 8)}-${t.getFullYear()}${pad(t.getMonth() + 1)}${pad(t.getDate())}-${pad(t.getHours())}${pad(t.getMinutes())}${pad(t.getSeconds())}.png`;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 10000);
    } catch (e) {
      setError(`Screenshot failed: ${(e as Error).message}`);
    }
  };

  return (
    <div className="toolbar">
      {typing ? (
        <span className="muted status">
          Typing {typing.done} / {typing.total}{" "}
          <button className="link" onClick={() => (stop.current = true)}>
            Stop
          </button>
        </span>
      ) : (
        <StatusLine status={p.status} onReconnect={p.onReconnect} />
      )}
      <div className="spacer" />
      <button disabled={!live} onClick={() => press(CAD)} title="Send Ctrl+Alt+Del">
        Ctrl+Alt+Del
      </button>
      <DropdownMenu
        chevron={false}
        ariaLabel="Keys"
        title="Keys"
        label={
          <>
            <Keyboard size={15} aria-hidden />
            <span className="lbl">Keys</span>
          </>
        }
        items={COMBOS.map((c) => ({ label: c.label, disabled: !live, run: () => press(c) }))}
      />
      <button disabled={!live || !!typing} onClick={() => setPaste(true)} title="Paste text" aria-label="Paste text">
        <ClipboardPaste size={15} aria-hidden />
        <span className="lbl">Paste text</span>
      </button>
      <button disabled={!live} onClick={shot} title="Download a screenshot" aria-label="Screenshot">
        <Camera size={15} aria-hidden />
        <span className="lbl">Screenshot</span>
      </button>
      <button
        onClick={() => p.onFit(!p.fit)}
        aria-pressed={p.fit}
        title={p.fit ? "Scaled to fit; click for actual size (1:1)" : "Actual size (1:1); click to scale to fit"}
        aria-label={p.fit ? "Scale to fit is on" : "Actual size is on"}
      >
        <Scaling size={15} aria-hidden />
        <span className="lbl">{p.fit ? "Fit" : "1:1"}</span>
      </button>
      <FullscreenButton box={p.box} />
      {p.children}
      {error && <div className="toolbar-error"><ResultLine status="error">{error}</ResultLine></div>}
      {paste && <PasteDialog onClose={() => setPaste(false)} onType={type} />}
    </div>
  );
}

export function SerialBar(p: {
  status: Status;
  detail?: string;
  onReconnect: () => void;
  box: RefObject<HTMLElement | null>;
  copyAll: () => Promise<void>;
  clear: () => void;
  children?: ReactNode;
}) {
  const [copied, setCopied] = useState("");
  return (
    <div className="toolbar">
      <StatusLine status={p.status} detail={p.detail} onReconnect={p.onReconnect} />
      {copied && <span className="muted">{copied}</span>}
      <div className="spacer" />
      <button
        title="Copy the whole terminal buffer"
        aria-label="Copy all"
        onClick={() => p.copyAll().then(() => setCopied("Copied"), () => setCopied("Copy failed (the browser needs HTTPS for the clipboard)"))}
      >
        <Copy size={15} aria-hidden />
        <span className="lbl">Copy all</span>
      </button>
      <button title="Clear the terminal" aria-label="Clear" onClick={() => { p.clear(); setCopied(""); }}>
        <Eraser size={15} aria-hidden />
        <span className="lbl">Clear</span>
      </button>
      <FullscreenButton box={p.box} />
      {p.children}
    </div>
  );
}
