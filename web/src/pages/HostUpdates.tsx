import { useEffect, useState } from "react";
import { get, HostUpdates as Updates } from "../api";
import { StatusLine } from "../Checks";

/** Pending package updates of a node (read-only; nothing is installed from here). */
export default function HostUpdates({ node, online }: { node: number; online: boolean }) {
  const [u, setU] = useState<Updates | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const load = (refresh: boolean) => {
    setBusy(true);
    get<Updates>(`/nodes/${node}/updates${refresh ? "?refresh=1" : ""}`)
      .then((d) => {
        setU(d);
        setErr(d.error);
      })
      .catch((e) => setErr(String(e.message ?? e)))
      .finally(() => setBusy(false));
  };
  useEffect(() => {
    setU(undefined);
    if (online) load(false);
  }, [node, online]);
  if (!online) return null;
  if (err) return <StatusLine status="error">Updates: {err}</StatusLine>;
  if (!u) return <span className="muted">{busy ? "Checking updates" : ""}</span>;
  if (u.family === "unsupported") return <span className="muted" title={u.note}>Updates not checked ({u.distro || "unknown system"})</span>;
  return (
    <div className="hostupdates">
      <button className="link" onClick={() => setOpen(!open)} aria-expanded={open}>
        {u.updates.length === 0 ? "Up to date" : `${u.updates.length} update${u.updates.length === 1 ? "" : "s"} pending`}
      </button>
      {u.rebootRequired && (
        <span className="badge s-paused" title={u.rebootReason}>
          reboot required
        </span>
      )}
      {open && (
        <div className="updlist">
          {u.rebootRequired && <div className="muted">{u.rebootReason}</div>}
          <ul>
            {u.updates.map((p) => (
              <li key={p.name}>
                <code>{p.name}</code> {p.from ? `${p.from} → ` : ""}
                {p.version} {p.source && <span className="muted">({p.source})</span>}
              </li>
            ))}
          </ul>
          <small>
            {u.note} {u.metadataAt ? `Package lists last refreshed ${new Date(u.metadataAt * 1000).toLocaleString()}. ` : ""}Checked {new Date(u.checkedAt * 1000).toLocaleTimeString()}.{" "}
            <button className="link" disabled={busy} onClick={() => load(true)}>
              Check again
            </button>
          </small>
        </div>
      )}
    </div>
  );
}
