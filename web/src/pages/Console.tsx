import { useEffect, useRef, useState } from "react";
import RFB from "@novnc/novnc/lib/rfb";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { wsURL } from "../api";

type Status = "connecting" | "connected" | "closed";

function StatusLine({ status, detail, onReconnect }: { status: Status; detail?: string; onReconnect: () => void }) {
  return (
    <span className="muted">
      {status === "connecting" && "Connecting…"}
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

export function VNCConsole({ node, uuid }: { node: number; uuid: string }) {
  const holder = useRef<HTMLDivElement>(null);
  const rfb = useRef<RFB | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const el = holder.current!;
    setStatus("connecting");
    const r = new RFB(el, wsURL(`/nodes/${node}/vms/${uuid}/vnc`), { wsProtocols: ["binary"] });
    r.scaleViewport = true;
    r.background = "#000";
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
    <div className="console">
      <div className="toolbar">
        <StatusLine status={status} onReconnect={() => setAttempt((a) => a + 1)} />
        <div className="spacer" />
        <button disabled={status !== "connected"} onClick={() => rfb.current?.sendCtrlAltDel()}>
          Send Ctrl+Alt+Del
        </button>
        <button onClick={() => holder.current?.requestFullscreen()}>Full screen</button>
      </div>
      <div className="screen" ref={holder} onClick={() => rfb.current?.focus()} />
    </div>
  );
}

export function SerialConsole({ node, uuid }: { node: number; uuid: string }) {
  const holder = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [detail, setDetail] = useState<string | undefined>();
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const term = new Terminal({ cursorBlink: true, fontSize: 13, scrollback: 5000, theme: { background: "#0b0e14" } });
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
      term.dispose();
    };
  }, [node, uuid, attempt]);

  return (
    <div className="console">
      <div className="toolbar">
        <StatusLine status={status} detail={detail} onReconnect={() => setAttempt((a) => a + 1)} />
      </div>
      <div className="screen term" ref={holder} />
    </div>
  );
}
