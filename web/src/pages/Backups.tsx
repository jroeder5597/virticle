import { useEffect, useState } from "react";
import { ApiError, bytes, del, get, NodeStatus, Pool, post, Task, VMDetail } from "../api";
import { useRole } from "../auth";
import { useLive } from "../live";
import { ErrorBox, Field, Modal, NodePicker, Progress, UsageBar, useAction } from "../ui";

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
}

const when = (iso: string) => new Date(iso).toLocaleString();
const url = (b: BackupEntry) => `/nodes/${b.node}/backups/${encodeURIComponent(b.pool)}/${encodeURIComponent(b.dir)}`;

const notIncluded = "A backup holds the VM's current state: snapshots are not included, and neither is TPM state (a restored VM starts with an empty TPM).";

/** Follows a task started in the background, showing progress and the result. */
function TaskFollow(props: { id: number; onDone: (ok: boolean) => void; onCancel?: () => void }) {
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
        {t ? t.kind : "…"} {t?.target}: <strong>{t?.status ?? "starting"}</strong>
      </p>
      <Progress value={t?.status === "ok" ? 1 : (t?.progress ?? 0)} failed={t?.status === "failed"} />
      {t?.status === "failed" && <ErrorBox error={t.error} />}
      {t?.status === "running" && props.onCancel && (
        <button className="danger" onClick={props.onCancel}>
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
  const cur = pools.find((p) => p.name === props.pool);
  return (
    <>
      <Field label="Target node">
        <NodePicker nodes={nodes} value={props.node} onChange={props.setNode} />
      </Field>
      <Field label="Target pool" hint="Only running directory-backed pools can hold backups.">
        <select value={props.pool} onChange={(e) => props.setPool(e.target.value)}>
          {pools.map((p) => (
            <option key={p.name}>{p.name}</option>
          ))}
        </select>
        {cur && <UsageBar used={cur.allocation} total={cur.capacity} />}
        {cur && <small>{bytes(cur.available)} free</small>}
      </Field>
    </>
  );
}

function BackupNow(props: { d: VMDetail; onClose: () => void; onDone: () => void }) {
  const { d } = props;
  const act = useAction();
  const [node, setNode] = useState(d.node);
  const [pool, setPool] = useState("");
  const [keep, setKeep] = useState(0);
  const [sha, setSha] = useState(false);
  const [quiesce, setQuiesce] = useState(false);
  const [task, setTask] = useState(0);
  const base = `/nodes/${d.node}/vms/${d.uuid}`;
  const live = d.state !== "shutoff";
  const submit = async () => {
    await act.run(async () => {
      const r = await post<{ task: number }>(`${base}/backup`, { targetNode: node === d.node ? 0 : node, pool, keepLast: keep, sha256: sha, quiesce });
      setTask(r.task);
    });
  };
  return (
    <Modal title={`Back up ${d.name}`} onClose={props.onClose}>
      {task ? (
        <TaskFollow id={task} onDone={props.onDone} onCancel={() => post(`${base}/backup/cancel`)} />
      ) : (
        <>
          <p className="muted">
            {live ? "The VM keeps running; the disks are copied consistently as of the start of the backup (crash-consistent)." : "The VM is shut off; its disks are copied and flattened."} {notIncluded}
          </p>
          <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
          <Field label="Keep last" hint="After a successful backup, delete this VM's older backups in this pool beyond the newest N. 0 keeps everything.">
            <input type="number" min={0} max={1000} value={keep} onChange={(e) => setKeep(Number(e.target.value))} />
          </Field>
          <label>
            <input type="checkbox" checked={sha} onChange={(e) => setSha(e.target.checked)} /> Record SHA-256 checksums (checked on restore)
          </label>
          {live && d.agent === "connected" && (
            <label>
              <input type="checkbox" checked={quiesce} onChange={(e) => setQuiesce(e.target.checked)} /> Freeze guest file systems through the guest agent
            </label>
          )}
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
  const [mode, setMode] = useState<"new" | "original">(b.vmExists ? "new" : "original");
  const [name, setName] = useState(m.vm + "-restored");
  const [node, setNode] = useState(b.node);
  const [pool, setPool] = useState(b.pool);
  const [replace, setReplace] = useState(false);
  const [start, setStart] = useState(false);
  const [task, setTask] = useState(0);
  const [warnings, setWarnings] = useState<string[]>([]);
  const submit = () =>
    act.run(async () => {
      const r = await post<{ task: number; warnings: string[] }>(`${url(b)}/restore`, { mode, name, targetNode: node, pool, replace, start });
      setWarnings(r.warnings ?? []);
      setTask(r.task);
    });
  return (
    <Modal title={`Restore ${m.vm}`} onClose={props.onClose}>
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
          <Field label="Restore as">
            <select value={mode} onChange={(e) => setMode(e.target.value as "new" | "original")}>
              <option value="new">A new VM (new name, UUID and MAC addresses)</option>
              <option value="original">The original (same name and UUID)</option>
            </select>
          </Field>
          {mode === "new" ? (
            <Field label="New VM name">
              <input value={name} onChange={(e) => setName(e.target.value)} />
            </Field>
          ) : (
            <>
              <p className="muted">Refused while a VM with this name or UUID exists on any node.</p>
              <label>
                <input type="checkbox" checked={replace} onChange={(e) => setReplace(e.target.checked)} /> Replace the existing VM on the target node (it must be shut off; its disks are deleted)
              </label>
            </>
          )}
          <Target node={node} pool={pool} setNode={setNode} setPool={setPool} />
          <label>
            <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Start the VM afterwards
          </label>
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
    if (await act.run(() => del(url(b)))) {
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
                {b.error && <div className="rowerror">{b.error}</div>}
              </td>
              <td title={b.path}>
                {b.nodeName}: {b.pool}
              </td>
              <td>{b.manifest?.disks.map((x) => `${x.target} (${bytes(x.virtualSize)})`).join(", ")}</td>
              <td className="num">{bytes(b.size)}</td>
              <td className="num">
                <div className="actions" style={{ justifyContent: "flex-end" }}>
                  {b.manifest && <button onClick={() => setRestoring(b)}>Restore</button>}
                  {b.manifest && role !== "viewer" && (
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
      {!list.data && !list.error && <div className="empty">Looking for backups on all nodes…</div>}
      {list.data && list.data.length === 0 && <div className="empty">No backups of this VM yet.</div>}
      {list.data && list.data.length > 0 && <BackupTable list={list.data} reload={list.reload} />}
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
      {!list.data && !list.error && <div className="empty">Looking for backups on all nodes…</div>}
      {list.data && list.data.length === 0 && <div className="empty">No backups yet. Use the Backups tab of a VM to make one.</div>}
      {list.data && list.data.length > 0 && <BackupTable list={list.data} global reload={list.reload} />}
      {importing && <ImportFile onClose={() => setImporting(false)} onDone={list.reload} />}
    </section>
  );
}
