import { useState } from "react";
import { CornerDownRight } from "lucide-react";
import { del, get, post, put, VMDetail } from "../api";
import { useLive } from "../live";
import { ErrorBox, Field, HelpTip, Modal, StateBadge, useAction, useConfirm } from "../ui";

interface Snap {
  name: string;
  description?: string;
  created: number;
  state: string;
  parent?: string;
  isCurrent: boolean;
  hasMemory: boolean;
  kind: "internal" | "external";
  children: number;
  files: string[];
}

interface Schedule {
  enabled: boolean;
  mode: "interval" | "daily";
  everyMinutes?: number;
  at?: string;
  keep: number;
  prefix: string;
  memory: boolean;
  quiesce: boolean;
}

const when = (d: Date) => d.toLocaleString();

/** Depth of each snapshot in the tree; the list arrives parents first. */
function withDepth(list: Snap[]): [Snap, number][] {
  const depth = new Map<string, number>();
  return list.map((s) => {
    const dp = s.parent ? (depth.get(s.parent) ?? 0) + 1 : 0;
    depth.set(s.name, dp);
    return [s, dp];
  });
}

export default function SnapshotsTab({ d, base }: { d: VMDetail; base: string }) {
  const snaps = useLive(
    () => get<Snap[]>(`${base}/snapshots`),
    base,
    (e) => e.type === "node" || (e.type === "domain" && e.uuid === d.uuid) || (e.type === "task" && e.task?.status !== "running"),
  );
  const [taking, setTaking] = useState(false);
  const [reverting, setReverting] = useState<Snap | undefined>();
  const [deleting, setDeleting] = useState<Snap | undefined>();
  const list = snaps.data;

  return (
    <>
      <div className="head">
        <h3>Snapshots</h3>
        <div className="actions">
          <button className="primary" onClick={() => setTaking(true)}>
            Take snapshot
          </button>
        </div>
      </div>
      <p className="muted">
        {d.state === "shutoff"
          ? "This VM is shut off, so snapshots are stored inside its disk image (internal)."
          : "Snapshots with memory are stored inside the disk image by default; disk-only snapshots of a running VM, and external ones, add an overlay file next to the disk."}
      </p>
      <ErrorBox error={snaps.error} />
      {list && list.length === 0 && <div className="empty">No snapshots yet.</div>}
      {list && list.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>Snapshot</th>
              <th>Taken</th>
              <th>VM was</th>
              <th>Type</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {withDepth(list).map(([s, depth]) => (
              <tr key={s.name}>
                <td>
                  <div style={{ paddingLeft: depth * 22 }}>
                    {depth > 0 && <CornerDownRight className="muted" size={14} aria-hidden="true" />}
                    <strong>{s.name}</strong> {s.isCurrent && <span className="badge s-running">current</span>}
                    {s.description && <div className="muted">{s.description}</div>}
                  </div>
                </td>
                <td>{when(new Date(s.created * 1000))}</td>
                <td>
                  <StateBadge state={s.state} />
                </td>
                <td title={s.files.join("\n")}>
                  {s.kind}
                  {s.hasMemory ? ", with memory" : ""}
                </td>
                <td className="num">
                  <div className="actions" style={{ justifyContent: "flex-end" }}>
                    <button onClick={() => setReverting(s)}>Revert</button>
                    <button className="danger" onClick={() => setDeleting(s)}>
                      Delete
                    </button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <SchedulePanel d={d} base={base} />
      {taking && <TakeSnapshot d={d} base={base} onClose={() => setTaking(false)} onDone={snaps.reload} />}
      {reverting && <Revert snap={reverting} base={base} onClose={() => setReverting(undefined)} onDone={snaps.reload} />}
      {deleting && <Delete snap={deleting} base={base} onClose={() => setDeleting(undefined)} onDone={snaps.reload} />}
    </>
  );
}

export function TakeSnapshot(props: { d: VMDetail; base: string; onClose: () => void; onDone: () => void }) {
  const { d } = props;
  const act = useAction();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [memory, setMemory] = useState(d.state === "running");
  const [quiesce, setQuiesce] = useState(false);
  const [mode, setMode] = useState("");
  const live = d.state !== "shutoff";
  const agentOK = d.agent === "connected";
  const submit = async () => {
    const ok = await act.run(() => post(`${props.base}/snapshots`, { name, description, memory, quiesce, mode: memory ? mode : "" }));
    if (ok) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title="Take snapshot" onClose={props.onClose}>
      <Field label="Name" hint="Optional. Letters, digits, dot, dash and underscore. Defaults to snap-<timestamp>.">
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="snap-1" autoFocus />
      </Field>
      <Field label="Description">
        <input value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>
      {live ? (
        <>
          <label className="check">
            <input
              type="checkbox"
              checked={memory}
              onChange={(e) => {
                setMemory(e.target.checked);
                if (e.target.checked) setQuiesce(false);
              }}
            />
            Include memory (reverting resumes the running guest exactly where it was)
          </label>
          {memory && (
            <div className="field-help">
              <Field label="Snapshot type">
                <select value={mode} onChange={(e) => setMode(e.target.value)}>
                  <option value="">Automatic</option>
                  <option value="internal">Internal</option>
                  <option value="external">External</option>
                </select>
              </Field>
              <HelpTip label="About snapshot types">
                <h4>Automatic</h4>
                <p>The controller chooses for you. It picks external, except on nodes that confine QEMU with AppArmor (Debian and Ubuntu), where libvirt cannot delete external snapshots, and when the VM's current snapshot is already internal. Then it picks internal.</p>
                <h4>Internal</h4>
                <p>The data lives inside the disk image (qcow2 only). The guest is paused while its memory is written, and a VM with an internal snapshot cannot be live migrated. Deleting it only edits the image.</p>
                <h4>External</h4>
                <p>The guest keeps running. The snapshot is a file next to the disk and new writes go to an overlay file, so the VM can be live migrated. Deleting it merges the overlay back into the disk, which takes a while on a big disk and needs a node where libvirt is allowed to do that.</p>
              </HelpTip>
            </div>
          )}
          <label className="check" style={{ marginTop: 8 }}>
            <input type="checkbox" checked={quiesce} disabled={!agentOK || memory} onChange={(e) => setQuiesce(e.target.checked)} />
            Freeze guest file systems first (quiesce)
          </label>
          {!agentOK && (
            <p className="muted">
              Quiesce needs the QEMU guest agent, which is {d.agent === "absent" ? "not set up for this VM" : "not responding inside the guest"}. See the Guest agent card on the
              Overview tab.
            </p>
          )}
          {agentOK && memory && <p className="muted">Quiesce applies to disk-only snapshots; untick “Include memory” to use it.</p>}
        </>
      ) : (
        <p className="muted">The VM is shut off: the snapshot captures its disk only.</p>
      )}
      <ErrorBox error={act.error} />
      <div className="actions" style={{ justifyContent: "flex-end", marginTop: 14 }}>
        <button onClick={props.onClose}>Cancel</button>
        <button className="primary" disabled={act.busy} onClick={submit}>
          {act.busy ? "Taking snapshot" : "Take snapshot"}
        </button>
      </div>
    </Modal>
  );
}

function Revert(props: { snap: Snap; base: string; onClose: () => void; onDone: () => void }) {
  const { snap } = props;
  const act = useAction();
  const [start, setStart] = useState("");
  const [force, setForce] = useState(false);
  const submit = async () => {
    const ok = await act.run(() => post(`${props.base}/snapshots/${encodeURIComponent(snap.name)}/revert`, { start, force }));
    if (ok) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title={`Revert to ${snap.name}`} onClose={props.onClose}>
      <p>
        The VM goes back to the state it had on {when(new Date(snap.created * 1000))}. Everything written to its disk since then is discarded
        {snap.hasMemory ? ", and the running guest returns to that exact moment" : ""}. Other snapshots are kept.
      </p>
      <Field label="Afterwards">
        <select value={start} onChange={(e) => setStart(e.target.value)}>
          <option value="">As it was when the snapshot was taken ({snap.state})</option>
          <option value="running">Running</option>
          <option value="paused">Paused</option>
        </select>
      </Field>
      <label className="check">
        <input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />
        Force (only if libvirt refuses because the VM configuration changed since the snapshot)
      </label>
      <ErrorBox error={act.error} />
      <div className="actions" style={{ justifyContent: "flex-end", marginTop: 14 }}>
        <button onClick={props.onClose}>Cancel</button>
        <button className="danger" disabled={act.busy} onClick={submit}>
          {act.busy ? "Reverting" : "Revert"}
        </button>
      </div>
    </Modal>
  );
}

function Delete(props: { snap: Snap; base: string; onClose: () => void; onDone: () => void }) {
  const { snap } = props;
  const act = useAction();
  const [children, setChildren] = useState(false);
  const submit = async () => {
    const ok = await act.run(() => del(`${props.base}/snapshots/${encodeURIComponent(snap.name)}${children ? "?children=1" : ""}`));
    if (ok) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title={`Delete ${snap.name}`} onClose={props.onClose}>
      <p>
        {snap.kind === "external"
          ? "The snapshot's overlay and memory files are merged into the disk chain and removed. This can take a while for large disks."
          : "The snapshot is removed from the disk image."}{" "}
        The VM's current data is not lost.
      </p>
      {snap.children > 0 && (
        <label className="check">
          <input type="checkbox" checked={children} onChange={(e) => setChildren(e.target.checked)} />
          Also delete the snapshots below it ({snap.children} direct {snap.children === 1 ? "child" : "children"}). Otherwise they are kept and attach to its parent.
        </label>
      )}
      <ErrorBox error={act.error} />
      <div className="actions" style={{ justifyContent: "flex-end", marginTop: 14 }}>
        <button onClick={props.onClose}>Cancel</button>
        <button className="danger" disabled={act.busy} onClick={submit}>
          {act.busy ? "Deleting" : "Delete"}
        </button>
      </div>
    </Modal>
  );
}

function SchedulePanel({ d, base }: { d: VMDetail; base: string }) {
  const path = `${base}/snapshot-schedule`;
  const cur = useLive(() => get<{ schedule: Schedule | null; last?: number }>(path), path, (e) => e.type === "node" || (e.type === "task" && e.task?.status !== "running"));
  const act = useAction();
  const confirm = useConfirm();
  const [form, setForm] = useState<Schedule | undefined>();
  const sch = cur.data?.schedule;
  const edit = () =>
    setForm(sch ?? { enabled: true, mode: "daily", everyMinutes: 60, at: "03:00", keep: 7, prefix: "auto", memory: false, quiesce: false });
  const save = async () => {
    if (!form) return;
    const body = { ...form, everyMinutes: form.mode === "interval" ? form.everyMinutes : undefined, at: form.mode === "daily" ? form.at : undefined };
    if (await act.run(() => put(path, body))) {
      setForm(undefined);
      cur.reload();
    }
  };
  const remove = async () => {
    const ok = await confirm.ask({ title: "Remove snapshot schedule", message: `The snapshot schedule of ${d.name} is removed. Snapshots already taken are kept.`, confirm: "Remove schedule" });
    if (ok && (await act.run(() => del(path)))) cur.reload();
  };
  const agentOK = d.agent === "connected";

  return (
    <section className="card" id="snap-schedule" style={{ marginTop: 20 }}>
      {confirm.dialog}
      <h3>Scheduled snapshots</h3>
      <ErrorBox error={cur.error} />
      {!form && (
        <>
          {sch ? (
            <p>
              <span className={"badge " + (sch.enabled ? "s-running" : "")}>{sch.enabled ? "enabled" : "paused"}</span>{" "}
              {sch.mode === "interval" ? `Every ${sch.everyMinutes} minutes` : `Daily at ${sch.at} UTC`}, keeping the newest {sch.keep} named <code>{sch.prefix}-&lt;timestamp&gt;</code>
              {sch.memory ? ", with memory" : ""}
              {sch.quiesce ? ", frozen via guest agent" : ""}.
              {cur.data?.last ? ` Last taken ${when(new Date(cur.data.last * 1000))}.` : " None taken yet."}
            </p>
          ) : (
            <p className="muted">No schedule. Snapshots are taken while the VM is running or paused; only snapshots the schedule made itself are ever pruned.</p>
          )}
          <div className="actions">
            <button onClick={edit}>{sch ? "Edit schedule" : "Set up schedule"}</button>
            {sch && (
              <button className="danger" disabled={act.busy} onClick={remove}>
                Remove
              </button>
            )}
          </div>
        </>
      )}
      {form && (
        <>
          <div className="grid2">
            <Field label="When">
              <select value={form.mode} onChange={(e) => setForm({ ...form, mode: e.target.value as Schedule["mode"] })}>
                <option value="daily">Daily at a fixed time (UTC)</option>
                <option value="interval">At a fixed interval</option>
              </select>
            </Field>
            {form.mode === "daily" ? (
              <Field label="Time (UTC)">
                <input type="time" value={form.at ?? "03:00"} onChange={(e) => setForm({ ...form, at: e.target.value })} />
              </Field>
            ) : (
              <Field label="Every (minutes)">
                <input type="number" min={1} value={form.everyMinutes ?? 60} onChange={(e) => setForm({ ...form, everyMinutes: Number(e.target.value) })} />
              </Field>
            )}
            <Field label="Keep the newest" hint="Older scheduled snapshots with this prefix are deleted after each run.">
              <input type="number" min={1} value={form.keep} onChange={(e) => setForm({ ...form, keep: Number(e.target.value) })} />
            </Field>
            <Field label="Name prefix" hint="Snapshots are named <prefix>-<timestamp>.">
              <input value={form.prefix} onChange={(e) => setForm({ ...form, prefix: e.target.value })} />
            </Field>
          </div>
          <label className="check">
            <input
              type="checkbox"
              checked={form.memory}
              onChange={(e) => setForm({ ...form, memory: e.target.checked, quiesce: e.target.checked ? false : form.quiesce })}
            />
            Include memory
          </label>
          <label className="check" style={{ margin: "8px 0" }}>
            <input type="checkbox" checked={form.quiesce} disabled={form.memory || (!agentOK && !form.quiesce)} onChange={(e) => setForm({ ...form, quiesce: e.target.checked })} />
            Freeze guest file systems (needs the guest agent{agentOK ? "" : ", which is not connected now"})
          </label>
          <label className="check" style={{ marginBottom: 8 }}>
            <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
            Enabled
          </label>
          <ErrorBox error={act.error} />
          <div className="actions">
            <button className="primary" disabled={act.busy} onClick={save}>
              Save schedule
            </button>
            <button onClick={() => setForm(undefined)}>Cancel</button>
          </div>
        </>
      )}
    </section>
  );
}
