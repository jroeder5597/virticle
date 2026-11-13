import { useEffect, useState } from "react";
import { del, get, post, put, VMDetail } from "../api";
import { useLive } from "../live";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import { buildSnapRows } from "../snapTree";
import SnapTree from "./SnapTree";
import { describeWhen, NextRunLine, ScheduleWhen, When, whenBody, whenFrom } from "../ScheduleTime";
import { CheckList, StatusLine } from "../Checks";

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
  scheduled: boolean;
  deletable: boolean;
  reason?: string;
}

interface Choice {
  allowed: boolean;
  reason?: string;
}

/** What a new snapshot of this VM can be right now; the server decides (snapkind.go). */
interface SnapOptions {
  withState: Choice;
  diskOnly: Choice;
  reason?: string;
  mixed?: string;
}

function useSnapOptions(d: VMDetail, base: string) {
  return useLive(
    () => get<SnapOptions>(`${base}/snapshot-options`),
    `${base}/snapshot-options/${d.state}`,
    (e) => e.type === "node" || (e.type === "domain" && e.uuid === d.uuid) || (e.type === "task" && e.task?.status !== "running"),
  ).data;
}

interface Schedule {
  enabled: boolean;
  mode: "interval" | "daily";
  everyMinutes?: number;
  at?: string;
  keep: number;
  prefix: string;
  memory: boolean;
}

const when = (d: Date) => d.toLocaleString();

export default function SnapshotsTab({ d, base }: { d: VMDetail; base: string }) {
  const snaps = useLive(
    () => get<Snap[]>(`${base}/snapshots`),
    base,
    (e) => e.type === "node" || (e.type === "domain" && e.uuid === d.uuid) || (e.type === "task" && e.task?.status !== "running"),
  );
  const [taking, setTaking] = useState(false);
  const [reverting, setReverting] = useState<Snap | undefined>();
  const [deleting, setDeleting] = useState<{ initial?: string }>();
  const list = snaps.data;
  const opts = useSnapOptions(d, base);
  const blocked = opts && !opts.withState.allowed && !opts.diskOnly.allowed ? opts.reason : undefined;

  return (
    <>
      <div className="head">
        <h3>Snapshots</h3>
        <div className="actions">
          <button disabled={!list || list.length === 0} onClick={() => setDeleting({})}>
            Delete snapshots
          </button>
          <button className="primary" disabled={!!blocked} onClick={() => setTaking(true)}>
            Take snapshot
          </button>
        </div>
      </div>
      {blocked && <p className="muted">{blocked}</p>}
      {opts?.mixed && <StatusLine status="warning">{opts.mixed}</StatusLine>}
      <ErrorBox error={snaps.error} />
      {list && list.length === 0 && <div className="empty">No snapshots yet.</div>}
      {list && list.length > 0 && (
        <SnapTree list={list} vmState={d.state} mode="view" ariaLabel="Snapshots" onRevert={setReverting} onDelete={(x) => setDeleting({ initial: x.name })} />
      )}
      <SchedulePanel d={d} base={base} />
      {taking && <TakeSnapshot d={d} base={base} onClose={() => setTaking(false)} onDone={snaps.reload} />}
      {reverting && <Revert snap={reverting} base={base} onClose={() => setReverting(undefined)} onDone={snaps.reload} />}
      {deleting && list && <DeleteSnapshots list={list} initial={deleting.initial} base={base} vmState={d.state} onClose={() => setDeleting(undefined)} onDone={snaps.reload} />}
    </>
  );
}

export function TakeSnapshot(props: { d: VMDetail; base: string; onClose: () => void; onDone: () => void }) {
  const { d } = props;
  const act = useAction();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [memory, setMemory] = useState(d.state === "running");
  const live = d.state !== "shutoff";
  const opts = useSnapOptions(d, props.base);
  // When the VM's earlier snapshots allow only one state of the checkbox, it shows that state and is disabled.
  const forced = live && opts && opts.withState.allowed !== opts.diskOnly.allowed;
  const saveState = forced ? opts.withState.allowed : memory;
  const cannot = opts && !opts.withState.allowed && !opts.diskOnly.allowed ? opts.reason : !live && opts && !opts.diskOnly.allowed ? opts.diskOnly.reason : undefined;
  // The server freezes the guest file systems when the guest agent is connected and memory is not saved.
  const submit = async () => {
    const ok = await act.run(() => post(`${props.base}/snapshots?async=1`, { name, description, memory: saveState }));
    if (ok) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title="Take snapshot" onClose={props.onClose}>
      <Field label="Name" hint="Optional; default snap-<timestamp>.">
        <input value={name} onChange={(e) => setName(e.target.value)} placeholder="snap-1" autoFocus />
      </Field>
      <Field label="Description">
        <input value={description} onChange={(e) => setDescription(e.target.value)} />
      </Field>
      {live ? (
        <>
          <label className="check">
            <input type="checkbox" checked={saveState} disabled={!!forced || !!cannot} onChange={(e) => setMemory(e.target.checked)} />
            Save the running state too
          </label>
          {forced && <p className="muted">{saveState ? opts.diskOnly.reason : opts.withState.reason}</p>}
        </>
      ) : (
        <p className="muted">Shut off: disk only.</p>
      )}
      {cannot && <p className="muted">{cannot}</p>}
      <ErrorBox error={act.error} />
      <div className="actions" style={{ justifyContent: "flex-end", marginTop: 14 }}>
        <button onClick={props.onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || !!cannot} onClick={submit}>
          {act.busy ? "Taking snapshot" : "Take snapshot"}
        </button>
      </div>
    </Modal>
  );
}

function Revert(props: { snap: Snap; base: string; onClose: () => void; onDone: () => void }) {
  const { snap } = props;
  const act = useAction();
  // The first attempt is made without force. If libvirt refuses because the VM's configuration changed
  // since the snapshot, the error is shown with a "Revert anyway" button.
  const [forceable, setForceable] = useState(false);
  const submit = async (force: boolean) => {
    const ok = await act.run(() => post(`${props.base}/snapshots/${encodeURIComponent(snap.name)}/revert`, { force }));
    setForceable(!ok && !force);
    if (ok) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title={`Revert to ${snap.name}`} onClose={props.onClose}>
      <p>
        Back to the state of {when(new Date(snap.created * 1000))}: disk changes since then are discarded
        {snap.hasMemory ? " and the guest resumes from that moment" : ""}. Other snapshots are kept.
      </p>
      <ErrorBox error={act.error} />
      <div className="actions" style={{ justifyContent: "flex-end", marginTop: 14 }}>
        <button onClick={props.onClose}>Cancel</button>
        {forceable && act.error && /force/i.test(act.error) ? (
          <button className="danger" disabled={act.busy} onClick={() => submit(true)}>
            {act.busy ? "Reverting" : "Revert anyway"}
          </button>
        ) : (
          <button className="danger" disabled={act.busy} onClick={() => submit(false)}>
            {act.busy ? "Reverting" : "Revert"}
          </button>
        )}
      </div>
    </Modal>
  );
}

interface Plan {
  order: string[];
  problems: { name: string; reason: string }[];
}

/** Choose which snapshots to delete in the tree; one task deletes them, deepest first. */
function DeleteSnapshots(props: { list: Snap[]; initial?: string; base: string; vmState: string; onClose: () => void; onDone: () => void }) {
  const { list } = props;
  const act = useAction();
  const [sel, setSel] = useState<Set<string>>(new Set(props.initial ? [props.initial] : []));
  const [plan, setPlan] = useState<{ key: string; plan?: Plan; error?: string }>();
  const rows = buildSnapRows(list, false);
  const key = [...sel].sort().join("\n");
  const blocked = (x: Snap) => (x.deletable ? undefined : x.reason);
  const allowed = list.filter((x) => x.deletable).map((x) => x.name);
  // Ask the server whether libvirt accepts this selection, in the order it would delete them.
  useEffect(() => {
    if (sel.size === 0) {
      setPlan({ key });
      return;
    }
    let live = true;
    const h = setTimeout(() => {
      post<Plan>(`${props.base}/snapshots/delete-plan`, { names: [...sel] }).then(
        (p) => live && setPlan({ key, plan: p }),
        (e) => live && setPlan({ key, error: String(e.message ?? e) }),
      );
    }, 150);
    return () => {
      live = false;
      clearTimeout(h);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);
  const toggle = (x: Snap, on: boolean) => setSel((c) => {
    const n = new Set(c);
    if (on) n.add(x.name);
    else n.delete(x.name);
    return n;
  });
  const branch = (names: string[]) => setSel((c) => new Set([...c, ...names.filter((n) => allowed.includes(n))]));
  const ready = plan?.key === key;
  const problems = ready ? plan?.plan?.problems ?? [] : [];
  const n = sel.size;
  const external = list.some((x) => sel.has(x.name) && x.kind === "external");
  const submit = async () => {
    const ok = await act.run(() => post(`${props.base}/snapshots/delete?async=1`, { names: [...sel] }));
    props.onDone();
    if (ok) props.onClose();
  };
  const title = props.initial ? `Delete ${props.initial}` : "Delete snapshots";
  return (
    <Modal title={title} onClose={() => !act.busy && props.onClose()} wide>
      <p>
        Snapshots you do not tick are kept; children of a deleted snapshot are attached to its parent. The VM's current data is unchanged.
        {external ? " External snapshots are merged into the disk, which can take a while." : ""}
      </p>
      <div className="stbar">
        <span aria-live="polite">
          <strong>{n}</strong> of {list.length} selected
        </span>
        <span className="grow" />
        <button onClick={() => setSel(new Set(allowed))} disabled={act.busy}>
          Select all
        </button>
        <button onClick={() => setSel(new Set())} disabled={act.busy || n === 0}>
          Select none
        </button>
      </div>
      <SnapTree list={list} vmState={props.vmState} mode="select" ariaLabel="Snapshots to delete" selected={sel} onToggle={toggle} onBranch={branch} blocked={blocked} />
      <p className="stkey">
        Space ticks a snapshot, B ticks it and everything below.
      </p>
      {rows.length > 0 && problems.length > 0 && (
        <>
          <p className="checksum">libvirt will not delete this selection as it stands</p>
          <CheckList summary={false} label="Why the selection cannot be deleted" items={problems.map((x) => ({ key: x.name, status: "error", title: x.name, detail: x.reason }))} />
        </>
      )}
      {ready && plan?.error && <StatusLine status="error">{plan.error}</StatusLine>}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={props.onClose} disabled={act.busy}>
          Cancel
        </button>
        <button className="danger" disabled={act.busy || n === 0 || !ready || problems.length > 0 || !!plan?.error} onClick={submit}>
          {act.busy ? "Deleting" : `Delete ${n} snapshot${n === 1 ? "" : "s"}`}
        </button>
      </footer>
    </Modal>
  );
}

function SchedulePanel({ d, base }: { d: VMDetail; base: string }) {
  const path = `${base}/snapshot-schedule`;
  const cur = useLive(() => get<{ schedule: Schedule | null; last?: number; nextRun?: number; runner?: "this" | "disabled" | "other"; otherController?: string; tick?: number; lastRun?: { kind: string; time: number; ok: boolean; error?: string } }>(path), path, (e) => e.type === "node" || (e.type === "task" && e.task?.status !== "running"));
  const act = useAction();
  const confirm = useConfirm();
  const [form, setForm] = useState<{ when: When; keep: number; prefix: string; memory: boolean } | undefined>();
  const sch = cur.data?.schedule;
  const edit = () => setForm({ when: whenFrom(sch ?? undefined), keep: sch?.keep ?? 7, prefix: sch?.prefix ?? "auto", memory: false });
  const save = async () => {
    if (!form) return;
    // A saved schedule is enabled. The prefix of an existing schedule is kept so its snapshots stay recognised.
    const body = { ...whenBody(form.when), keep: form.keep, prefix: form.prefix, memory: form.memory };
    if (await act.run(() => put(path, body))) {
      setForm(undefined);
      cur.reload();
    }
  };
  const remove = async () => {
    const ok = await confirm.ask({ title: "Remove snapshot schedule", message: `The snapshot schedule of ${d.name} is removed. Snapshots already taken are kept.`, confirm: "Remove schedule" });
    if (ok && (await act.run(() => del(path)))) cur.reload();
  };

  return (
    <section className="card" id="snap-schedule" style={{ marginTop: 20 }}>
      {confirm.dialog}
      <h3>Scheduled snapshots</h3>
      <ErrorBox error={cur.error} />
      {!form && (
        <>
          {sch ? (
            <p>
              {!sch.enabled && <><span className="badge">Paused</span>{" "}</>}
              {describeWhen(sch)}, keeping the newest {sch.keep}.
              {sch.memory ? " Each snapshot includes memory." : ""}
              {cur.data?.last ? ` Last taken ${when(new Date(cur.data.last * 1000))}.` : " None taken yet."}
            </p>
          ) : (
            <p className="muted">No schedule.</p>
          )}
          {sch && (
            <>
              <NextRunLine
                nextRun={cur.data?.nextRun}
                runner={cur.data?.runner}
                otherController={cur.data?.otherController}
                enabled={sch.enabled}
                tick={cur.data?.tick}
                skipsStopped={d.state === "shutoff"}
                onElapsed={cur.reload}
              />
              {cur.data?.lastRun && (
                <StatusLine status={cur.data.lastRun.ok ? "ok" : "error"}>
                  <strong>Last run:</strong> {cur.data.lastRun.kind === "snapshot prune" ? "pruning" : "taking a snapshot"} {cur.data.lastRun.ok ? "succeeded" : `failed: ${cur.data.lastRun.error}`} ({when(new Date(cur.data.lastRun.time * 1000))})
                </StatusLine>
              )}
            </>
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
            <ScheduleWhen value={form.when} onChange={(when) => setForm({ ...form, when })} />
            <Field label="Keep the newest" hint="Older scheduled ones are deleted.">
              <input type="number" min={1} value={form.keep} onChange={(e) => setForm({ ...form, keep: Number(e.target.value) })} />
            </Field>
          </div>
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
