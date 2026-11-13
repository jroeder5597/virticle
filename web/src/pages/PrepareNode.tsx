import { FormEvent, useCallback, useEffect, useRef, useState } from "react";
import { get, post, put } from "../api";
import { useEvents } from "../live";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import Select from "../Select";
import { CheckItem, CheckList, CheckStatus } from "../Checks";
import DialogAdvanced from "./DialogAdvanced";

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
 * The Add node dialog. It makes a host ready to be a node (libvirt, the node command, and unless Root access is chosen a service
 * account with a restricted key and a sudo rule for the node command only) and then registers it with the account that the
 * preparation settled on. Preparing a host that is already prepared changes nothing. The administrator's password or key is sent with
 * each run and kept in memory only; it is never shown again and never stored, and it is emptied when a real run has prepared the node and
 * when the dialog closes. A Check, or a run that stopped, keeps it in memory so that the next button on the result needs no retyping.
 *
 * A node that trusts another Virticle controller's key stops the run at the "Other controller" row with a question. "Take over node"
 * answers yes: the run starts again with takeOver set, and the node is registered with takeOver too (its old migration certificates
 * are then replaced).
 *
 * mode "add" registers the node afterwards. mode "prepare" is for a node that is already registered: it prepares it again and does
 * not register it a second time (the login user of the node is updated if the preparation settled on another account).
 * "Already prepared" (under Advanced) registers a node without an administrator login, for hosts prepared on the command line.
 */
export default function PrepareNode({ addr: addr0, jump: jump0, mode = "add", nodeId, nodeUser, onClose, onRegistered }: { addr: string; jump: string; mode?: "add" | "prepare"; nodeId?: number; nodeUser?: string; onClose: () => void; onRegistered: () => void }) {
  const adding = mode === "add";
  const [addr, setAddr] = useState(addr0);
  const [jump, setJump] = useState(jump0);
  const [adminUser, setAdminUser] = useState("root");
  const [cred, setCred] = useState("password");
  const [secret, setSecret] = useState("");
  const [allowRoot, setAllowRoot] = useState(false);
  const [direct, setDirect] = useState(false);
  const [sshUser, setSshUser] = useState("virticle-node");
  const [advOpen, setAdvOpen] = useState(false);
  const [task, setTask] = useState<number | undefined>();
  const [job, setJob] = useState<Job | undefined>();
  const [lastCheck, setLastCheck] = useState(true);
  const [takeOver, setTakeOver] = useState(false);
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

  const start = async (checkOnly: boolean, takeOverNow = takeOver) => {
    setTakeOver(takeOverNow);
    const body = {
      addr,
      jump,
      adminUser,
      password: cred === "password" ? secret : "",
      privateKey: cred === "key" ? secret : "",
      checkOnly,
      allowRoot,
      takeOver: takeOverNow,
    };
    const ok = await act.run(async () => {
      const r = await post<{ task: number }>("/prepare?async=1", body);
      setJob(undefined);
      setLastCheck(checkOnly);
      setTask(r.task);
    });
    return ok;
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (direct) {
      if (await reg.run(() => post("/nodes", { addr, user: sshUser, jump }))) onRegistered();
      return;
    }
    // Enter in a field presses the first submit button, which is Check.
    const who = (e.nativeEvent as SubmitEvent).submitter?.getAttribute("value");
    return start(who !== "add");
  };

  // What follows a successful preparation: a new node is registered with the account that was settled on; a node that is already
  // registered only gets that account as its login when it differs.
  const register = async () => {
    if (!job) return;
    const ok = await reg.run(async () => {
      if (adding) await post("/nodes", { addr: job.addr, user: job.user, jump: job.jump ?? "", takeOver });
      else if (nodeId !== undefined && job.user && job.user !== nodeUser) await put(`/nodes/${nodeId}`, { user: job.user });
    });
    if (ok && adding) onRegistered(); // a node that was prepared again stays on its page with the result shown
  };
  const autoDone = useRef(false);
  useEffect(() => {
    if (job?.done && job.ok && !job.checkOnly && !autoDone.current) {
      autoDone.current = true;
      setSecret(""); // prepared: the credential is gone from the page
      register();
    }
  });

  const label = adding ? "Add node" : "Prepare node";
  if (task !== undefined) {
    const running = !job || !job.done;
    const checked = !running && !!job?.checkOnly && !!job?.ok;
    const prepared = !running && !!job?.ok && !job.checkOnly;
    const asksTakeOver = !running && !job?.ok && !!job?.rows.some((r) => r.id === "controller" && r.status === "error");
    return (
      <Modal title={job?.checkOnly ?? lastCheck ? "Check node" : label} onClose={onClose} wide>
        {job && job.rows.length > 0 ? <CheckList sort={false} label="Preparation" items={rowItems(job.rows)} /> : <p className="muted">Starting. Progress is also in the Tasks panel.</p>}
        {running && job && job.rows.length > 0 && <p className="muted">Running. Progress is also in the Tasks panel.</p>}
        {checked && <p className="muted">The check changed nothing. Press {label} to make the changes listed above.</p>}
        <ErrorBox error={act.error || reg.error} />
        <footer>
          {!running && (
            <button type="button" onClick={() => { setTask(undefined); setJob(undefined); setTakeOver(false); autoDone.current = false; }}>
              Back
            </button>
          )}
          <button type="button" onClick={onClose}>
            Close
          </button>
          {asksTakeOver && (
            <button type="button" className="primary" disabled={act.busy} onClick={() => start(job?.checkOnly ?? false, true)}>
              Take over node
            </button>
          )}
          {checked && (
            <button type="button" className="primary" disabled={act.busy} onClick={() => start(false)}>
              {label}
            </button>
          )}
          {prepared && reg.error && (
            <button type="button" className="primary" disabled={reg.busy} onClick={register}>
              {adding ? "Add node" : "Try again"}
            </button>
          )}
        </footer>
      </Modal>
    );
  }

  return (
    <Modal title={adding ? "Add node" : "Prepare node"} onClose={onClose} wide>
      <form onSubmit={submit} autoComplete="off">
        <p>
          {direct
            ? "Connects to a node that is already prepared; no administrator login is needed."
            : "Installs libvirt and the node command on the host if they are missing, then adds it. The credential is used for this run only and is not stored."}
        </p>
        <div className="grid2">
          <Field label="Address" hint="Host name or IP, optionally with :port.">
            <input value={addr} onChange={(e) => setAddr(e.target.value)} required autoFocus={!addr0} readOnly={!adding} />
          </Field>
          {direct ? (
            <Field label="SSH user" hint="virticle-node, or root for a node prepared with Root access.">
              <input value={sshUser} onChange={(e) => setSshUser(e.target.value)} required />
            </Field>
          ) : (
            <Field label="Administrator user" hint="root, or a user that may use sudo.">
              <input value={adminUser} onChange={(e) => setAdminUser(e.target.value)} required />
            </Field>
          )}
        </div>
        {!direct && (
          <>
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
            {cred === "password" ? (
              <Field label="Password" hint="Also used for sudo when the user is not root.">
                <input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} required autoComplete="new-password" autoFocus={!!addr0} />
              </Field>
            ) : (
              <Field label="Private key" hint="Paste the key. It must have no passphrase.">
                <textarea className="mono" rows={5} value={secret} onChange={(e) => setSecret(e.target.value)} required spellCheck={false} autoComplete="off" />
              </Field>
            )}
            <label className="check">
              <input type="checkbox" checked={allowRoot} onChange={(e) => setAllowRoot(e.target.checked)} /> Root access instead of a service account
            </label>
          </>
        )}
        <DialogAdvanced open={advOpen} onToggle={setAdvOpen}>
          <Field label="Jump host (optional)" hint="user@address of a host to connect through.">
            <input value={jump} onChange={(e) => setJump(e.target.value)} />
          </Field>
          {adding && (
            <p>
              <button type="button" className="link" onClick={() => setDirect(!direct)}>
                {direct ? "Use an administrator login instead" : "Already prepared: connect without an administrator login"}
              </button>
            </p>
          )}
        </DialogAdvanced>
        <ErrorBox error={act.error || reg.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          {!direct && (
            <button type="submit" value="check" disabled={act.busy} title="Look at the host and show what would be done; changes nothing">
              Check
            </button>
          )}
          <button type="submit" value="add" className="primary" disabled={act.busy || reg.busy}>
            {label}
          </button>
        </footer>
      </form>
    </Modal>
  );
}
