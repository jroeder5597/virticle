import { useEffect, useState } from "react";
import { Check, TriangleAlert, X } from "lucide-react";
import { bytes, get, Pool, post, Task, VMDetail } from "../api";
import { useEvents } from "../live";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";

interface CheckT {
  check: string;
  ok: boolean;
  severity: "error" | "warning" | "info";
  message: string;
}
interface Plan {
  ok: boolean;
  mode?: "live" | "offline";
  checks: CheckT[];
  source?: string;
  format?: string;
  capacity: number;
  allocation: number;
  backing: boolean;
  newPath?: string;
}

type DiskT = VMDetail["disks"][number];

const rank = (c: CheckT) => (c.ok ? 2 : c.severity === "error" ? 0 : 1);

/** Move one disk to another storage pool of the same node: pre-flight, options, then a task with progress and cancel. */
export default function DiskMoveDialog({ d, disk, onClose, onDone }: { d: VMDetail; disk: DiskT; onClose: () => void; onDone: () => void }) {
  const base = `/nodes/${d.node}/vms/${d.uuid}/disks/${disk.target}/move`;
  const oldName = (disk.source ?? "").split("/").pop() ?? "";
  const [pools, setPools] = useState<Pool[]>([]);
  const [pool, setPool] = useState("");
  const [name, setName] = useState(oldName);
  const [format, setFormat] = useState(disk.format === "raw" ? "raw" : "qcow2");
  const [deleteOld, setDeleteOld] = useState(true);
  const [flatten, setFlatten] = useState(false);
  const [plan, setPlan] = useState<Plan>();
  const [checking, setChecking] = useState(false);
  const [taskId, setTaskId] = useState(0);
  const [task, setTask] = useState<Task>();
  const act = useAction();
  const cancel = useAction();
  const confirm = useConfirm();

  useEffect(() => {
    get<Pool[]>(`/nodes/${d.node}/pools`).then((ps) => {
      const ok = ps.filter((p) => p.state === "running");
      setPools(ok);
      const cur = ok.find((p) => oldName && disk.source?.startsWith((p.target ?? "\0").replace(/\/?$/, "/")));
      const first = ok.find((p) => p.name !== cur?.name);
      if (first) setPool(first.name);
    }, () => {});
  }, [d.node]);

  // Pre-flight whenever an option changes (debounced, one request).
  useEffect(() => {
    if (taskId) return;
    let stale = false;
    setChecking(true);
    const h = setTimeout(() => {
      const q = new URLSearchParams({ pool, name, format, deleteOld: deleteOld ? "1" : "0", flatten: flatten ? "1" : "0" });
      get<Plan>(`${base}/plan?${q}`).then(
        (p) => !stale && setPlan(p),
        (e) => !stale && setPlan({ ok: false, checks: [{ check: "Pre-flight", ok: false, severity: "error", message: String(e.message ?? e) }], capacity: 0, allocation: 0, backing: false }),
      ).finally(() => !stale && setChecking(false));
    }, 300);
    return () => {
      stale = true;
      clearTimeout(h);
    };
  }, [pool, name, format, deleteOld, flatten, base, taskId]);

  // The task announces its progress on the event stream; no polling.
  useEvents((e) => {
    if (!taskId) return;
    if (e.task?.id === taskId) setTask(e.task);
    else if (e.detail === "resync") get<Task[]>("/tasks").then((ts) => setTask(ts.find((x) => x.id === taskId)), () => {});
  });

  useEffect(() => {
    if (task?.status === "ok" || task?.status === "failed") onDone();
  }, [task?.status]);

  const busy = taskId !== 0 && task?.status !== "ok" && task?.status !== "failed";
  const start = async () => {
    if (deleteOld) {
      const ok = await confirm.ask({
        title: `Move ${disk.target} and delete the old volume`,
        message: `${disk.target} of ${d.name} is copied to pool ${pool}. When the copy has finished, the old volume ${oldName} is deleted and cannot be recovered.`,
        confirm: "Move and delete old volume",
      });
      if (!ok) return;
    }
    return act.run(async () => {
      const r = await post<{ task: number }>(base, { pool, name, format, deleteOld, flatten });
      setTask(undefined);
      setTaskId(r.task);
    });
  };
  const live = plan?.mode === "live";
  const pct = Math.round((task?.status === "ok" ? 1 : task?.progress ?? 0) * 100);

  return (
    <Modal title={`Move ${disk.target} to another pool`} onClose={() => !busy && onClose()} wide>
      {confirm.dialog}
      {!taskId && (
        <>
          <p className="muted">
            <span className="mono wrap">{disk.source}</span>
            {disk.capacity ? ` · ${bytes(disk.capacity)}` : ""}
          </p>
          <div className="row">
            <Field label="Target pool">
              <select value={pool} onChange={(e) => setPool(e.target.value)} aria-label="Target pool">
                {pools.length === 0 && <option value="">no pool</option>}
                {pools.map((p) => (
                  <option key={p.name} value={p.name}>
                    {p.name} ({p.type}, {bytes(p.available)} free)
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Volume name">
              <input value={name} onChange={(e) => setName(e.target.value.trim())} aria-label="Volume name" />
            </Field>
            <Field label="Format">
              <select
                value={format}
                onChange={(e) => {
                  const f = e.target.value;
                  setName((n) => (n === oldName || n.endsWith(".qcow2") || n.endsWith(".raw") ? n.replace(/\.(qcow2|raw)$/, "") + "." + f : n));
                  setFormat(f);
                }}
                aria-label="Format"
              >
                <option value="qcow2">qcow2</option>
                <option value="raw">raw</option>
              </select>
            </Field>
          </div>
          {plan?.backing && (
            <label className="check">
              <input type="checkbox" checked={flatten} onChange={(e) => setFlatten(e.target.checked)} />
              Copy as one standalone image (this disk is a linked clone and will no longer share its base)
            </label>
          )}
          <label className="check">
            <input type="checkbox" checked={deleteOld} onChange={(e) => setDeleteOld(e.target.checked)} />
            Delete the old volume afterwards
          </label>
          <h3>Pre-flight checks</h3>
          {checking && <p className="muted">Checking</p>}
          {plan && (
            <>
              {plan.mode && (
                <p>
                  <strong>{live ? "Live: the VM keeps running while the disk is copied." : "Offline: the VM is shut off."}</strong>
                  {plan.newPath && (
                    <>
                      {" "}
                      New file: <span className="mono wrap">{plan.newPath}</span>
                    </>
                  )}
                </p>
              )}
              <ul className="checks">
                {[...plan.checks]
                  .sort((a, b) => rank(a) - rank(b))
                  .map((c, i) => (
                    <li key={i} className={c.ok ? "ok" : c.severity}>
                      <span className="mark">{c.ok ? <Check size={14} aria-label="ok" /> : c.severity === "error" ? <X size={14} aria-label="error" /> : <TriangleAlert size={14} aria-label="warning" />}</span>
                      <span>
                        <strong>{c.check}</strong>: {c.message}
                      </span>
                    </li>
                  ))}
              </ul>
            </>
          )}
          <ErrorBox error={act.error} />
          <footer>
            <button onClick={onClose}>Cancel</button>
            <button className="primary" disabled={act.busy || checking || !plan?.ok || !pool} onClick={start}>
              Move disk
            </button>
          </footer>
        </>
      )}
      {taskId !== 0 && (
        <>
          <p>
            Moving <strong>{disk.target}</strong> of <strong>{d.name}</strong> to pool <strong>{pool}</strong>.
          </p>
          <div className="bar" role="progressbar" aria-valuenow={pct}>
            <div className={task?.status === "failed" ? "full" : ""} style={{ width: pct + "%" }} />
          </div>
          <p className="muted">{task?.status === "ok" ? "Done." : task?.status === "failed" ? (task.error?.includes("cancelled") ? "Cancelled." : "Failed.") : `${pct}%`}</p>
          {!live && busy && <p className="muted">A copy of a shut-off VM's volume cannot be interrupted part-way: a cancel is honoured when the copy returns, and the new volume is then removed.</p>}
          <ErrorBox error={task?.status === "failed" ? task.error : cancel.error} />
          <footer>
            {busy ? (
              <button className="danger" disabled={cancel.busy} onClick={async () => (await confirm.ask({ title: "Stop disk move", message: `The running move of ${disk.target} of ${d.name} is stopped. The disk stays where it is and a partial copy is removed.`, confirm: "Stop move" })) && cancel.run(() => post(`${base}/cancel`))}>
                Cancel move
              </button>
            ) : (
              <button onClick={onClose}>Close</button>
            )}
          </footer>
        </>
      )}
    </Modal>
  );
}
