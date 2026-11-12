import { FormEvent, useCallback, useEffect, useState } from "react";
import { get, post } from "../api";
import { useEvents } from "../live";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import Select from "../Select";
import { CheckItem, CheckList, CheckStatus } from "../Checks";

interface Row {
  id: string;
  title: string;
  status: "ok" | "warning" | "error" | "info" | "skipped";
  detail?: string;
}
interface Job {
  rows: Row[];
  done: boolean;
  ok: boolean;
  checkOnly: boolean;
  user: string;
  addr: string;
  jump?: string;
}

const STATUS: Record<Row["status"], CheckStatus> = { ok: "ok", warning: "warning", error: "error", info: "info", skipped: "pending" };

export function rowItems(rows: Row[]): CheckItem[] {
  return rows.map((r) => ({ key: r.id, status: STATUS[r.status], title: r.title, detail: r.detail }));
}

/**
 * Makes a host ready to be a node: libvirt installed, the node command and a service account with a restricted key and a sudo rule for
 * the node command only. The administrator's password or key is sent once and kept in memory only; the form is gone
 * (and the fields emptied) as soon as it is submitted.
 */
export default function PrepareNode({ addr: addr0, jump: jump0, onClose, onRegistered }: { addr: string; jump: string; onClose: () => void; onRegistered: () => void }) {
  const [addr, setAddr] = useState(addr0);
  const [jump, setJump] = useState(jump0);
  const [adminUser, setAdminUser] = useState("root");
  const [cred, setCred] = useState("password");
  const [secret, setSecret] = useState("");
  const [checkOnly, setCheckOnly] = useState(true);
  const [user, setUser] = useState("virticle");
  const [useExisting, setUseExisting] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const [allowRoot, setAllowRoot] = useState(false);
  const [from, setFrom] = useState("");
  const [guestfs, setGuestfs] = useState(false);
  const [virtiofs, setVirtiofs] = useState(false);
  const [task, setTask] = useState<number | undefined>();
  const [job, setJob] = useState<Job | undefined>();
  const act = useAction();
  const reg = useAction();

  const load = useCallback(() => {
    if (task === undefined) return;
    get<Job>(`/prepare/${task}`).then(setJob, () => {});
  }, [task]);
  useEffect(load, [load]);
  useEvents((e) => {
    if (e.type === "task" && e.task?.id === task) load();
  });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body = {
      addr,
      jump,
      adminUser,
      password: cred === "password" ? secret : "",
      privateKey: cred === "key" ? secret : "",
      checkOnly,
      user: allowRoot ? "" : user,
      useExisting,
      confirm,
      allowRoot,
      from,
      extras: [guestfs && "guestfs", virtiofs && "virtiofs"].filter(Boolean),
    };
    const ok = await act.run(async () => {
      const r = await post<{ task: number }>("/prepare?async=1", body);
      setJob(undefined);
      setTask(r.task);
    });
    setSecret(""); // gone from the page whatever the answer
    return ok;
  };

  const register = async () => {
    if (job && (await reg.run(() => post("/nodes", { addr: job.addr, user: job.user, jump: job.jump ?? "" })))) {
      onRegistered();
    }
  };

  if (task !== undefined) {
    const running = !job || !job.done;
    return (
      <Modal title={job?.checkOnly ?? checkOnly ? "Check node" : "Prepare node"} onClose={onClose} wide>
        {job && job.rows.length > 0 ? <CheckList sort={false} label="Preparation" items={rowItems(job.rows)} /> : <p className="muted">Starting. Progress is also in the Tasks panel.</p>}
        {running && job && job.rows.length > 0 && <p className="muted">Running. Progress is also in the Tasks panel.</p>}
        <ErrorBox error={reg.error} />
        <footer>
          {!running && (
            <button type="button" onClick={() => { setTask(undefined); setJob(undefined); }}>
              Back
            </button>
          )}
          <button type="button" onClick={onClose}>
            Close
          </button>
          {!running && job?.ok && !job.checkOnly && (
            <button type="button" className="primary" disabled={reg.busy} onClick={register}>
              Add this node
            </button>
          )}
        </footer>
      </Modal>
    );
  }

  return (
    <Modal title="Prepare node" onClose={onClose} wide>
      <form onSubmit={submit} autoComplete="off">
        <p>
          Installs libvirt and QEMU, installs the node command, and creates a service account for this server with a restricted key and a sudo rule that lets it run the node command as root, and nothing else. The credential below is used for this run only and is not stored.
        </p>
        <div className="grid2">
          <Field label="Address" hint="Host name or IP, optionally with :port.">
            <input value={addr} onChange={(e) => setAddr(e.target.value)} required autoFocus />
          </Field>
          <Field label="Jump host (optional)" hint="user@address">
            <input value={jump} onChange={(e) => setJump(e.target.value)} />
          </Field>
          <Field label="Administrator user" hint="root, or a user that may use sudo.">
            <input value={adminUser} onChange={(e) => setAdminUser(e.target.value)} required />
          </Field>
          <Field label="Credential">
            <Select
              label="Credential"
              value={cred}
              onChange={(v) => {
                setCred(v);
                setSecret("");
              }}
              options={[
                { value: "password", label: "Password" },
                { value: "key", label: "Private key" },
              ]}
            />
          </Field>
        </div>
        {cred === "password" ? (
          <Field label="Password" hint="Also used for sudo when the user is not root.">
            <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} required autoComplete="new-password" />
          </Field>
        ) : (
          <Field label="Private key" hint="Paste the key. It must have no passphrase.">
            <textarea className="mono" rows={5} value={secret} onChange={(e) => setSecret(e.target.value)} required spellCheck={false} autoComplete="off" />
          </Field>
        )}
        <label className="check">
          <input type="checkbox" checked={checkOnly} onChange={(e) => setCheckOnly(e.target.checked)} /> Check only, change nothing
        </label>
        <label className="check">
          <input type="checkbox" checked={allowRoot} onChange={(e) => setAllowRoot(e.target.checked)} /> Use Root access: the controller signs in as root and no service account is created
        </label>
        {!allowRoot && (
          <>
            <div className="grid2">
              <Field label="Service account name">
                <input value={user} onChange={(e) => setUser(e.target.value)} required />
              </Field>
              <Field label="Controller address (optional)" hint="Limits the key to this source address.">
                <input value={from} onChange={(e) => setFrom(e.target.value)} />
              </Field>
            </div>
            <label className="check">
              <input type="checkbox" checked={useExisting} onChange={(e) => setUseExisting(e.target.checked)} /> The account exists already
            </label>
            {useExisting && (
              <label className="check">
                <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} /> Add it to the libvirt and kvm groups and give it the sudo rule for the node command
              </label>
            )}
          </>
        )}
        <label className="check">
          <input type="checkbox" checked={guestfs} onChange={(e) => setGuestfs(e.target.checked)} /> Install guest disk tools (guestfs)
        </label>
        <label className="check">
          <input type="checkbox" checked={virtiofs} onChange={(e) => setVirtiofs(e.target.checked)} /> Install virtiofsd
        </label>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            {checkOnly ? "Check" : "Prepare"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}
