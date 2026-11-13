import { useEffect, useState } from "react";
import { del, get, post } from "../api";
import { useEvents } from "../live";
import { useAction, useConfirm } from "../ui";
import { CheckList, CheckStatus, StatusLine } from "../Checks";

export interface MigTLS {
  state: "not set up" | "ok" | "expiring" | "expired" | "mismatch";
  problems?: string[];
  hostname: string;
  dir?: string;
  qemuUser?: string;
  notes?: string[];
  changes?: string[];
  note?: string;
  /** Removed by hand: the controller does not set it up again until Fix. */
  off?: boolean;
  /** The last failure of the automatic setup. */
  autoError?: string;
  daysLeft?: number;
  cert?: { subject: string; issuer: string; dnsNames: string[]; ips: string[]; notAfter: string; fingerprint: string };
  files?: { name: string; owner: string; mode: string }[];
}

/**
 * Encrypted-migration status of one node. The controller sets it up and renews it by itself; this shows one status line and, under
 * Details, what is installed with Fix (when something is wrong) and Remove. A certificate removed by hand stays removed until Fix.
 */
export default function MigrationTLS({ node, online, admin }: { node: number; online: boolean; admin: boolean }) {
  const [st, setSt] = useState<MigTLS | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const act = useAction();
  const confirm = useConfirm();
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
  // The controller works in the background: refresh when it reports a result for this node.
  useEvents((e) => {
    if (online && e.type === "node" && e.node === node && e.detail === "migration-tls") load();
  });
  if (!online) return null;
  if (err) return <StatusLine status="error">Encrypted migration: {err}</StatusLine>;
  if (!st) return <span className="muted">Checking</span>;

  const fix = (force: boolean) =>
    act.run(async () => {
      await post<MigTLS>(`/nodes/${node}/migration-tls`, { force });
      await load();
    });
  const remove = () =>
    act.run(async () => {
      await del<MigTLS>(`/nodes/${node}/migration-tls`);
      await load();
    });
  // The node's address changed and the certificate does not cover it: the files are ours and unmodified, so issuing again is safe.
  const addressOnly = st.state === "mismatch" && (st.problems ?? []).length > 0 && (st.problems ?? []).every((p) => p.includes("does not cover"));
  // The certificates came from another controller's CA (the node was prepared by another controller): say so, and Fix replaces them after a confirmation.
  const otherCA = st.state === "mismatch" && (st.problems ?? []).some((p) => p.startsWith("set up by a different controller or CA"));
  const wrong = st.state !== "ok";
  const status: CheckStatus = st.state === "ok" ? "ok" : st.state === "not set up" ? (st.off ? "info" : "warning") : st.state === "expiring" ? "warning" : "error";
  const word =
    st.state === "ok"
      ? "Encrypted"
      : st.state === "expiring"
        ? `Expires in ${st.daysLeft} days`
        : st.state === "not set up"
          ? st.off
            ? "Off"
            : "Not set up"
          : st.state === "expired"
            ? "Expired"
            : otherCA
              ? "Set up by another controller"
              : "Needs repair";
  const doFix = async () => {
    if (st.state === "mismatch" && !addressOnly) {
      const ok = await confirm.ask({
        title: "Fix encrypted migration",
        message: `The certificate files in ${st.dir} on ${st.hostname} are replaced. Existing files that this server did not create are kept as *.virticle-orig.`,
        items: st.problems ?? [],
        confirm: "Replace certificates",
      });
      if (ok) await fix(true);
      return;
    }
    await fix(false);
  };
  return (
    <div className="migtls">
      <StatusLine status={status}>{word}</StatusLine>
      <details open={wrong}>
        <summary>Details</summary>
        <div className="updlist">
          {st.autoError && <StatusLine status="warning">Automatic setup failed: {st.autoError}</StatusLine>}
          {st.problems && st.problems.length > 0 && (
            <CheckList summary={false} label="Encrypted migration problems" items={st.problems.map((p) => ({ status: st.state === "expiring" ? "warning" : "error", detail: p }))} />
          )}
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
          {act.error && <StatusLine status="error">{act.error}</StatusLine>}
          {admin && (
            <div className="actions">
              {wrong && (
                <button disabled={act.busy} onClick={doFix}>
                  Fix
                </button>
              )}
              {st.state !== "not set up" && (
                <button
                  className="danger"
                  disabled={act.busy}
                  onClick={async () =>
                    (await confirm.ask({
                      title: "Remove encrypted migration",
                      message: `The migration certificates this server installed on ${st.hostname} are removed. Live migrations to and from it will be unencrypted until you press Fix.`,
                      confirm: "Remove certificates",
                    })) && remove()
                  }
                >
                  Remove
                </button>
              )}
            </div>
          )}
        </div>
      </details>
      {confirm.dialog}
    </div>
  );
}
