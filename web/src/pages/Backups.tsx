import { describeWhen, NextRunLine, ScheduleWhen, When, whenBody, whenFrom } from "../ScheduleTime";
import DialogAdvanced from "./DialogAdvanced";
import { useEffect, useState } from "react";
import { ApiError, bytes, del, get, NodeStatus, Pool, post, put, Task, VMDetail } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import PoolSelect from "../PoolSelect";
import Select from "../Select";
import { ErrorBox, Field, Modal, NodePicker, Progress, useAction, useConfirm } from "../ui";
import { StatusLine } from "../Checks";

interface Manifest {
  vm: string;
  uuid: string;
  created: string;
  sourceNode: string;
  live: boolean;
  tpm: boolean;
  disks: { target: string; file: string; virtualSize: number; bytes: number; sha256?: string }[];
  nvram?: string;
  notes: string[];
  /** "full" or "incremental"; backups from before incremental backups have none and are full. */
  kind?: "full" | "incremental";
  /** Why a backup is full when an incremental one might have been expected. */
  fullReason?: string;
}
export interface BackupEntry {
  node: number;
  nodeName: string;
  pool: string;
  dir: string;
  path: string;
  size: number;
  manifest?: Manifest;
  error?: string;
  vmExists: boolean;
  /** Where the backed-up VM is now, and whether it is shut off (for "Replace"). */
  vmNode?: number;
  vmNodeName?: string;
  vmShutOff?: boolean;
  /** How many backups a restore reads (1 for a full backup), and why it cannot be restored when a link is missing. */
  chain?: number;
  chainError?: string;
  /** Later backups that build on this one and go with it when it is deleted. */
  dependents?: number;
}

const isIncremental = (b: BackupEntry) => b.manifest?.kind === "incremental";

const when = (iso: string) => new Date(iso).toLocaleString();
const url = (b: BackupEntry) => `/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}`;

const hasLaterOnes = (n?: number) => (n ?? 0) > 0;

const notIncluded = "A backup holds the VM's current state: snapshots are not included, and neither is TPM state (a restored VM starts with an empty TPM).";

/** Follows a task started in the background, showing progress and the result. */
function TaskFollow(props: { id: number; onDone: (ok: boolean) => void; onCancel?: () => void }) {
  const confirm = useConfirm();
  const [t, setT] = useState<Task | undefined>();
  useEffect(() => {
    let finished = false;
    const poll = () =>
      get<Task[]>("/tasks").then((ts) => {
        const x = ts.find((y) => y.id === props.id);
        if (x) {
          setT(x);
          if (x.status !== "running" && !finished) {
            finished = true;
            props.onDone(x.status === "ok");
          }
        }
      }, () => {});
    poll();
    const h = setInterval(poll, 1000);
    return () => clearInterval(h);
  }, [props.id]);
  return (
    <div>
      <p>
        {t ? t.kind : "task"} {t?.target}: <strong>{t?.status ?? "starting"}</strong>
      </p>
      <Progress value={t?.status === "ok" ? 1 : (t?.progress ?? 0)} failed={t?.status === "failed"} />
      {confirm.dialog}
      {t?.status === "failed" && <ErrorBox error={t.error} />}
      {t?.status === "running" && props.onCancel && (
        <button
          className="danger"
          onClick={async () => (await confirm.ask({ title: "Stop backup", message: "The running backup is stopped before it finishes. No usable backup of this run is kept.", confirm: "Stop backup" })) && props.onCancel?.()}
        >
          Cancel
        </button>
      )}
    </div>
  );
}

function usePools(node: number) {
  const [pools, setPools] = useState<Pool[]>([]);
  useEffect(() => {
    if (!node) return;
    get<Pool[]>(`/nodes/${node}/pools`).then(
      (p) => setPools(p.filter((x) => x.state === "running" && ["dir", "fs", "netfs"].includes(x.type))),
      () => setPools([]),
    );
  }, [node]);
  return pools;
}

function useNodes() {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  return (nodes.data?.nodes ?? []).filter((n) => n.state === "online");
}

/** Where to put something: a node and a directory-backed pool on it. */
function Target(props: { node: number; pool: string; setNode: (n: number) => void; setPool: (p: string) => void }) {
  const nodes = useNodes();
  const pools = usePools(props.node);
  useEffect(() => {
    if (pools.length && !pools.some((p) => p.name === props.pool)) props.setPool(pools[0].name);
  }, [pools]);
  return (
    <>
      <Field label="Target node">
        <NodePicker label="Target node" nodes={nodes} value={props.node} onChange={props.setNode} />
      </Field>
      <Field label="Target pool" hint="Only running directory-backed pools can hold backups.">
        <PoolSelect label="Target pool" pools={pools} value={props.pool} onChange={props.setPool} />
      </Field>
    </>
  );
}

function BackupNow(props: { d: VMDetail; onClose: () => void; onDone: () => void }) {
  const { d } = props;
  const act = useAction();
  const [node, setNode] = useState(d.node);
  const [pool, setPool] = useState("");
  const [keep, setKeep] = useState(7);
  const [task, setTask] = useState(0);
  const [kind, setKind] = useState("");
  const [why, setWhy] = useState("");
  const [finished, setFinished] = useState(false);
  const base = `/nodes/${d.node}/vms/${d.uuid}`;
  // The kind in the start answer is the plan: libvirt may refuse an incremental backup and the run then makes a
  // full one. Once the run is over, the manifest of the new backup says what it really is.
  const done = async (ok: boolean) => {
    if (ok) {
      try {
        const l = await get<BackupEntry[]>(`/backups?uuid=${d.uuid}`);
        const newest = l.filter((b) => b.manifest).sort((x, y) => Date.parse(y.manifest!.created) - Date.parse(x.manifest!.created))[0];
        if (newest?.manifest) {
          setKind(newest.manifest.kind ?? "full");
          setWhy(newest.manifest.fullReason ?? "");
          setFinished(true);
        }
      } catch {
        // keep the planned kind
      }
    }
    props.onDone();
  };
  const submit = async () => {
    await act.run(async () => {
      const r = await post<{ task: number; plannedKind: string }>(`${base}/backup`, { targetNode: node === d.node ? 0 : node, pool, keepLast: keep });
      setKind(r.plannedKind);
      setTask(r.task);
    });
  };
  return (
    <Modal title={`Back up ${d.name}`} onClose={props.onClose}>
      {task ? (
        <>
          <p className="muted">
            {finished
              ? kind === "incremental"
                ? "Incremental backup: only what changed since the last backup was copied."
                : `Full backup: the whole disks were copied.${why ? " " + why : ""}`
              : kind === "incremental"
                ? "Planned as an incremental backup: only what changed since the last backup is copied. If that is not possible, a full backup is made instead."
                : "Full backup: the whole disks are copied."}
          </p>
          <TaskFollow id={task} onDone={done} onCancel={() => post(`${base}/backup/cancel`)} />
        </>
      ) : (
        <>
          <p className="muted">Copies what changed since the last backup when that is possible, otherwise the whole disks. Snapshots and TPM state are not included.</p>
          <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
          <Field label="Keep the newest" hint="Older backups of this VM in this pool are deleted, except those a kept incremental backup builds on. 0 keeps all.">
            <input type="number" min={0} max={1000} value={keep} onChange={(e) => setKeep(Number(e.target.value))} />
          </Field>
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy || !pool} onClick={submit}>
              Back up now
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}

function Restore(props: { b: BackupEntry; onClose: () => void; onDone: () => void }) {
  const { b } = props;
  const m = b.manifest!;
  const act = useAction();
  const confirm = useConfirm();
  const [replace, setReplace] = useState(false);
  const [name, setName] = useState(m.vm + "-restored");
  const [node, setNode] = useState(b.node);
  const [pool, setPool] = useState(b.pool);
  const [start, setStart] = useState(true);
  const [task, setTask] = useState(0);
  const [warnings, setWarnings] = useState<string[]>([]);
  // Replacing needs the original VM on the target node and shut off.
  const whyNot = !b.vmExists
    ? `Replace is not possible: ${m.vm} no longer exists.`
    : b.vmNode !== node
      ? `Replace is not possible: ${m.vm} is on ${b.vmNodeName ?? "another node"}; choose that node under Advanced.`
      : !b.vmShutOff
        ? `Replace is not possible while ${m.vm} is running; shut it down first.`
        : "";
  const doReplace = replace && !whyNot;
  const submit = async () => {
    if (doReplace) {
      const ok = await confirm.ask({
        title: `Replace ${m.vm}`,
        message: `The existing VM ${m.vm} on the target node is deleted together with its disks, and replaced by the backup from ${when(m.created)}.`,
        confirm: "Replace VM",
      });
      if (!ok) return;
    }
    return act.run(async () => {
      const r = await post<{ task: number; warnings: string[] }>(`${url(b)}/restore`, { mode: doReplace ? "original" : "new", name, targetNode: node, pool, replace: doReplace, start });
      setWarnings(r.warnings ?? []);
      setTask(r.task);
    });
  };
  return (
    <Modal title={`Restore ${m.vm}`} onClose={props.onClose}>
      {confirm.dialog}
      {task ? (
        <>
          <TaskFollow id={task} onDone={props.onDone} />
          {warnings.map((w) => (
            <p key={w} className="muted">
              {w}
            </p>
          ))}
        </>
      ) : (
        <>
          <p className="muted">
            Backup of {m.vm} from {when(m.created)}. {notIncluded}
          </p>
          <Field label="Restore" hint={whyNot || undefined}>
            <Select
              label="Restore"
              value={doReplace ? "replace" : "new"}
              onChange={(v) => setReplace(v === "replace")}
              options={[
                { value: "new", label: "As a new VM" },
                { value: "replace", label: `Replace ${m.vm}`, disabled: !!whyNot },
              ]}
            />
          </Field>
          {!doReplace && (
            <Field label="New VM name">
              <input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          )}
          <label className="check">
            <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Start the VM afterwards
          </label>
          <DialogAdvanced>
            <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
          </DialogAdvanced>
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy || !pool} onClick={submit}>
              Restore
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}

function BackupTable(props: { list: BackupEntry[]; global?: boolean; reload: () => void }) {
  const role = useRole();
  const [restoring, setRestoring] = useState<BackupEntry | undefined>();
  const [deleting, setDeleting] = useState<BackupEntry | undefined>();
  const act = useAction();
  const remove = async (b: BackupEntry) => {
    if (await act.run(() => del(url(b) + (hasLaterOnes(b.dependents) ? "?withLater=1" : "")))) {
      setDeleting(undefined);
      props.reload();
    }
  };
  return (
    <>
      <ErrorBox error={act.error} />
      <table>
        <thead>
          <tr>
            {props.global && <th>VM</th>}
            <th>Taken</th>
            <th>Type</th>
            <th>Where</th>
            <th>Disks</th>
            <th className="num">Size</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {props.list.map((b) => (
            <tr key={b.node + b.path}>
              {props.global && (
                <td>
                  <strong>{b.manifest?.vm ?? b.dir}</strong> {b.manifest && !b.vmExists && <span className="badge s-shutoff">VM gone</span>}
                </td>
              )}
              <td>
                {b.manifest ? when(b.manifest.created) : "?"}
                {b.manifest && <div className="muted">{b.manifest.live ? "live" : "shut off"}{b.manifest.nvram ? ", UEFI variables" : ""}</div>}
                {b.error && <StatusLine status="error">{b.error}</StatusLine>}
              </td>
              <td>
                {b.manifest && <strong>{isIncremental(b) ? "Incremental" : "Full"}</strong>}
                {b.manifest && isIncremental(b) && <div className="muted">changes only; needs {b.chain ? b.chain - 1 : "?"} earlier {b.chain === 2 ? "backup" : "backups"}</div>}
                {b.manifest && !isIncremental(b) && b.manifest.fullReason && <div className="muted">{b.manifest.fullReason}</div>}
                {b.chainError && <StatusLine status="error">Cannot be restored: {b.chainError}</StatusLine>}
              </td>
              <td title={b.path}>
                {b.nodeName}: {b.pool}
              </td>
              <td>{b.manifest?.disks.map((x) => `${x.target} (${bytes(x.virtualSize)})`).join(", ")}</td>
              <td className="num">{bytes(b.size)}</td>
              <td className="num">
                <div className="actions" style={{ justifyContent: "flex-end" }}>
                  {b.manifest && (
                    <button disabled={!!b.chainError} onClick={() => setRestoring(b)}>
                      Restore
                    </button>
                  )}
                  {b.manifest && role !== "viewer" && !b.chainError && (
                    <a className="button" href={"/api" + url(b) + "/export"} download title="Download as one tar file">
                      Export
                    </a>
                  )}
                  <button className="danger" onClick={() => setDeleting(b)}>
                    Delete
                  </button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {restoring && <Restore b={restoring} onClose={() => setRestoring(undefined)} onDone={props.reload} />}
      {deleting && (
        <Modal title="Delete backup" onClose={() => setDeleting(undefined)}>
          <p>
            Delete the backup <strong>{deleting.dir}</strong> on {deleting.nodeName}? This cannot be undone.
          </p>
          {hasLaterOnes(deleting.dependents) && (
            <p>
              {deleting.dependents} later incremental {deleting.dependents === 1 ? "backup builds" : "backups build"} on this one and {deleting.dependents === 1 ? "is" : "are"} deleted with it.
            </p>
          )}
          <div className="actions">
            <button className="danger" disabled={act.busy} onClick={() => remove(deleting)}>
              Delete
            </button>
          </div>
        </Modal>
      )}
    </>
  );
}

interface BackupSched {
  enabled: boolean;
  mode: "interval" | "daily";
  everyMinutes?: number;
  at?: string;
  targetNode: number;
  pool: string;
  keepLast: number;
}

interface SchedForm {
  when: When;
  targetNode: number;
  pool: string;
  keepLast: number;
}

/** The schedule form and summary; the schedule lives in the VM's metadata. */
function SchedulePanel({ d }: { d: VMDetail }) {
  const path = `/nodes/${d.node}/vms/${d.uuid}/backup-schedule`;
  const role = useRole();
  const cur = useLive(() => get<{ schedule: BackupSched | null; last?: number; lastError?: string; nextRun?: number; runner?: "this" | "disabled" | "other"; tick?: number }>(path), path, (e) => e.type === "node" || (e.type === "task" && e.task?.status !== "running"));
  const nodes = useNodes();
  const act = useAction();
  const confirm = useConfirm();
  const [form, setForm] = useState<SchedForm | undefined>();
  const sch = cur.data?.schedule;
  const nodeName = (id: number) => {
    const n = nodes.find((x) => x.id === (id || d.node));
    return n?.hostname || n?.addr || `node ${id || d.node}`;
  };
  const edit = () => setForm({ when: whenFrom(sch ?? undefined), targetNode: sch?.targetNode || d.node, pool: sch?.pool ?? "", keepLast: sch?.keepLast ?? 7 });
  const save = async () => {
    if (!form) return;
    // A saved schedule is enabled.
    const body = { ...whenBody(form.when), targetNode: form.targetNode === d.node ? 0 : form.targetNode, pool: form.pool, keepLast: form.keepLast };
    if (await act.run(() => put(path, body))) {
      setForm(undefined);
      cur.reload();
    }
  };
  const remove = async () => {
    const ok = await confirm.ask({ title: "Remove backup schedule", message: `The backup schedule of ${d.name} is removed. Backups already made are kept.`, confirm: "Remove schedule" });
    if (ok && (await act.run(() => del(path)))) cur.reload();
  };
  const canEdit = role !== "viewer";
  return (
    <section className="card" style={{ marginTop: 20 }} id="backup-schedule">
      {confirm.dialog}
      <h3>Scheduled backups</h3>
      <ErrorBox error={cur.error} />
      {!form && (
        <>
          {sch ? (
            <p>
              {!sch.enabled && <><span className="badge">Paused</span>{" "}</>}
              {describeWhen(sch)} to {sch.pool} on {nodeName(sch.targetNode)}, keeping the newest {sch.keepLast}.
              {cur.data?.last ? ` Last backup ${when(new Date(cur.data.last * 1000).toISOString())}.` : " No backup there yet."}
            </p>
          ) : (
            <p className="muted">No schedule. A failed run raises an alert.</p>
          )}
          {sch && cur.data?.lastError && <StatusLine status="error">Cannot read the target: {cur.data.lastError}</StatusLine>}
          {sch && <NextRunLine nextRun={cur.data?.nextRun} runner={cur.data?.runner} enabled={sch.enabled} tick={cur.data?.tick} onElapsed={cur.reload} />}
          {canEdit && (
            <div className="actions">
              <button onClick={edit}>{sch ? "Edit schedule" : "Set up schedule"}</button>
              {sch && (
                <button className="danger" disabled={act.busy} onClick={remove}>
                  Remove
                </button>
              )}
            </div>
          )}
        </>
      )}
      {form && (
        <>
          <div className="grid2">
            <ScheduleWhen value={form.when} onChange={(when) => setForm({ ...form, when })} />
            <Field label="Keep the newest" hint="Older backups of this VM in the target pool are deleted.">
              <input type="number" min={1} max={1000} value={form.keepLast} onChange={(e) => setForm({ ...form, keepLast: Number(e.target.value) })} />
            </Field>
          </div>
          <Target node={form.targetNode} pool={form.pool} setNode={(n) => setForm((f) => f && { ...f, targetNode: n })} setPool={(p) => setForm((f) => f && { ...f, pool: p })} />
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy || !form.pool} onClick={save}>
              Save schedule
            </button>
            <button onClick={() => setForm(undefined)}>Cancel</button>
          </div>
        </>
      )}
    </section>
  );
}

const taskDone = (e: { type: string; task?: Task }) => e.type === "task" && e.task?.status !== "running";

/** The Backups tab of a VM. */
export default function BackupsTab({ d }: { d: VMDetail; base: string }) {
  const list = useLive(() => get<BackupEntry[]>(`/backups?uuid=${d.uuid}`), "backups/" + d.uuid, taskDone);
  const [making, setMaking] = useState(false);
  return (
    <>
      <div className="head">
        <h3>Backups</h3>
        <div className="actions">
          <button className="primary" onClick={() => setMaking(true)}>
            Back up now
          </button>
        </div>
      </div>
      <p className="muted">Backups of this VM on all nodes. {notIncluded}</p>
      <ErrorBox error={list.error} />
      {!list.data && !list.error && <div className="empty">Looking for backups on all nodes</div>}
      {list.data && list.data.length === 0 && <div className="empty">No backups of this VM yet.</div>}
      {list.data && list.data.length > 0 && <BackupTable list={list.data} reload={list.reload} />}
      <SchedulePanel d={d} />
      {making && <BackupNow d={d} onClose={() => setMaking(false)} onDone={list.reload} />}
    </>
  );
}

function uploadTar(node: number, pool: string, file: File, onProgress: (p: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/nodes/${node}/backups/import?pool=${encodeURIComponent(pool)}`);
    xhr.upload.onprogress = (e) => e.lengthComputable && onProgress(e.loaded / e.total);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = `${xhr.status} ${xhr.statusText}`;
      try {
        msg = JSON.parse(xhr.responseText).error ?? msg;
      } catch {
        /* keep the status text */
      }
      reject(new ApiError(xhr.status, msg));
    };
    xhr.onerror = () => reject(new Error("upload failed: connection lost"));
    xhr.send(file);
  });
}

function ImportFile(props: { onClose: () => void; onDone: () => void }) {
  const nodes = useNodes();
  const [node, setNode] = useState(0);
  const [pool, setPool] = useState("");
  const [file, setFile] = useState<File | undefined>();
  const [progress, setProgress] = useState(0);
  const act = useAction();
  useEffect(() => {
    if (!node && nodes.length) setNode(nodes[0].id);
  }, [nodes]);
  const go = async () => {
    if (file && (await act.run(() => uploadTar(node, pool, file, setProgress)))) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title="Import a VM export" onClose={props.onClose}>
      <p className="muted">
        Upload a <code>.vmexport.tar</code> file. It is checked while it streams to the node and stored as a backup in the chosen pool; restore it from the list afterwards.
      </p>
      <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
      <Field label="File">
        <input type="file" accept=".tar" onChange={(e) => setFile(e.target.files?.[0])} />
      </Field>
      {act.busy && <Progress value={progress} />}
      <ErrorBox error={act.error} />
      <div className="actions">
        <button className="primary" disabled={act.busy || !file || !pool} onClick={go}>
          Upload and import
        </button>
      </div>
    </Modal>
  );
}

/** All backups on all nodes, including those of VMs that no longer exist. */
export function BackupsSection() {
  const list = useLive(() => get<BackupEntry[]>("/backups"), "backups", taskDone);
  const [importing, setImporting] = useState(false);
  return (
    <section>
      <div className="head">
        <h2>Backups</h2>
        <div className="actions">
          <button onClick={() => setImporting(true)}>Import from file</button>
        </div>
      </div>
      <p className="muted">Every backup found in the directory pools of all nodes. {notIncluded}</p>
      <ErrorBox error={list.error} />
      {!list.data && !list.error && <div className="empty">Looking for backups on all nodes</div>}
      {list.data && list.data.length === 0 && <div className="empty">No backups yet. Use the Backups tab of a VM to make one.</div>}
      {list.data && list.data.length > 0 && <BackupTable list={list.data} global reload={list.reload} />}
      {importing && <ImportFile onClose={() => setImporting(false)} onDone={list.reload} />}
    </section>
  );
}
