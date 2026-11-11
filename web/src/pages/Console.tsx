import { ReactNode, useEffect, useRef, useState } from "react";
import RFB from "@novnc/novnc/lib/rfb";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { wsURL } from "../api";
import { cssVar, useTheme } from "../theme";
import { SerialBar, Status, VncBar } from "./ConsoleToolbar";

const termTheme = () => ({ background: cssVar("--term-bg", "#1B252B"), foreground: cssVar("--term-fg", "#F4F6F7"), cursor: cssVar("--term-fg", "#F4F6F7"), selectionBackground: cssVar("--term-sel", "#51626E") });

/** variant: "tab" is the console tab, "inline" the fixed box in the overview card, "page" the standalone window. */
export type Variant = "tab" | "inline" | "page";

export function VNCConsole({ node, uuid, name, variant = "tab", extra }: { node: number; uuid: string; name?: string; variant?: Variant; extra?: ReactNode }) {
  const holder = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const [fit, setFit] = useState(true);
  const fitRef = useRef(fit);
  fitRef.current = fit;
  useEffect(() => {
    if (rfb.current) rfb.current.scaleViewport = fit;
  }, [fit]);
  const rfb = useRef<RFB | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const el = holder.current!;
    setStatus("connecting");
    const r = new RFB(el, wsURL(`/nodes/${node}/vms/${uuid}/vnc`), { wsProtocols: ["binary"] });
    r.scaleViewport = fitRef.current;
    r.background = cssVar("--screen", "#1B252B");
    r.addEventListener("connect", () => setStatus("connected"));
    r.addEventListener("disconnect", () => setStatus("closed"));
    r.addEventListener("credentialsrequired", () => {
      const password = window.prompt("This VM's display is password protected. VNC password:");
      if (password !== null) r.sendCredentials({ password });
      else r.disconnect();
    });
    rfb.current = r;
    return () => {
      rfb.current = null;
      try {
        r.disconnect();
      } catch {
        /* already closed */
      }
      el.replaceChildren();
    };
  }, [node, uuid, attempt]);

  return (
    <div className={`console vc ${variant}`} ref={box}>
      <VncBar status={status} onReconnect={() => setAttempt((a) => a + 1)} rfb={() => rfb.current} box={box} node={node} uuid={uuid} name={name} fit={fit} onFit={setFit}>
        {extra}
      </VncBar>
      <div className={"screen" + (fit ? "" : " actual")} ref={holder} onClick={() => rfb.current?.focus()} />
    </div>
  );
}

export function SerialConsole({ node, uuid, variant = "tab", extra }: { node: number; uuid: string; variant?: Variant; extra?: ReactNode }) {
  const holder = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [detail, setDetail] = useState<string | undefined>();
  const [attempt, setAttempt] = useState(0);
  const termRef = useRef<Terminal | null>(null);
  const { theme } = useTheme();
  useEffect(() => {
    if (termRef.current) termRef.current.options.theme = termTheme();
  }, [theme]);

  useEffect(() => {
    const term = new Terminal({ cursorBlink: false, fontSize: 13, scrollback: 5000, fontFamily: cssVar("--mono", "monospace"), theme: termTheme() });
    termRef.current = term;
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(holder.current!);
    fit.fit();
    const onResize = () => fit.fit();
    window.addEventListener("resize", onResize);

    setStatus("connecting");
    setDetail(undefined);
    const ws = new WebSocket(wsURL(`/nodes/${node}/vms/${uuid}/serial`));
    ws.binaryType = "arraybuffer";
    const enc = new TextEncoder();
    ws.onopen = () => {
      setStatus("connected");
      term.focus();
      ws.send(enc.encode("\r")); // nudge the guest into printing its prompt
    };
    ws.onmessage = (m) => term.write(new Uint8Array(m.data as ArrayBuffer));
    ws.onclose = (e) => {
      setStatus("closed");
      setDetail(e.reason || undefined);
    };
    const input = term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(enc.encode(d)));
    return () => {
      window.removeEventListener("resize", onResize);
      input.dispose();
      ws.close();
      termRef.current = null;
      term.dispose();
    };
  }, [node, uuid, attempt]);

  const copyAll = async () => {
    const t = termRef.current;
    if (!t) return;
    t.selectAll();
    const text = t.getSelection();
    t.clearSelection();
    await navigator.clipboard.writeText(text);
  };
  return (
    <div className={`console vc ${variant}`} ref={box}>
      <SerialBar status={status} detail={detail} onReconnect={() => setAttempt((a) => a + 1)} box={box} copyAll={copyAll} clear={() => termRef.current?.clear()}>
        {extra}
      </SerialBar>
      <div className="screen term" ref={holder} />
    </div>
  );
}
