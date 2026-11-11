import { useEffect, useState } from "react";
import { Lock } from "lucide-react";
import { del, get, post } from "../api";
import { useAction } from "../ui";

export interface MigTLS {
  state: "not set up" | "ok" | "expiring" | "expired" | "mismatch";
  problems?: string[];
  hostname: string;
  dir?: string;
  qemuUser?: string;
  notes?: string[];
  changes?: string[];
  note?: string;
  daysLeft?: number;
  cert?: { subject: string; issuer: string; dnsNames: string[]; ips: string[]; notAfter: string; fingerprint: string };
  files?: { name: string; owner: string; mode: string }[];
}

const badgeClass = { "not set up": "", ok: "s-ok", expiring: "s-paused", expired: "s-failed", mismatch: "s-failed" };

/** Encrypted-migration status of one node, with the admin actions to set it up, issue it again and remove it. */
export default function MigrationTLS({ node, online, admin }: { node: number; online: boolean; admin: boolean }) {
  const [st, setSt] = useState<MigTLS | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [open, setOpen] = useState(false);
  const [done, setDone] = useState<MigTLS | undefined>();
  const act = useAction();
  const load = () =>
    get<MigTLS>(`/nodes/${node}/migration-tls`).then(
      (d) => {
        setSt(d);
        setErr(undefined);
      },
      (e) => setErr(String(e.message ?? e)),
    );
  useEffect(() => {
    setSt(undefined);
    if (online) load();
  }, [node, online]);
  if (!online) return null;
  if (err) return <div className="rowerror">Encrypted migration: {err}</div>;
  if (!st) return <span className="muted">Checking…</span>;

  const setup = (force: boolean) =>
    act.run(async () => {
      setDone(await post<MigTLS>(`/nodes/${node}/migration-tls`, { force }));
      await load();
    });
  const remove = () =>
    act.run(async () => {
      setDone(await del<MigTLS>(`/nodes/${node}/migration-tls`));
      await load();
    });
  const label = st.state === "ok" ? "encrypted" : st.state === "expiring" ? `expires in ${st.daysLeft} days` : st.state;
  return (
    <div className="migtls">
      <button className="link" onClick={() => setOpen(!open)} aria-expanded={open} title="TLS for the memory and disk streams of live migrations">
        <span className={`badge ${badgeClass[st.state]}`}>
          {(st.state === "ok" || st.state === "expiring") && <Lock size={12} aria-hidden="true" />}{" "}
          {label}
        </span>
      </button>
      {admin && st.state === "not set up" && (
        <button disabled={act.busy} onClick={() => setup(false)} title="Create a certificate for this node and install it where libvirt and QEMU read it. qemu.conf is not changed.">
          Set up encrypted migration
        </button>
      )}
      {admin && (st.state === "ok" || st.state === "expiring" || st.state === "expired") && (
        <button disabled={act.busy} onClick={() => setup(false)}>
          Re-issue
        </button>
      )}
      {admin && st.state === "mismatch" && (
        <button
          disabled={act.busy}
          onClick={() => window.confirm(`Replace the certificate files in ${st.dir} on ${st.hostname}?\n\n${(st.problems ?? []).join("\n")}\n\nExisting files that this server did not create are kept as *.virt-proj-orig.`) && setup(true)}
        >
          Repair…
        </button>
      )}
      {admin && st.state !== "not set up" && (
        <button
          className="danger"
          disabled={act.busy}
          onClick={() => window.confirm(`Remove the migration certificates this server installed on ${st.hostname}? Live migrations to and from it will be unencrypted.`) && remove()}
        >
          Remove
        </button>
      )}
      {act.error && <div className="rowerror">{act.error}</div>}
      {st.problems && st.problems.length > 0 && (
        <div className="rowerror" role="status">
          {st.problems.join("; ")}
        </div>
      )}
      {open && (
        <div className="updlist">
          {st.cert ? (
            <ul>
              <li>
                Subject <code>{st.cert.subject}</code>, issued by <code>{st.cert.issuer}</code>
              </li>
              <li>
                Names <code>{[...st.cert.dnsNames, ...st.cert.ips].join(", ")}</code>
              </li>
              <li>Valid until {new Date(st.cert.notAfter).toLocaleDateString()}</li>
            </ul>
          ) : (
            <div className="muted">No certificate installed by this server.</div>
          )}
          <small>
            Directory <code>{st.dir}</code>, key readable by <code>{st.qemuUser ?? "?"}</code>. {(st.notes ?? []).join(" ")}
          </small>
        </div>
      )}
      {done && done.changes && (
        <div className="updlist">
          <small>Done: {done.changes.join("; ")}.</small>
        </div>
      )}
    </div>
  );
}
