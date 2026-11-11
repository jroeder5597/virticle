import { useEffect, useState } from "react";
import { bytes, EvacPlan, EvacRun, get, NodeStatus, post } from "../api";
import Select from "../Select";
import { ErrorBox, Modal, useAction, useConfirm } from "../ui";
import { CheckList, StatusLine } from "../Checks";

/** Plan and run the evacuation of one node. */
export default function Evacuate({ node, nodes, onClose }: { node: NodeStatus; nodes: NodeStatus[]; onClose: () => void }) {
  const [includeShutOff, setIncludeShutOff] = useState(true);
  const [targets, setTargets] = useState<Record<number, boolean>>({});
  const [concurrency, setConcurrency] = useState(1);
  const [plan, setPlan] = useState<EvacPlan | undefined>();
  const [picked, setPicked] = useState<Record<string, boolean>>({});
  const [run, setRun] = useState<EvacRun | undefined>();
  const check = useAction();
  const start = useAction();
  const cancel = useAction();
  const confirm = useConfirm();
  const others = nodes.filter((n) => n.id !== node.id);
  const chosen = others.filter((n) => targets[n.id] !== false).map((n) => n.id);
  const name = node.hostname || node.addr;

  const runCheck = async () => {
    setPlan(undefined);
    await check.run(async () => {
      const p = await post<EvacPlan>(`/nodes/${node.id}/evacuate/check`, { targets: chosen, includeShutOff });
      setPlan(p);
      const all: Record<string, boolean> = {};
      [...p.assignments.map((a) => a.vm), ...p.blocked.map((b) => b.vm)].forEach((v) => (all[v.uuid] = true));
      setPicked(all);
    });
  };

  // Pick up a run that is already going (or just ended) when the dialog opens.
  useEffect(() => {
    get<{ run: EvacRun | null }>(`/nodes/${node.id}/evacuate`).then(
      (r) => r.run && r.run.running && setRun(r.run),
      () => {},
    );
  }, [node.id]);
  useEffect(() => {
    if (!run?.running) return;
    const t = setInterval(() => get<{ run: EvacRun | null }>(`/nodes/${node.id}/evacuate`).then((r) => r.run && setRun(r.run), () => {}), 2000);
    return () => clearInterval(t);
  }, [run?.running, node.id]);

  const go = async () => {
    const vms = Object.keys(picked).filter((u) => picked[u]);
    await start.run(async () => {
      await post(`/nodes/${node.id}/evacuate`, { targets: chosen, vms, includeShutOff, concurrency });
      const r = await get<{ run: EvacRun | null }>(`/nodes/${node.id}/evacuate`);
      if (r.run) setRun(r.run);
    });
  };

  const done = run ? run.results.filter((r) => r.status === "ok" || r.status === "failed" || r.status === "blocked" || r.status === "skipped").length : 0;
  return (
    <Modal title={`Evacuate ${name}`} onClose={onClose} wide>
      {!run && (
        <>
          <p>
            Moves the virtual machines of this node to other nodes with the normal migration (every move gets its own task and pre-flight). Nodes in maintenance are never used as a
            target.
          </p>
          <div className="evacopts">
            <div>
              <strong>Targets</strong>
              {others.length === 0 && <div className="muted">There is no other node.</div>}
              {others.map((n) => (
                <label key={n.id} className="check">
                  <input type="checkbox" checked={targets[n.id] !== false} disabled={n.state !== "online" || n.maintenance} onChange={(e) => setTargets({ ...targets, [n.id]: e.target.checked })} />
                  {n.hostname || n.addr}
                  {n.state !== "online" && <span className="badge s-offline">{n.state}</span>}
                  {n.maintenance && <span className="badge maint">maintenance</span>}
                </label>
              ))}
            </div>
            <div>
              <label className="check">
                <input type="checkbox" checked={includeShutOff} onChange={(e) => setIncludeShutOff(e.target.checked)} />
                Include VMs that are shut off
              </label>
              <label className="check">
                Moves at the same time
                <Select auto label="Moves at the same time" value={String(concurrency)} onChange={(v) => setConcurrency(Number(v))} options={[1, 2, 3].map((n) => ({ value: String(n), label: String(n) }))} />
              </label>
            </div>
          </div>
          <button onClick={runCheck} disabled={check.busy || others.length === 0}>
            {check.busy ? "Checking" : plan ? "Check again" : "Check what can move"}
          </button>
          <ErrorBox error={check.error} />
          {plan && (
            <div className="evacplan">
              <table className="compact">
                <thead>
                  <tr>
                    <th />
                    <th>VM</th>
                    <th>State</th>
                    <th>Memory</th>
                    <th>Goes to</th>
                  </tr>
                </thead>
                <tbody>
                  {plan.assignments.map((a) => (
                    <tr key={a.vm.uuid}>
                      <td>
                        <input type="checkbox" checked={!!picked[a.vm.uuid]} onChange={(e) => setPicked({ ...picked, [a.vm.uuid]: e.target.checked })} aria-label={`Move ${a.vm.name}`} />
                      </td>
                      <td>{a.vm.name}</td>
                      <td>{a.vm.state}</td>
                      <td>{bytes(a.vm.memoryKiB * 1024)}</td>
                      <td>{a.targetName}</td>
                    </tr>
                  ))}
                  {plan.blocked.map((b) => (
                    <tr key={b.vm.uuid} className="blockedrow">
                      <td>
                        <span className="sev critical">blocked</span>
                      </td>
                      <td>{b.vm.name}</td>
                      <td>{b.vm.state}</td>
                      <td>{bytes(b.vm.memoryKiB * 1024)}</td>
                      <td>
                        <CheckList summary={false} label="Why it is blocked" items={b.reasons.map((r) => ({ status: "error", detail: r }))} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {plan.assignments.length + plan.blocked.length === 0 && <div className="muted">This node has no virtual machines to move.</div>}
              <ErrorBox error={start.error} />
              <footer>
                <button onClick={onClose}>Close</button>
                <button className="primary" onClick={go} disabled={start.busy || plan.assignments.every((a) => !picked[a.vm.uuid])}>
                  Evacuate {plan.assignments.filter((a) => picked[a.vm.uuid]).length} VM(s)
                </button>
              </footer>
            </div>
          )}
        </>
      )}
      {run && (
        <>
          <div className="bar" title={`${done} of ${run.results.length}`}>
            <div style={{ width: `${(done / Math.max(1, run.results.length)) * 100}%` }} />
          </div>
          <p>
            {run.running ? (run.cancelled ? "Cancelling: no further VMs will be started." : `Evacuating ${done} of ${run.results.length} finished.`) : run.cancelled ? "Cancelled." : "Finished."}
          </p>
          <table className="compact">
            <thead>
              <tr>
                <th>VM</th>
                <th>Goes to</th>
                <th>Result</th>
              </tr>
            </thead>
            <tbody>
              {run.results.map((r) => (
                <tr key={r.uuid}>
                  <td>{r.name}</td>
                  <td>{r.targetName ?? ""}</td>
                  <td>
                    <span className={"badge es-" + r.status}>{r.status}</span> {r.error && <StatusLine status="error">{r.error}</StatusLine>}
                    {r.reasons && (
                      <CheckList summary={false} label="Reasons" items={r.reasons.map((x) => ({ status: "warning", detail: x }))} />
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {confirm.dialog}
          <ErrorBox error={cancel.error} />
          <footer>
            {run.running && (
              <button className="danger" disabled={cancel.busy || run.cancelled} onClick={async () => (await confirm.ask({ title: "Stop the evacuation", message: `No further VMs of ${name} are moved. Moves that are already running finish.`, confirm: "Stop evacuation" })) && cancel.run(() => post(`/nodes/${node.id}/evacuate/cancel`))}>
                Stop starting new moves
              </button>
            )}
            <button onClick={onClose}>Close</button>
          </footer>
        </>
      )}
    </Modal>
  );
}
