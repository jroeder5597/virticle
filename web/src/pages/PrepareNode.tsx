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
 * the node command only. The administrator's password or key is sent with each run and kept in memory only. It is never shown again
 * and never stored; it is emptied when a real preparation starts and when the dialog closes. A Check keeps it in memory so that the
 * Prepare button on the result needs no retyping.
 */
export default function PrepareNode({ addr: addr0, jump: jump0, onClose, onRegistered }: { addr: string; jump: string; onClose: () => void; onRegistered: () => void }) {
  const [addr, setAddr] = useState(addr0);
  const [jump, setJump] = useState(jump0);
  const [adminUser, setAdminUser] = useState("root");
  const [cred, setCred] = useState("password");
  const [secret, setSecret] = useState("");
  const [allowRoot, setAllowRoot] = useState(false);
  const [task, setTask] = useState<number | undefined>();
  const [job, setJob] = useState<Job | undefined>();
  const [lastCheck, setLastCheck] = useState(true);
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

  const start = async (checkOnly: boolean) => {
    const body = {
      addr,
      jump,
      adminUser,
      password: cred === "password" ? secret : "",
      privateKey: cred === "key" ? secret : "",
      checkOnly,
      allowRoot,
    };
    const ok = await act.run(async () => {
      const r = await post<{ task: number }>("/prepare?async=1", body);
      setJob(undefined);
      setLastCheck(checkOnly);
      setTask(r.task);
    });
    if (!checkOnly) setSecret(""); // a real run: gone from the page whatever the answer
    return ok;
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    // Enter in a field presses the first submit button, which is Check.
    const who = (e.nativeEvent as SubmitEvent).submitter?.getAttribute("value");
    return start(who !== "prepare");
  };

  const register = async () => {
    if (job && (await reg.run(() => post("/nodes", { addr: job.addr, user: job.user, jump: job.jump ?? "" })))) {
      onRegistered();
    }
  };

  if (task !== undefined) {
    const running = !job || !job.done;
    const checked = !running && !!job?.checkOnly && !!job?.ok;
    return (
      <Modal title={job?.checkOnly ?? lastCheck ? "Check node" : "Prepare node"} onClose={onClose} wide>
        {job && job.rows.length > 0 ? <CheckList sort={false} label="Preparation" items={rowItems(job.rows)} /> : <p className="muted">Starting. Progress is also in the Tasks panel.</p>}
        {running && job && job.rows.length > 0 && <p className="muted">Running. Progress is also in the Tasks panel.</p>}
        {checked && <p className="muted">The check changed nothing. Press Prepare to make the changes listed above.</p>}
        <ErrorBox error={act.error || reg.error} />
        <footer>
          {!running && (
            <button type="button" onClick={() => { setTask(undefined); setJob(undefined); }}>
              Back
            </button>
          )}
          <button type="button" onClick={onClose}>
            Close
          </button>
          {checked && (
            <button type="button" className="primary" disabled={act.busy} onClick={() => start(false)}>
              Prepare
            </button>
          )}
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
          {allowRoot
            ? "Installs libvirt, QEMU and the node command. This server will sign in to the node as root; no service account is created."
            : "Installs libvirt, QEMU and the node command, and sets up a service account for this server. The account can run the node command as root, and nothing else."}{" "}
          The credential below is used for this run only and is not stored.
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
          <input type="checkbox" checked={allowRoot} onChange={(e) => setAllowRoot(e.target.checked)} /> Root access instead of a service account
        </label>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" value="check" disabled={act.busy} title="Look at the host and show what would be done; changes nothing">
            Check
          </button>
          <button type="submit" value="prepare" className="primary" disabled={act.busy}>
            Prepare
          </button>
        </footer>
      </form>
    </Modal>
  );
}
