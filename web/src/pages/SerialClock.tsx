// Hardware areas "Serial console" and "Clock" (Hardware tab and Actions > Edit Settings).
import { useCallback, useEffect, useState } from "react";
import { get, post, VMDetail } from "../api";
import { CheckStatus, StatusLine } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, useConfirm } from "../ui";

interface HwSent {
  applied: string;
  needsRestart: boolean;
  note?: string;
}
type Send = (method: string, path: string, body?: unknown) => Promise<HwSent>;

interface CardProps {
  d: VMDetail;
  base: string;
  send: Send;
  run: (fn: () => Promise<unknown>) => Promise<boolean>;
  busy: boolean;
  /** Shown in a dialog: no card frame, no element ids. */
  bare?: boolean;
}

function Frame({ id, title, bare, children }: { id: string; title: string; bare?: boolean; children: React.ReactNode }) {
  return (
    <section className={bare ? "hwbare" : "card"} id={bare ? undefined : id} aria-label={bare ? title : undefined}>
      {!bare && (
        <div className="chead">
          <h3>{title}</h3>
        </div>
      )}
      {children}
    </section>
  );
}

const serialTypeLabel = (t: string) => (t === "pty" ? "pty (used by the Console tab)" : t || "unknown");

export function SerialCard({ d, send, run, busy, bare }: CardProps) {
  const confirm = useConfirm();
  const pty = d.serials.some((s) => s.type === "pty");
  const pending = d.serialPending;
  const canAdd = d.serials.length === 0 && pending !== "add";
  const canRemove = pty && pending !== "remove";
  return (
    <Frame id="hw-serial" title="Serial console" bare={bare}>
      {confirm.dialog}
      <dl className="hwdl">
        <dt>Serial console</dt>
        <dd>
          {d.serials.length === 0
            ? "None"
            : d.serials.map((s, i) => (
                <div key={i}>
                  {serialTypeLabel(s.type)}, port {s.port || "0"}
                  {s.type !== "pty" && <span className="muted"> (read-only, edit on the XML tab)</span>}
                </div>
              ))}
        </dd>
      </dl>
      {pending && <StatusLine status="info">{pending === "add" ? "Saved: a serial console is added after the VM is powered off and started." : "Saved: the serial console is removed after the VM is powered off and started."}</StatusLine>}
      <div className="actions" style={{ marginTop: 10 }}>
        <button
          disabled={!canAdd || busy}
          title={canAdd ? undefined : pending === "add" ? "Already saved" : "The VM already has a serial device"}
          onClick={() => run(() => send("POST", "/serial-console"))}
        >
          Add serial console
        </button>
        <button
          className="danger"
          disabled={!canRemove || busy}
          title={canRemove ? undefined : pending === "remove" ? "Already saved" : "The VM has no pty serial console"}
          onClick={async () => {
            if (!(await confirm.ask({ title: "Remove serial console", message: `The serial console is removed from ${d.name}. The Console tab then has no serial terminal.`, confirm: "Remove" }))) return;
            run(() => send("DELETE", "/serial-console"));
          }}
        >
          Remove
        </button>
      </div>
      {d.state !== "shutoff" && <p className="muted">A running VM is changed live when libvirt allows it; otherwise the change applies after the VM is powered off and started.</p>}
    </Frame>
  );
}

interface ClockInfo {
  offset: string;
  saved: string;
  sync: boolean;
  guest?: string;
  skewMs?: number;
  guestStatus?: string;
}

/** Wording of an offset in milliseconds: "12 s behind", "3 min ahead". */
export function skewText(ms: number): string {
  const a = Math.abs(ms) / 1000;
  if (a < 0.5) return "in sync";
  const n = a < 10 ? `${a.toFixed(1)} s` : a < 90 ? `${Math.round(a)} s` : a < 5400 ? `${Math.round(a / 60)} min` : a < 172800 ? `${Math.round(a / 3600)} h` : `${Math.round(a / 86400)} d`;
  return `${n} ${ms < 0 ? "behind" : "ahead"}`;
}

/** Green within 2 s, yellow within 60 s, red beyond. */
export function skewStatus(ms: number): CheckStatus {
  const a = Math.abs(ms);
  return a <= 2000 ? "ok" : a <= 60000 ? "warning" : "error";
}

export function ClockCard({ d, base, send, run, busy, bare }: CardProps) {
  const [info, setInfo] = useState<ClockInfo>();
  const [err, setErr] = useState<string>();
  const [offset, setOffset] = useState("");
  const [note, setNote] = useState<string>();
  const [syncOn, setSyncOn] = useState(true);
  const load = useCallback(() => {
    get<ClockInfo>(`${base}/clock`)
      .then((c) => {
        setInfo(c);
        setOffset(c.saved);
        setSyncOn(c.sync);
        setErr(undefined);
      })
      .catch((e) => setErr(String(e.message ?? e)));
  }, [base]);
  // On open and when the VM changes state; never on a timer.
  useEffect(load, [load, d.state, d.agent]);
  const running = d.state === "running";
  const canSync = running && d.agent === "connected";
  const why = canSync ? undefined : !running ? "The VM is not running" : "Needs the guest agent to be connected";
  const editable = info?.saved === "utc" || info?.saved === "localtime";
  const differs = info && info.offset !== info.saved;
  const label = (o: string) => (o === "utc" ? "UTC" : o === "localtime" ? "Local time" : o || "Not set");
  return (
    <Frame id="hw-clock" title="Clock" bare={bare}>
      <ErrorBox error={err} />
      <div className="hwadd" style={{ marginTop: 0 }}>
        <div style={{ flex: "1 1 12em", minWidth: 0 }}>
          <Field label="Clock offset">
          {editable ? (
            <Select
              label="Clock offset"
              value={offset}
              onChange={setOffset}
              options={[
                { value: "utc", label: "UTC" },
                { value: "localtime", label: "Local time" },
              ]}
            />
          ) : (
            <div>{label(info?.saved ?? "")}</div>
          )}
          </Field>
        </div>
        <button disabled={!editable || offset === info?.saved || busy} onClick={() => run(async () => { await send("PUT", "/clock", { offset }); load(); })}>
          Save
        </button>
      </div>
      <p className="muted">UTC suits Linux and BSD guests. Windows keeps local time. A change applies after the VM is powered off and started.</p>
      {differs && <StatusLine status="info">Saved offset is {label(info.saved)}; the running VM still uses {label(info.offset)}.</StatusLine>}
      <label className="check">
        <input
          type="checkbox"
          checked={syncOn}
          disabled={!info || busy}
          onChange={async (e) => {
            const on = e.target.checked;
            setSyncOn(on);
            await run(() => send("PUT", "/clock", { sync: on }));
            load(); // show what is really stored
          }}
        />{" "}
        Sync clock after resume, revert and migration
      </label>
      <div style={{ marginTop: 8 }}>
        {info?.skewMs !== undefined ? (
          <StatusLine status={skewStatus(info.skewMs)}>
            Guest clock: {skewText(info.skewMs)}
            {info.guest && <span className="muted"> ({new Date(info.guest).toLocaleString()})</span>}
          </StatusLine>
        ) : (
          <StatusLine status="info">Guest clock: {info?.guestStatus ?? "not read"}</StatusLine>
        )}
      </div>
      {note && <StatusLine status="warning">{note}</StatusLine>}
      <div className="actions" style={{ marginTop: 10 }}>
        <button disabled={!canSync || busy} title={why} onClick={() => run(async () => { setNote((await post<{ note?: string }>(`${base}/clock/sync`)).note); load(); })}>
          Sync guest clock
        </button>
        <button disabled={!running || busy} title={running ? undefined : "The VM is not running"} onClick={load}>
          Read clock
        </button>
      </div>
    </Frame>
  );
}
