import { FormEvent, Fragment, ReactNode, useEffect, useState } from "react";
import { ApiError, get, NodeStatus, post, put } from "../api";
import { CheckItem, CheckList, CheckStatus, StatusLine } from "../Checks";
import DialogAdvanced from "./DialogAdvanced";
import PrepareNode from "./PrepareNode";
import { ErrorBox, Field, Modal, StateBadge, useAction, useConfirm } from "../ui";
import { LimitField, MigNetInfo, MigNetResult, MigrationNetworkDialog, MigrationResult, NetworkSelect, resultItems, useMigNet } from "./MigrationNetwork";

export interface Capability {
  ok: boolean;
  reason?: string;
}

/** What GET /api/nodes/{id} says about how a node is reached (it also answers for a node that is offline). */
interface ConnInfo {
  addr: string;
  user: string;
  jump?: string;
  privilege: string;
  detectedPrivilege?: string;
  privilegeNote?: string;
  migrationAddress: string;
  effectiveMigrationAddress: string;
  resolvedIps?: string[];
  hostUuid?: string;
  hostname?: string;
  capabilities: Record<string, Capability>;
}

interface TestResult {
  hostname: string;
  libvirtVersion: string;
  hostUuid: string;
  privilege: string;
  privilegeNote: string;
  resolvedIps?: string[];
  ok?: boolean;
  steps?: { id: string; title: string; status: string; detail?: string }[];
}

/** The server's test steps as list items; a step that did not run shows as "Not run". */
function stepItems(steps: NonNullable<TestResult["steps"]>): CheckItem[] {
  return steps.map((s) => ({
    key: s.id,
    status: (s.status === "skipped" ? "pending" : s.status) as CheckStatus,
    title: s.title,
    detail: s.status === "skipped" ? "Not run" : s.detail,
  }));
}
interface EditResponse {
  test?: TestResult;
  warnings?: string[];
  notes?: string[];
}

const mono = (v: ReactNode, key: string) => (
  <span className="mono" key={key}>
    {v}
  </span>
);

const Facts = ({ rows }: { rows: [string, ReactNode][] }) => (
  <dl className="connfacts">
    {rows.map(([k, v]) => (
      <Fragment key={k}>
        <dt>{k}</dt>
        <dd>{v}</dd>
      </Fragment>
    ))}
  </dl>
);

/** The Connection card of a node's Settings tab: how it is reached, whether the node is prepared, and the edit actions. */
export default function ConnectionCard({ n, admin }: { n: NodeStatus; admin: boolean }) {
  const [info, setInfo] = useState<ConnInfo | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [editing, setEditing] = useState(false);
  const [preparing, setPreparing] = useState(false);
  const [changing, setChanging] = useState(false);
  const [notes, setNotes] = useState<CheckItem[]>([]);
  const [migDone, setMigDone] = useState<CheckItem[]>([]);
  const mig = useMigNet(n.id, [n.state, n.addr, n.migrationAddress, n.migrationLimit]);
  const act = useAction();
  const confirm = useConfirm();
  // Reloaded when the node's connection settings or state change, not on a timer.
  useEffect(() => {
    get<ConnInfo>(`/nodes/${n.id}`).then(
      (d) => {
        setInfo(d);
        setErr(undefined);
      },
      (e) => setErr(String(e.message ?? e)),
    );
  }, [n.id, n.state, n.addr, n.user, n.jump, n.privilege, n.migrationAddress]);

  const forget = async () => {
    const ok = await confirm.ask({
      title: "Forget host key",
      message: `The SSH host key this server has recorded for ${n.addr} is deleted and the node reconnects. The key it presents next is trusted without question, so do this only for a node you reinstalled or whose key you know changed.`,
      confirm: "Forget host key",
    });
    if (ok) await act.run(() => post(`/nodes/${n.id}/forget-host-key`));
  };

  // One warning: the node is not prepared (or was prepared by an older version), or else the first thing that needs root and is off.
  const unprepared = n.state === "online" && info?.detectedPrivilege === "none";
  const older = !!info?.privilegeNote?.includes("older version");
  const blocked = info ? Object.values(info.capabilities).filter((c) => !c.ok) : [];
  const hostKeyError = !!n.error && n.error.includes("key mismatch");
  return (
    <div className="card">
      <h3>Connection</h3>
      <Facts
        rows={[
          ["Address", mono(n.addr, "a")],
          ["SSH user", mono(n.user, "u")],
          ["State", <StateBadge key="s" state={n.state} />],
          ...(mig.info?.show
            ? ([
                [
                  "Migration network",
                  <span key="mn" className="migrow">
                    <span>{mig.info.current}</span>
                    {admin && (
                      <button className="link" onClick={() => setChanging(true)} aria-label="Change the migration network">
                        Change
                      </button>
                    )}
                  </span>,
                ],
              ] as [string, ReactNode][])
            : []),
        ]}
      />
      {n.error && <StatusLine status="error">{n.error}</StatusLine>}
      {hostKeyError && admin && (
        <p>
          <button onClick={forget} disabled={act.busy} title="For a node that was reinstalled: trust the SSH host key it presents next">
            Forget host key
          </button>
        </p>
      )}
      <ErrorBox error={err} />
      <details className="conndetails">
        <summary>Details</summary>
        <Facts
          rows={[
            ["Resolved IP", info?.resolvedIps?.length ? mono(info.resolvedIps.join(", "), "r") : "–"],
            ["Reported hostname", info?.hostname ? mono(info.hostname, "h") : n.state === "online" ? "–" : "unknown while offline"],
            ["Migration address", mono(info?.effectiveMigrationAddress ?? n.addr, "m")],
            ["Limit on a shared link", mig.info?.limitText ?? "–"],
            ["Jump host", n.jump ? mono(n.jump, "j") : "none"],
          ]}
        />
      </details>
      {unprepared ? (
        <div className="warnrow">
          <StatusLine status="warning">
            {older ? "This node was prepared by an older version, so features that need root are off." : "This node is not prepared, so features that need root are off."}
          </StatusLine>
          {admin && <button onClick={() => setPreparing(true)}>Prepare this node</button>}
        </div>
      ) : (
        blocked.length > 0 && <StatusLine status="warning">{blocked[0].reason ?? "A feature that needs root is unavailable."}</StatusLine>
      )}
      <ErrorBox error={act.error} />
      {notes.length > 0 && <CheckList summary={false} label="Result of the change" items={notes} />}
      {migDone.length > 0 && <MigrationResult items={migDone} onClose={() => setMigDone([])} />}
      {admin && (
        <div className="actions">
          <button onClick={() => setEditing(true)}>Edit connection</button>
        </div>
      )}
      {confirm.dialog}
      {preparing && <PrepareNode mode="prepare" nodeId={n.id} nodeUser={n.user} addr={n.addr} jump={n.jump ?? ""} onClose={() => setPreparing(false)} onRegistered={() => setPreparing(false)} />}
      {changing && mig.info && (
        <MigrationNetworkDialog
          node={n.id}
          info={mig.info}
          onClose={() => setChanging(false)}
          onSaved={(r: MigNetResult) => {
            setChanging(false);
            setMigDone(resultItems(r, "Migration settings saved."));
          }}
        />
      )}
      {editing && info && (
        <EditConnection
          n={n}
          info={info}
          mig={mig.info}
          onClose={() => setEditing(false)}
          onSaved={(r) => {
            setEditing(false);
            setNotes([
              { status: "ok", detail: "Connection settings saved." },
              ...(r.warnings ?? []).map((t): CheckItem => ({ status: "warning", detail: t })),
              ...(r.notes ?? []).map((t): CheckItem => ({ status: "info", detail: t })),
            ]);
          }}
        />
      )}
    </div>
  );
}

function EditConnection({ n, info, mig: migInfo, onClose, onSaved }: { n: NodeStatus; info: ConnInfo; mig?: MigNetInfo; onClose: () => void; onSaved: (r: EditResponse) => void }) {
  const [addr, setAddr] = useState(n.addr);
  const [user, setUser] = useState(n.user);
  const [jump, setJump] = useState(n.jump ?? "");
  const [mig, setMig] = useState(info.migrationAddress === migInfo?.management.address ? "" : info.migrationAddress);
  const [limit, setLimit] = useState(migInfo?.limit ?? "");
  const [publicKey, setPublicKey] = useState("");
  const [copied, setCopied] = useState(false);
  const [tested, setTested] = useState<EditResponse | undefined>();
  const [failed, setFailed] = useState(false); // the last save was refused because the connection test failed
  const act = useAction();
  useEffect(() => {
    get<{ publicKey: string }>("/nodes").then((d) => setPublicKey(d.publicKey), () => {});
  }, []);
  // The access mode is detected by the server when the login changes; it is not sent.
  // The migration settings are sent only when the choices were loaded, so that a slow load cannot reset them.
  const body = (force: boolean) => ({ addr, user, jump, force, ...(migInfo ? { migrationAddress: mig, migrationLimit: limit } : {}) });
  const test = () =>
    act.run(async () => {
      setTested(undefined);
      setFailed(false);
      setTested(await post<EditResponse>(`/nodes/${n.id}/test-connection`, body(false)));
    });
  const save = async (force: boolean) => {
    let saved: EditResponse | undefined;
    setTested(undefined);
    const ok = await act.run(async () => {
      try {
        saved = await put<EditResponse>(`/nodes/${n.id}`, body(force));
      } catch (e) {
        // A refused save carries the steps of its trial connection.
        if (e instanceof ApiError && e.data?.test?.steps) setTested({ test: e.data.test });
        setFailed(e instanceof ApiError && !!e.data?.connection);
        throw e;
      }
    });
    if (ok) onSaved(saved ?? {});
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    return save(false);
  };
  const steps = tested?.test?.steps;
  return (
    <Modal title="Edit connection" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="grid2">
          <Field label="Address" hint="Host name or IP, optionally with :port.">
            <input value={addr} onChange={(e) => setAddr(e.target.value)} required />
          </Field>
          <Field label="SSH user" hint="virticle-node, or root.">
            <input value={user} onChange={(e) => setUser(e.target.value)} required />
          </Field>
        </div>
        <DialogAdvanced>
          <div className="grid2">
            <Field label="Jump host (optional)" hint="user@address of a host to connect through.">
              <input value={jump} onChange={(e) => setJump(e.target.value)} />
            </Field>
            {migInfo?.show && <NetworkSelect info={migInfo} value={mig} onChange={setMig} />}
            {migInfo && !migInfo.show && <LimitField value={limit} onChange={setLimit} />}
          </div>
          {migInfo?.show && <LimitField value={limit} onChange={setLimit} />}
          <p className="muted">Add this key to ~/.ssh/authorized_keys of the SSH user on the node.</p>
          <div className="keybox">
            <code>{publicKey}</code>
            <button type="button" className="link" onClick={() => navigator.clipboard?.writeText(publicKey).then(() => setCopied(true))}>
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
        </DialogAdvanced>
        {steps && <CheckList sort={false} label="Connection test" items={stepItems(steps)} />}
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="button" onClick={test} disabled={act.busy}>
            Test connection
          </button>
          {failed && (
            <button type="button" onClick={() => save(true)} disabled={act.busy} title="For a node that is down: save the settings without a working connection">
              Save anyway
            </button>
          )}
          <button className="primary" disabled={act.busy}>
            Save
          </button>
        </footer>
      </form>
    </Modal>
  );
}
