import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { bytes, get, NodeStatus, post, Task, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";

interface Check {
  check: string;
  ok: boolean;
  severity: "error" | "warning" | "info";
  message: string;
}
interface Preview {
  ok: boolean;
  mode?: "live" | "suspended" | "offline";
  checks: Check[];
  copyBytes: number;
  mirrorBytes: number;
  encrypt?: boolean;
}

const modeText = { live: "Live: the VM keeps running", suspended: "Non-live: the VM is paused during the copy", offline: "Offline: the VM is shut off" };

/** Move a VM to another node: pre-flight checks, options, then a task with live progress. */
export default function MigrateDialog(props: { d: VMDetail; onClose: () => void }) {
  const { d } = props;
  const nav = useNavigate();
  const [nodes, setNodes] = useState<NodeStatus[]>([]);
  const [target, setTarget] = useState(0);
  const [live, setLive] = useState(true);
  const [keep, setKeep] = useState(false);
  const [encrypt, setEncrypt] = useState(true); // true = encrypt when both nodes are set up for it
  const [preview, setPreview] = useState<Preview | undefined>();
  const [checking, setChecking] = useState(false);
  const [taskId, setTaskId] = useState(0);
  const [task, setTask] = useState<Task | undefined>();
  const act = useAction();
  const cancel = useAction();
  const base = `/nodes/${d.node}/vms/${d.uuid}`;
  const running = d.state !== "shutoff";

  useEffect(() => {
    get<{ nodes: NodeStatus[] }>("/nodes").then((r) => {
      const others = r.nodes.filter((n) => n.id !== d.node);
      setNodes(others);
      const first = others.find((n) => n.state === "online");
      if (first) setTarget(first.id);
    }, () => {});
  }, [d.node]);

  // Pre-flight whenever the target or the live option changes.
  useEffect(() => {
    if (!target || taskId) return;
    let stale = false;
    setChecking(true);
    setPreview(undefined);
    post<Preview>(`${base}/migrate/check`, { target, live, encrypt: encrypt ? undefined : false }).then(
      (p) => !stale && setPreview(p),
      (e) => !stale && setPreview({ ok: false, checks: [{ check: "Pre-flight", ok: false, severity: "error", message: String(e.message ?? e) }], copyBytes: 0, mirrorBytes: 0 }),
    ).finally(() => !stale && setChecking(false));
    return () => {
      stale = true;
    };
  }, [target, live, encrypt, base, taskId]);

  // Follow the task. The VM leaves this node when the move completes, so the
  // task list is polled instead of the VM.
  useEffect(() => {
    if (!taskId) return;
    const poll = () =>
      get<Task[]>("/tasks").then((ts) => {
        const t = ts.find((x) => x.id === taskId);
        if (t) setTask(t);
      }, () => {});
    poll();
    const h = setInterval(poll, 1000);
    return () => clearInterval(h);
  }, [taskId]);

  useEffect(() => {
    if (task?.status === "ok") {
      const h = setTimeout(() => {
        props.onClose();
        nav(`/vm/${target}/${d.uuid}`);
      }, 1200);
      return () => clearTimeout(h);
    }
  }, [task?.status]);

  const start = () =>
    act.run(async () => {
      const r = await post<{ task: number }>(`${base}/migrate`, { target, live, keepSourceDisks: keep, encrypt: encrypt ? undefined : false });
      setTask(undefined);
      setTaskId(r.task);
    });

  const busy = taskId !== 0 && task?.status !== "failed" && task?.status !== "ok";
  const blocked = !preview || !preview.ok;
  const targetName = nodes.find((n) => n.id === target)?.hostname ?? "";

  return (
    <Modal title={`Migrate ${d.name}`} onClose={() => !busy && props.onClose()} wide>
      {!taskId && (
        <>
          <div className="row">
            <Field label="Target node">
              <select value={target} onChange={(e) => setTarget(Number(e.target.value))} aria-label="Target node">
                {nodes.length === 0 && <option value={0}>no other node</option>}
                {nodes.map((n) => (
                  <option key={n.id} value={n.id} disabled={n.state !== "online"}>
                    {n.hostname || n.addr}
                    {n.state !== "online" ? ` (${n.state})` : ""}
                  </option>
                ))}
              </select>
            </Field>
          </div>
          {running && (
            <label className="check">
              <input type="checkbox" checked={live} onChange={(e) => setLive(e.target.checked)} />
              Live migration (the VM keeps running; otherwise it is paused while memory is copied)
            </label>
          )}
          <label className="check">
            <input type="checkbox" checked={keep} onChange={(e) => setKeep(e.target.checked)} />
            Keep the disks on the source node (by default they are deleted after a successful move)
          </label>
          {preview?.mode !== "offline" && (
            <label className="check">
              <input type="checkbox" checked={encrypt} onChange={(e) => setEncrypt(e.target.checked)} />
              Encrypt memory and disk streams with TLS when both nodes are set up for it (Nodes page)
            </label>
          )}
          <h3>Pre-flight checks</h3>
          {checking && <p className="muted">Checking {targetName}…</p>}
          {preview && (
            <>
              {preview.mode && (
                <p>
                  <strong>{modeText[preview.mode]}.</strong>{" "}
                  {preview.mode !== "offline" && (
                    <span className={`badge ${preview.encrypt ? "s-ok" : "s-paused"}`} title={preview.encrypt ? "Memory and disk data are TLS-encrypted on the wire" : "Memory and disk data cross the network unencrypted"}>
                      {preview.encrypt ? "🔒 encrypted" : "unencrypted"}
                    </span>
                  )}{" "}
                  {preview.copyBytes > 0 && <>Files to copy: {bytes(preview.copyBytes)}. </>}
                  {preview.mirrorBytes > 0 && <>Disk data mirrored while running: up to {bytes(preview.mirrorBytes)}.</>}
                </p>
              )}
              <ul className="checks">
                {[...preview.checks]
                  .sort((a, b) => rank(a) - rank(b))
                  .map((c, i) => (
                    <li key={i} className={c.ok ? "ok" : c.severity}>
                      <span className="mark">{c.ok ? "✓" : c.severity === "error" ? "✗" : "!"}</span>
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
            <button onClick={props.onClose}>Cancel</button>
            <button className="primary" disabled={act.busy || checking || blocked || !target} onClick={start}>
              Migrate
            </button>
          </footer>
        </>
      )}
      {taskId !== 0 && (
        <>
          <p>
            Moving <strong>{d.name}</strong> to <strong>{targetName}</strong>…
          </p>
          <div className="bar" role="progressbar" aria-valuenow={Math.round((task?.progress ?? 0) * 100)}>
            <div className={task?.status === "failed" ? "full" : ""} style={{ width: (task?.status === "ok" ? 1 : task?.progress ?? 0) * 100 + "%" }} />
          </div>
          <p className="muted">
            {task?.status === "ok" ? "Done." : task?.status === "failed" ? "Failed." : `${Math.round((task?.progress ?? 0) * 100)}%`}
          </p>
          <ErrorBox error={task?.status === "failed" ? task.error : cancel.error} />
          <footer>
            {busy ? (
              <button className="danger" disabled={cancel.busy} onClick={() => cancel.run(() => post(`${base}/migrate/cancel`))}>
                Cancel migration
              </button>
            ) : (
              <button onClick={props.onClose}>Close</button>
            )}
          </footer>
        </>
      )}
    </Modal>
  );
}

// Errors first, then warnings, then the rest.
function rank(c: Check) {
  return c.ok ? 2 : c.severity === "error" ? 0 : 1;
}
