import { FormEvent, Fragment, ReactNode, useEffect, useState } from "react";
import { ApiError, get, NodeStatus, post, put } from "../api";
import { CheckItem, CheckList, CheckStatus, StatusLine } from "../Checks";
import Select from "../Select";
import { ErrorBox, Field, Modal, StateBadge, useAction, useConfirm } from "../ui";

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

const PRIVILEGE_TEXT: Record<string, string> = {
  root: "root: the SSH user is root",
  sudo: "sudo: commands that need root run with sudo -n",
  none: "none: nothing runs as root, features that need it are off",
};

const mono = (v: ReactNode, key: string) => (
  <span className="mono" key={key}>
    {v}
  </span>
);

/** The Connection card of a node's Settings tab: how it is reached, what its login may do, and the edit actions. */
export default function ConnectionCard({ n, admin }: { n: NodeStatus; admin: boolean }) {
  const [info, setInfo] = useState<ConnInfo | undefined>();
  const [err, setErr] = useState<string | undefined>();
  const [editing, setEditing] = useState(false);
  const [notes, setNotes] = useState<CheckItem[]>([]);
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

  const blocked = info ? Object.values(info.capabilities).filter((c) => !c.ok) : [];
  const rows: [string, ReactNode][] = [
    ["Address", mono(n.addr, "a")],
    ["Reported hostname", info?.hostname ? mono(info.hostname, "h") : n.state === "online" ? "–" : "unknown while offline"],
    ["Resolved IP", info?.resolvedIps?.length ? mono(info.resolvedIps.join(", "), "r") : "–"],
    ["Migration address", mono(info?.effectiveMigrationAddress ?? n.addr, "m")],
    ["SSH user", mono(n.user, "u")],
    ["Jump host", n.jump ? mono(n.jump, "j") : "none"],
    ["Root access", info?.privilege ? <span key="p">{PRIVILEGE_TEXT[info.privilege] ?? info.privilege}</span> : "not detected yet"],
    ["State", <StateBadge key="s" state={n.state} />],
  ];
  return (
    <div className="card">
      <h3>Connection</h3>
      <dl className="connfacts">
        {rows.map(([k, v]) => (
          <Fragment key={k}>
            <dt>{k}</dt>
            <dd>{v}</dd>
          </Fragment>
        ))}
      </dl>
      {n.error && <StatusLine status="error">{n.error}</StatusLine>}
      <ErrorBox error={err} />
      {info?.privilegeNote && <p className="muted">Detected: {info.privilegeNote}.</p>}
      {info && info.privilege && info.detectedPrivilege && info.privilege !== info.detectedPrivilege && (
        <StatusLine status="warning">
          The node reports {info.detectedPrivilege}, but this node is set to {info.privilege}.
        </StatusLine>
      )}
      {blocked.length > 0 ? (
        <CheckList summary={false} label="Features that need root" items={blocked.map((c, i) => ({ key: String(i), status: "warning", detail: c.reason ?? "Unavailable" }))} />
      ) : (
        info && <StatusLine status="ok">All features that need root work with this login.</StatusLine>
      )}
      <ErrorBox error={act.error} />
      {notes.length > 0 && <CheckList summary={false} label="Result of the change" items={notes} />}
      {admin && (
        <div className="actions">
          <button onClick={() => setEditing(true)}>Edit connection</button>
          <button onClick={forget} disabled={act.busy} title="For a node that was reinstalled: trust the SSH host key it presents next">
            Forget host key
          </button>
        </div>
      )}
      {confirm.dialog}
      {editing && info && (
        <EditConnection
          n={n}
          info={info}
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

function EditConnection({ n, info, onClose, onSaved }: { n: NodeStatus; info: ConnInfo; onClose: () => void; onSaved: (r: EditResponse) => void }) {
  const [addr, setAddr] = useState(n.addr);
  const [user, setUser] = useState(n.user);
  const [jump, setJump] = useState(n.jump ?? "");
  const [priv, setPriv] = useState("keep");
  const [mig, setMig] = useState(info.migrationAddress);
  const [force, setForce] = useState(false);
  const [publicKey, setPublicKey] = useState("");
  const [copied, setCopied] = useState(false);
  const [tested, setTested] = useState<EditResponse | undefined>();
  const act = useAction();
  useEffect(() => {
    get<{ publicKey: string }>("/nodes").then((d) => setPublicKey(d.publicKey), () => {});
  }, []);
  const body = () => ({
    addr,
    user,
    jump,
    migrationAddress: mig,
    // "keep" leaves the setting unless the login changed, in which case the server detects it again.
    ...(priv === "keep" ? {} : { privilege: priv }),
    force,
  });
  const test = () =>
    act.run(async () => {
      setTested(undefined);
      setTested(await post<EditResponse>(`/nodes/${n.id}/test-connection`, body()));
    });
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let saved: EditResponse | undefined;
    setTested(undefined);
    const ok = await act.run(async () => {
      try {
        saved = await put<EditResponse>(`/nodes/${n.id}`, body());
      } catch (e) {
        // A refused save carries the steps of its trial connection.
        if (e instanceof ApiError && e.data?.test?.steps) setTested({ test: e.data.test });
        throw e;
      }
    });
    if (ok) onSaved(saved ?? {});
  };
  return (
    <Modal title="Edit connection" onClose={onClose}>
      <form onSubmit={submit}>
        <p>
          This server signs in with the key below. It must be in <code>~/.ssh/authorized_keys</code> of the SSH user on the node.
        </p>
        <div className="keybox">
          <code>{publicKey}</code>
          <button type="button" className="link" onClick={() => navigator.clipboard?.writeText(publicKey).then(() => setCopied(true))}>
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
        <div className="grid2">
          <Field label="Address" hint="Host name or IP, optionally with :port. A new address is trusted on first use.">
            <input value={addr} onChange={(e) => setAddr(e.target.value)} required />
          </Field>
          <Field label="SSH user" hint="root, or a member of the libvirt group.">
            <input value={user} onChange={(e) => setUser(e.target.value)} required />
          </Field>
        </div>
        <div className="grid2">
          <Field label="Jump host (optional)" hint="user@address of a host to connect through.">
            <input value={jump} onChange={(e) => setJump(e.target.value)} />
          </Field>
          <Field label="Migration address (optional)" hint="What other nodes use to reach this one: an IP or a name they can resolve. Empty: the address above.">
            <input value={mig} onChange={(e) => setMig(e.target.value)} />
          </Field>
        </div>
        <Field label="Root access" hint="How commands that need root run. Keep detects it again when the user or address changes.">
          <Select
            label="Root access"
            value={priv}
            onChange={setPriv}
            options={[
              { value: "keep", label: `Keep (${info.privilege || "not detected yet"})` },
              { value: "auto", label: "Detect" },
              { value: "root", label: "root" },
              { value: "sudo", label: "sudo" },
              { value: "none", label: "none" },
            ]}
          />
        </Field>
        <label className="check">
          <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} /> Save even if the connection test fails (for a node that is down)
        </label>
        {tested?.test?.steps && <CheckList sort={false} label="Connection test" items={stepItems(tested.test.steps)} />}
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="button" onClick={test} disabled={act.busy}>
            Test connection
          </button>
          <button className="primary" disabled={act.busy}>
            Save
          </button>
        </footer>
      </form>
    </Modal>
  );
}

