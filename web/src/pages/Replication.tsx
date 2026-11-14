// VM replication: the card on a source VM (set up, status, replicate now, stop), the dialog that sets it up, and the
// cards of the standby's copy ("replica": fail over, delete) and of the old VM after a failover ("stale copy").
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { bytes, del, get, NodeStatus, post, put, ReplicaInfo, ReplicationStatus, VMDetail } from "../api";
import { atLeast, useRole } from "../auth";
import { StatusLine } from "../Checks";
import { useLive } from "../live";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";

const everyOptions = [
  { value: "5", label: "5 minutes" },
  { value: "15", label: "15 minutes" },
  { value: "30", label: "30 minutes" },
  { value: "60", label: "1 hour" },
  { value: "360", label: "6 hours" },
  { value: "1440", label: "1 day" },
];

/** "14:05", or with the date when it is not today. */
export function clock(unix: number): string {
  const d = new Date(unix * 1000);
  const t = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString()} ${t}`;
}

/** "3 minutes", "2 hours", "1 day". */
export function span(seconds: number): string {
  const s = Math.max(0, seconds);
  const n = (v: number, unit: string) => `${v} ${unit}${v === 1 ? "" : "s"}`;
  if (s < 90) return "less than 2 minutes";
  if (s < 5400) return n(Math.round(s / 60), "minute");
  if (s < 129600) return n(Math.round(s / 3600), "hour");
  return n(Math.round(s / 86400), "day");
}
const ago = (unix: number) => `${span(Date.now() / 1000 - unix)} ago`;

/** The other nodes that are online. */
function useOtherNodes(node: number, wanted = true) {
  const [nodes, setNodes] = useState<NodeStatus[] | undefined>();
  useEffect(() => {
    if (!wanted) return;
    get<{ nodes: NodeStatus[] }>("/nodes").then((r) => setNodes(r.nodes.filter((n) => n.id !== node && n.state === "online")), () => setNodes([]));
  }, [node, wanted]);
  return nodes;
}

const nodeName = (n: NodeStatus) => n.hostname || n.addr;

// ---------------------------------------------------------------- source VM

export function ReplicationCard({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const op = atLeast(useRole(), "operator");
  const st = useLive(() => get<ReplicationStatus>(`${base}/replication`), `${base}/repl`, (e) => e.type === "domain" ? e.uuid === d.uuid : e.type === "node" || (e.type === "task" && e.task?.status !== "running"));
  const act = useAction();
  const nodes = useOtherNodes(d.node, op);
  const [dialog, setDialog] = useState<"setup" | "stop">();
  const s = st.data;
  const running = !!s?.running;
  // The card follows a run that is going.
  useEffect(() => {
    if (!running) return;
    const h = setInterval(() => document.visibilityState !== "hidden" && st.reload(), 5000);
    return () => clearInterval(h);
  }, [running, st.reload]);
  if (!s) return <section className="card ov-wide" id="replication" aria-label="Replication"><h3>Replication</h3><ErrorBox error={st.error} /></section>;
  const cfg = s.config;
  const reload = () => {
    st.reload();
    onChanged();
  };
  const noNode = nodes !== undefined && nodes.length === 0;
  return (
    <section className="card ov-wide" id="replication" aria-label="Replication">
      <h3>Replication</h3>
      {!s.configured || !cfg ? (
        <>
          <p>Keep a copy of this VM on another node that starts with one click if this node fails.</p>
          {op && (
            <button className="primary" disabled={!nodes || noNode} title={noNode ? "No other node is online" : undefined} onClick={() => setDialog("setup")}>
              Set up replication
            </button>
          )}
          {noNode && <p className="muted">No other node is online.</p>}
        </>
      ) : (
        <>
          <dl className="ov-sum">
            <dt>Standby node</dt>
            <dd>{cfg.targetName}</dd>
            <dt>Every</dt>
            <dd>{everyOptions.find((o) => o.value === String(cfg.everyMinutes))?.label ?? `${cfg.everyMinutes} minutes`}</dd>
            <dt>Last copy</dt>
            <dd>{cfg.lastOk ? `${clock(cfg.lastOk)}, ${ago(cfg.lastOk)}` : "No copy yet"}</dd>
            <dt>Sent</dt>
            <dd>{cfg.lastOk ? `${bytes(cfg.lastBytes ?? 0)}, ${cfg.lastKind === "incremental" ? "changes only" : "everything"}` : "–"}</dd>
            <dt>Next copy</dt>
            <dd>{s.runner === "disabled" || !cfg.next ? "not scheduled: this controller runs without a scheduler" : clock(cfg.next)}</dd>
          </dl>
          {running && <StatusLine status="info">Copying now</StatusLine>}
          {s.error && !running && <StatusLine status="warning">The last run failed{s.errorAt ? ` at ${clock(s.errorAt)}` : ""}: {s.error}</StatusLine>}
          {s.replica?.late && <StatusLine status="warning">The copy on {cfg.targetName} is older than twice its interval.</StatusLine>}
          {s.standbyOffline && <StatusLine status="warning">{cfg.targetName} is offline, so the copy cannot be updated.</StatusLine>}
          <ErrorBox error={act.error} />
          {op && (
            <div className="actions">
              {running ? (
                <button disabled={act.busy} onClick={async () => (await act.run(() => post(`${base}/replication/cancel`))) && reload()}>
                  Cancel copy
                </button>
              ) : (
                <button className="primary" disabled={act.busy} onClick={async () => (await act.run(() => post(`${base}/replication/run`))) && reload()}>
                  Replicate now
                </button>
              )}
              <button disabled={act.busy} onClick={() => setDialog("setup")}>
                Change
              </button>
              <button className="danger" disabled={act.busy} onClick={() => setDialog("stop")}>
                Stop replication
              </button>
            </div>
          )}
        </>
      )}
      {dialog === "setup" && nodes && <SetupDialog d={d} base={base} nodes={nodes} current={cfg} onClose={() => setDialog(undefined)} onDone={reload} />}
      {dialog === "stop" && cfg && <StopDialog base={base} target={cfg.targetName} onClose={() => setDialog(undefined)} onDone={reload} />}
    </section>
  );
}

/** Where to and how often. The pool is chosen by the app. */
function SetupDialog(props: { d: VMDetail; base: string; nodes: NodeStatus[]; current?: ReplicationStatus["config"]; onClose: () => void; onDone: () => void }) {
  const { current } = props;
  const [target, setTarget] = useState(String(props.nodes.find((n) => n.id === current?.targetNode)?.id ?? props.nodes[0]?.id ?? 0));
  const [every, setEvery] = useState(String(current?.everyMinutes ?? 15));
  const act = useAction();
  const [result, setResult] = useState<{ warnings: string[]; note?: string; task?: number }>();
  const save = () =>
    act.run(async () => {
      const r = await put<{ warnings?: string[]; note?: string; task?: number }>(`${props.base}/replication`, { targetNode: Number(target), everyMinutes: Number(every) });
      setResult({ warnings: r.warnings ?? [], note: r.note, task: r.task });
      props.onDone();
    });
  const close = () => {
    props.onDone();
    props.onClose();
  };
  if (result) {
    return (
      <Modal title={`Replicate ${props.d.name}`} onClose={close}>
        <p>{result.task ? "Replication is set up. The first copy is running: follow it in the task list." : result.note ? "Replication is set up." : "Saved."}</p>
        {result.note && <StatusLine status="warning">{result.note}</StatusLine>}
        {result.warnings.map((w) => (
          <StatusLine key={w} status="warning">
            {w}
          </StatusLine>
        ))}
        <footer>
          <button className="primary" autoFocus onClick={close}>
            Close
          </button>
        </footer>
      </Modal>
    );
  }
  return (
    <Modal title={`Replicate ${props.d.name}`} onClose={props.onClose}>
      <Field label="Replicate to">
        <Select label="Replicate to" value={target} onChange={setTarget} options={props.nodes.map((n) => ({ value: String(n.id), label: nodeName(n), dot: "n-" + n.state }))} />
      </Field>
      <Field label="Every">
        <Select label="Every" value={every} onChange={setEvery} options={everyOptions} />
      </Field>
      <p className="muted">The first copy sends the whole VM, later copies only what changed. The standby node keeps a copy that is ready to start. A VM with a disk on shared storage cannot be replicated: that disk is already on both nodes.</p>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={props.onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || target === "0"} onClick={save}>
          {current ? "Save" : "Start replication"}
        </button>
      </footer>
    </Modal>
  );
}

function StopDialog(props: { base: string; target: string; onClose: () => void; onDone: () => void }) {
  const [copy, setCopy] = useState(true);
  const act = useAction();
  return (
    <Modal title="Stop replication" onClose={props.onClose}>
      <p>The VM keeps running and is no longer copied to {props.target}.</p>
      <label className="check">
        <input type="checkbox" checked={copy} onChange={(e) => setCopy(e.target.checked)} />
        Also delete the copy on {props.target}
      </label>
      <ErrorBox error={act.error} />
      <footer>
        <button autoFocus onClick={props.onClose}>
          Cancel
        </button>
        <button
          className="danger"
          disabled={act.busy}
          onClick={async () => {
            if (await act.run(() => del(`${props.base}/replication${copy ? "?deleteReplica=1" : ""}`))) {
              props.onDone();
              props.onClose();
            }
          }}
        >
          Stop replication
        </button>
      </footer>
    </Modal>
  );
}

// ---------------------------------------------------------------- the standby's copy

export function ReplicaCard({ d, base }: { d: VMDetail; base: string }) {
  const op = atLeast(useRole(), "operator");
  const nav = useNavigate();
  const info = useLive(() => get<ReplicaInfo>(`${base}/replica`), `${base}/replica`, (e) => e.type === "domain" ? e.uuid === d.uuid : e.type === "node" || e.type === "task");
  const act = useAction();
  const confirm = useConfirm();
  const r = info.data;
  const copying = !!r?.running;
  useEffect(() => {
    if (!copying) return;
    const h = setInterval(() => document.visibilityState !== "hidden" && info.reload(), 5000);
    return () => clearInterval(h);
  }, [copying, info.reload]);
  if (!r) return <section className="card ov-wide" aria-label="Replica"><ErrorBox error={info.error} /></section>;
  const src = r.sourceName;
  const fo = r.failover;
  const at = r.pointTime ?? r.last ?? 0;
  const t = at ? clock(at) : "";
  const points = r.points?.length ?? d.replica?.points ?? 0;
  const failover = async () => {
    const planned = fo.kind === "planned";
    const message = planned
      ? `${fo.vmState && fo.vmState !== "running" ? `The VM is shut off on ${src}. Its last changes are copied here, and the VM starts here.` : `${src} runs this VM now. It is shut down (if the guest has not shut down after 3 minutes, nothing is changed and the VM keeps running), its last changes are copied here, and the VM starts here.`} Nothing is lost. Replication then runs back to ${src}.`
      : fo.kind === "gone"
        ? `${src} no longer has this VM. Start it here from the copy of ${t}. Changes after that time are lost.`
        : `What you lose: everything ${d.name} wrote on ${src} after ${t}. Risk: Virticle cannot stop ${src}, it is offline. If the VM is still running there (for example only the network to it is down), two copies of the VM would run at the same time with the same MAC and IP addresses on your network. Each copy has its own disks, so no data is destroyed, but the two copies diverge and you will have to throw one away. When ${src} comes back, its copy is shut off and marked as old; its disks are not deleted. Only continue if you know ${src} is down.`;
    const ok = await confirm.ask({
      title: planned ? "Planned failover" : "Start from the copy",
      message,
      confirm: planned ? "Planned failover" : `Start here from the copy of ${t}`,
      danger: !planned,
    });
    if (!ok) return;
    if (await act.run(() => post(`${base}/replica/failover?async=1`, { mode: planned ? "planned" : "unplanned" }))) info.reload();
  };
  const remove = async () => {
    const ok = await confirm.ask({
      title: "Delete replica",
      message: `Deletes the copy on this node. The VM on ${src} keeps running and stops replicating.`,
      confirm: "Delete replica",
    });
    if (ok && (await act.run(() => del(`${base}/replica`)))) nav("/datacenter/vms");
  };
  const canFail = fo.kind === "planned" || fo.kind === "unplanned" || fo.kind === "gone";
  return (
    <section className="card ov-wide" id="replica" aria-label="Replica">
      <h3>Replica</h3>
      <p>
        This VM is the replica of {d.name} on {src}. {at ? `Its copy is from ${t} (${span(Date.now() / 1000 - at)} old).` : "No copy has arrived yet."}
      </p>
      <p className="muted">
        {bytes(r.bytes)}, {points} {points === 1 ? "copy" : "copies"} kept
      </p>
      {copying && <StatusLine status="info">A copy is arriving now</StatusLine>}
      {r.late && <StatusLine status="warning">The copy is older than twice its interval.</StatusLine>}
      {r.error && <StatusLine status="warning">{r.error}</StatusLine>}
      {fo.message && fo.kind === "none" && <p className="muted">{fo.message}</p>}
      <ErrorBox error={act.error} />
      {op && (
        <div className="actions">
          {canFail && (
            <button className={fo.kind === "planned" ? "primary" : "danger"} disabled={act.busy || copying || !at} onClick={failover}>
              {fo.kind === "planned" ? "Planned failover" : `Start here from the copy of ${t}`}
            </button>
          )}
          <button disabled={act.busy || copying} onClick={remove}>
            Delete replica
          </button>
        </div>
      )}
      {confirm.dialog}
    </section>
  );
}

// ---------------------------------------------------------------- the old VM after a failover

export function StaleCard({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const op = atLeast(useRole(), "operator");
  const act = useAction();
  const confirm = useConfirm();
  const [note, setNote] = useState<string[]>([]);
  const st = d.stale;
  if (!st) return null;
  const back = async () => {
    const ok = await confirm.ask({
      title: "Replicate back",
      message: `This old copy is deleted, disks included, once a current copy of the running VM on ${st.toName} has arrived here. The VM then has a replica here again, and you can fail back with one click. The copy sends the whole VM, not only the changes.`,
      confirm: "Replicate back",
      danger: false,
    });
    if (!ok) return;
    await act.run(async () => {
      const r = await put<{ warnings?: string[]; note?: string }>(`/nodes/${st.toNode}/vms/${d.uuid}/replication`, { targetNode: d.node, everyMinutes: 15, replaceStale: true });
      setNote([...(r.note ? [r.note] : []), ...(r.warnings ?? [])]);
      onChanged();
    });
  };
  return (
    <section className="card ov-wide" id="stale" aria-label="Old copy">
      <h3>Old copy</h3>
      <p>
        This is the old copy of {d.name}, from before it was started on {st.toName}. It is shut off and cannot be started, so the VM does not run twice.
      </p>
      {note.map((n) => (
        <StatusLine key={n} status="warning">
          {n}
        </StatusLine>
      ))}
      <ErrorBox error={act.error} />
      {op && (
        <div className="actions">
          <button className="primary" disabled={act.busy} onClick={back}>
            Replicate back
          </button>
        </div>
      )}
      {confirm.dialog}
    </section>
  );
}
