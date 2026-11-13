import { FormEvent, useCallback, useEffect, useState } from "react";
import { bytes, get, put } from "../api";
import { ErrorBox, Field, StateBadge, useAction } from "../ui";

interface Rec {
  time: number;
  size?: number;
  encrypted?: boolean;
  via: "cli" | "download" | "daily";
  ok: boolean;
  error?: string;
}
interface Status {
  daily: boolean;
  keep: number;
  dir: string;
  last?: Rec;
  lastDaily?: Rec;
  restoreCommand: string;
}

const when = (unix: number) => new Date(unix * 1000).toLocaleString();
const VIA = { cli: "command line", download: "download", daily: "daily backup" };

/** Settings > Backup: the controller's own backup. Restore is only on the command line. */
export default function ControllerBackup() {
  const [st, setSt] = useState<Status | undefined>();
  const [password, setPassword] = useState("");
  const [pass1, setPass1] = useState("");
  const [pass2, setPass2] = useState("");
  const dl = useAction();
  const daily = useAction();

  const load = useCallback(() => {
    get<Status>("/controller-backup").then(setSt, () => {});
  }, []);
  useEffect(load, [load]);

  const download = async (e: FormEvent) => {
    e.preventDefault();
    if (pass1 !== pass2) {
      dl.run(async () => {
        throw new Error("The two passphrases differ.");
      });
      return;
    }
    const ok = await dl.run(async () => {
      const res = await fetch("/api/controller-backup/download", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ password, passphrase: pass1 }),
      });
      if (!res.ok) {
        let msg = `${res.status} ${res.statusText}`;
        try {
          msg = (await res.json()).error ?? msg;
        } catch {
          /* not JSON */
        }
        throw new Error(msg);
      }
      const blob = await res.blob();
      const m = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "");
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = m ? m[1] : "virticle-backup.tar.gz";
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
    });
    setPassword(""); // gone from the page whatever the answer
    if (ok) {
      setPass1("");
      setPass2("");
      load();
    }
  };

  const setDaily = async (on: boolean) => {
    if (await daily.run(async () => setSt(await put<Status>("/controller-backup", { daily: on })))) {
      if (on) setTimeout(load, 3000); // the first copy is written at once
    }
  };

  return (
    <>
      <div className="head">
        <h1>Backup</h1>
      </div>
      <div className="cards">
        <form className="card" onSubmit={download} aria-label="Download backup" autoComplete="off">
          <h2>Download backup</h2>
          <p>
            The backup holds what makes this controller the controller: the users, the node list, the SSH key the nodes trust, the migration certificate authority and the web certificates. It does not hold VMs, storage or networks; those stay on the nodes.
          </p>
          <p>
            <strong>The file contains private keys.</strong> Keep it as safe as a root password, or give it a passphrase below.
          </p>
          <Field label="Your password" hint="Asked again because the backup holds the keys.">
            <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
          </Field>
          <Field label="Passphrase (optional)" hint="Encrypts the file. Without the passphrase nobody can open it, and it cannot be recovered.">
            <input type="password" value={pass1} onChange={(e) => setPass1(e.target.value)} autoComplete="new-password" />
          </Field>
          <Field label="Passphrase again">
            <input type="password" value={pass2} onChange={(e) => setPass2(e.target.value)} autoComplete="new-password" disabled={!pass1} />
          </Field>
          <ErrorBox error={dl.error} />
          <button className="primary" disabled={dl.busy}>
            Download backup
          </button>
          <p className="muted" role="status">
            {st?.last ? `Last backup: ${when(st.last.time)}, by ${VIA[st.last.via]}${st.last.size ? `, ${bytes(st.last.size)}` : ""}${st.last.encrypted ? ", encrypted" : ""}.` : "No backup has been taken yet."}
          </p>
          <p className="muted">
            Restore is done on the command line, with the controller stopped: <code>{st?.restoreCommand ?? "sudo systemctl stop virticle && sudo virticle restore FILE"}</code>
          </p>
        </form>

        <div className="card" role="group" aria-label="Daily backup">
          <h2>Daily backup on this server</h2>
          <label className="check">
            <input type="checkbox" checked={!!st?.daily} disabled={!st || daily.busy} onChange={(e) => setDaily(e.target.checked)} /> Keep a daily backup on this server
          </label>
          <p>
            Writes a backup to <code>{st?.dir ?? "the backups folder"}</code> at 03:30 server time and keeps the newest {st?.keep ?? 7}. The files are not encrypted; only the controller&apos;s account can read them.
          </p>
          <p>
            They protect against a bad upgrade or a mistake, not against losing this machine. For a copy elsewhere, use the download above, or run <code>virticle backup -o DIR</code> from cron.
          </p>
          <ErrorBox error={daily.error} />
          {st?.daily && st.lastDaily && (
            <p role="status">
              Last daily backup: {when(st.lastDaily.time)} <StateBadge state={st.lastDaily.ok ? "ok" : "failed"} />
              {!st.lastDaily.ok && st.lastDaily.error ? ` ${st.lastDaily.error}` : ""}
            </p>
          )}
        </div>
      </div>
    </>
  );
}
