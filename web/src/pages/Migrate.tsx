import { useEffect, useState } from "react";
import { Lock } from "lucide-react";
import { CheckList, preflightItems } from "../Checks";
import { useNavigate } from "react-router-dom";
import { bytes, get, NodeStatus, post, Task, VMDetail } from "../api";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import { SectionFrame } from "./Disclosure";

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
  const [keep, setKeep] = useState(false);
  const [preview, setPreview] = useState<Preview | undefined>();
  const [checking, setChecking] = useState(false);
  const [taskId, setTaskId] = useState(0);
  const [task, setTask] = useState<Task | undefined>();
  const act = useAction();
  const cancel = useAction();
  const confirm = useConfirm();
  const base = `/nodes/${d.node}/vms/${d.uuid}`;

  useEffect(() => {
    get<{ nodes: NodeStatus[] }>("/nodes").then((r) => {
      const others = r.nodes.filter((n) => n.id !== d.node);
      setNodes(others);
      const first = others.find((n) => n.state === "online");
      if (first) setTarget(first.id);
    }, () => {});
  }, [d.node]);

  // Pre-flight whenever the target changes. A running VM is migrated live, a shut-off one offline; the
  // server decides that and encrypts when both nodes are set up for it.
  useEffect(() => {
    if (!target || taskId) return;
    let stale = false;
    setChecking(true);
    setPreview(undefined);
    post<Preview>(`${base}/migrate/check`, { target }).then(
      (p) => !stale && setPreview(p),
      (e) => !stale && setPreview({ ok: false, checks: [{ check: "Pre-flight", ok: false, severity: "error", message: String(e.message ?? e) }], copyBytes: 0, mirrorBytes: 0 }),
    ).finally(() => !stale && setChecking(false));
    return () => {
      stale = true;
    };
  }, [target, base, taskId]);

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
      const r = await post<{ task: number }>(`${base}/migrate`, { target, keepSourceDisks: keep });
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
              <Select
                label="Target node"
                value={String(target)}
                onChange={(v) => setTarget(Number(v))}
                placeholder="no other node"
                options={nodes.map((n) => ({ value: String(n.id), label: (n.hostname || n.addr) + (n.state !== "online" ? ` (${n.state})` : ""), disabled: n.state !== "online", dot: "n-" + n.state }))}
              />
            </Field>
          </div>
          <SectionFrame id="migrate-advanced" title="Advanced" summary="">
            <label className="check">
              <input type="checkbox" checked={keep} onChange={(e) => setKeep(e.target.checked)} />
              Keep the disks on the source node
            </label>
          </SectionFrame>
          <h3>Pre-flight checks</h3>
          {checking && <p className="muted">Checking {targetName}</p>}
          {preview && (
            <>
              {preview.mode && (
                <p>
                  <strong>{modeText[preview.mode]}.</strong>{" "}
                  {preview.mode !== "offline" && (
                    <span className={`badge ${preview.encrypt ? "s-ok" : "s-paused"}`} title={preview.encrypt ? "Memory and disk data are TLS-encrypted on the wire" : "Memory and disk data cross the network unencrypted"}>
                      {preview.encrypt ? <><Lock size={12} aria-hidden="true" /> encrypted</> : "unencrypted"}
                    </span>
                  )}{" "}
                  {preview.copyBytes > 0 && <>Files to copy: {bytes(preview.copyBytes)}. </>}
                  {preview.mirrorBytes > 0 && <>Disk data mirrored while running: up to {bytes(preview.mirrorBytes)}.</>}
                </p>
              )}
              <CheckList items={preflightItems(preview.checks)} />
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
            Moving <strong>{d.name}</strong> to <strong>{targetName}</strong>
          </p>
          <div className="bar" role="progressbar" aria-valuenow={Math.round((task?.progress ?? 0) * 100)}>
            <div className={task?.status === "failed" ? "full" : ""} style={{ width: (task?.status === "ok" ? 1 : task?.progress ?? 0) * 100 + "%" }} />
          </div>
          <p className="muted">
            {task?.status === "ok" ? "Done." : task?.status === "failed" ? "Failed." : `${Math.round((task?.progress ?? 0) * 100)}%`}
          </p>
          {confirm.dialog}
          <ErrorBox error={task?.status === "failed" ? task.error : cancel.error} />
          <footer>
            {busy ? (
              <button className="danger" disabled={cancel.busy} onClick={async () => (await confirm.ask({ title: "Stop migration", message: `The running migration of ${d.name} to ${targetName} is stopped. The VM stays on its current node.`, confirm: "Stop migration" })) && cancel.run(() => post(`${base}/migrate/cancel`))}>
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

