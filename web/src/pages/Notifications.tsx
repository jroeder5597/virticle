import { FormEvent, useCallback, useEffect, useState } from "react";
import { del, get, post, put } from "../api";
import { CheckStatus, StatusDot } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import DialogAdvanced from "./DialogAdvanced";
import "./notify.css";

type Kind = "slack" | "mattermost" | "discord" | "webhook";

interface Dest {
  id: string;
  type: Kind;
  name: string;
  host: string;
  tail: string;
  insecure?: boolean;
  severity: "warning" | "critical";
  cleared: boolean;
  hasSecret: boolean;
  readOnly?: boolean;
  state: "idle" | "ok" | "failing";
  error?: string;
  at?: number;
  lastOk?: number;
}

const kinds: { value: Kind; label: string }[] = [
  { value: "slack", label: "Slack" },
  { value: "mattermost", label: "Mattermost" },
  { value: "discord", label: "Discord" },
  { value: "webhook", label: "Webhook" },
];
const kindLabel = (k: Kind) => kinds.find((x) => x.value === k)?.label ?? k;

function ago(unix: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (s < 90) return "just now";
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

const sends = (d: Dest) => (d.severity === "critical" ? "Critical only" : "Warning and critical") + (d.cleared ? ", with cleared messages" : ", no cleared messages");

/** Where alerts are sent besides the bell. */
export function AlertSettings() {
  const [dests, setDests] = useState<Dest[] | undefined>();
  const [links, setLinks] = useState(true);
  const [loadError, setLoadError] = useState<string | undefined>();
  const [editing, setEditing] = useState<Dest | "new" | undefined>();
  const [tests, setTests] = useState<Record<string, { ok: boolean; message: string }>>({});
  const act = useAction();
  const confirm = useConfirm();
  const reload = useCallback(
    () =>
      get<{ destinations: Dest[]; links: boolean }>("/notify/destinations").then(
        (r) => {
          setDests(r.destinations);
          setLinks(r.links);
          setLoadError(undefined);
        },
        (e) => setLoadError(String(e.message ?? e)),
      ),
    [],
  );
  useEffect(() => {
    reload();
    // Delivery is asynchronous: a destination turns to "Failing" after the fact.
    const t = window.setInterval(() => !document.hidden && reload(), 15000);
    return () => window.clearInterval(t);
  }, [reload]);

  const test = (d: Dest) =>
    act.run(async () => {
      const r = await post<{ ok: boolean; message: string }>(`/notify/destinations/${d.id}/test`);
      setTests((t) => ({ ...t, [d.id]: r }));
      await reload();
    });
  const remove = async (d: Dest) => {
    if (!(await confirm.ask({ title: `Delete ${d.name}`, message: `Alerts are no longer sent to ${d.name}.`, confirm: "Delete destination" }))) return;
    if (await act.run(() => del(`/notify/destinations/${d.id}`))) reload();
  };

  return (
    <>
      <div className="head">
        <h1>Alerts</h1>
        <button className="primary" onClick={() => setEditing("new")}>
          Add destination
        </button>
      </div>
      <section className="card" aria-label="Notifications">
        <h2>Notifications</h2>
        <p className="muted">{links ? "Alerts that are raised or cleared are sent to these destinations." : "Messages carry no links to Virticle until a public URL is set under Web access."}</p>
        <ErrorBox error={loadError ?? act.error} />
        {dests && dests.length === 0 && <p className="muted">No destinations yet. Add one to get alerts in Slack, Mattermost, Discord or your own system.</p>}
        <ul className="dests">
          {(dests ?? []).map((d) => {
            const status: CheckStatus = d.state === "failing" ? "error" : d.state === "ok" ? "ok" : "pending";
            const t = tests[d.id];
            return (
              <li key={d.id} aria-label={d.name}>
                <div className="dmain">
                  <div className="dtitle">
                    <strong>{d.name}</strong> <span className="muted">{kindLabel(d.type)}</span>
                  </div>
                  <div className="muted dline">
                    {d.host}, ends in {d.tail}
                  </div>
                  <div className="muted dline">{sends(d)}</div>
                  {d.hasSecret && <div className="muted dline">Messages are signed</div>}
                  {d.insecure && <div className="dline">Not encrypted: this address starts with http://</div>}
                  {d.readOnly && <div className="muted dline">Set by the -alert-webhook flag, which is deprecated. Remove the flag to change it here.</div>}
                  <div className="dline dstate">
                    <StatusDot status={status} />
                    <span>
                      {d.state === "failing" ? `Failing: ${d.error ?? "unknown error"}${d.at ? " (" + ago(d.at) + ")" : ""}` : d.state === "ok" ? `Working, last sent ${ago(d.lastOk ?? 0)}` : "Nothing sent yet"}
                    </span>
                  </div>
                  {t?.ok && (
                    <div className="dline dstate" role="status">
                      <StatusDot status="ok" />
                      <span>Test message sent: {t.message}</span>
                    </div>
                  )}
                </div>
                <div className="actions">
                  <button disabled={act.busy} onClick={() => test(d)}>
                    Send test message
                  </button>
                  {!d.readOnly && (
                    <>
                      <button disabled={act.busy} onClick={() => setEditing(d)}>
                        Edit
                      </button>
                      <button className="danger" disabled={act.busy} onClick={() => remove(d)}>
                        Delete
                      </button>
                    </>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      </section>
      {confirm.dialog}
      {editing && <DestinationDialog dest={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} onDone={reload} />}
    </>
  );
}

function DestinationDialog({ dest, onClose, onDone }: { dest?: Dest; onClose: () => void; onDone: () => void }) {
  const [type, setType] = useState<Kind>(dest?.type ?? "slack");
  const [name, setName] = useState(dest?.name ?? "Slack");
  const [url, setUrl] = useState("");
  const [severity, setSeverity] = useState(dest?.severity ?? "warning");
  const [cleared, setCleared] = useState(dest?.cleared ?? true);
  const [secret, setSecret] = useState("");
  const [dropSecret, setDropSecret] = useState(false);
  const act = useAction();
  const pickType = (k: Kind) => {
    if (name === "" || name === kindLabel(type)) setName(kindLabel(k));
    setType(k);
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body: Record<string, unknown> = { name, severity, cleared };
    if (url.trim() !== "") body.url = url.trim();
    if (type === "webhook") {
      if (secret !== "") body.secret = secret;
      if (dropSecret) body.clearSecret = true;
    }
    const ok = await act.run(() => (dest ? put(`/notify/destinations/${dest.id}`, body) : post("/notify/destinations", { ...body, type })));
    if (ok) {
      onDone();
      onClose();
    }
  };
  const web = type === "webhook";
  return (
    <Modal title={dest ? "Edit destination" : "Add destination"} onClose={onClose}>
      <form onSubmit={submit}>
        <Field label="Type">
          {dest ? <input value={kindLabel(type)} disabled aria-label="Type" /> : <Select label="Type" value={type} onChange={(v) => pickType(v as Kind)} options={kinds} />}
        </Field>
        <Field label="Name">
          <input value={name} onChange={(e) => setName(e.target.value)} maxLength={60} required />
        </Field>
        <Field label="URL" hint={web ? "Any web address that accepts a POST with JSON." : "The incoming webhook URL of the service."}>
          <input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder={dest ? `Saved: ${dest.host}, ends in ${dest.tail}. Leave empty to keep it.` : "https://"}
            required={!dest}
            autoComplete="off"
            spellCheck={false}
          />
        </Field>
        {web && url.trim().toLowerCase().startsWith("http://") && <p className="notify-warn">Not encrypted: messages sent to an http:// address can be read on the way.</p>}
        <DialogAdvanced>
          <Field label="Send">
            <Select
              label="Send"
              value={severity}
              onChange={(v) => setSeverity(v as "warning" | "critical")}
              options={[
                { value: "warning", label: "Warning and critical" },
                { value: "critical", label: "Critical only" },
              ]}
            />
          </Field>
          <label className="check">
            <input type="checkbox" checked={cleared} onChange={(e) => setCleared(e.target.checked)} /> Send a message when an alert clears
          </label>
          {web && (
            <>
              <Field label="Secret (optional)" hint="Signs each message with HMAC-SHA256 in the X-Virticle-Signature header.">
                <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} autoComplete="new-password" placeholder={dest?.hasSecret ? "Saved. Enter a new one to replace it." : ""} />
              </Field>
              {dest?.hasSecret && (
                <label className="check">
                  <input type="checkbox" checked={dropSecret} onChange={(e) => setDropSecret(e.target.checked)} /> Remove the saved secret
                </label>
              )}
            </>
          )}
        </DialogAdvanced>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            {dest ? "Save" : "Add destination"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}
