import { useEffect, useState } from "react";
import { get, post, RollNode, RollNodePlan, RollPlan, RollRun, RollStep } from "../api";
import { CheckList, CheckStatus, StatusDot, StatusLine } from "../Checks";
import { ErrorBox, Modal, Progress, useAction, useConfirm } from "../ui";
import "./rolling.css";

type Status = { run: RollRun | null; active: boolean };

const stepStatus: Record<RollStep["status"], CheckStatus> = { ok: "ok", running: "warning", failed: "error", skipped: "info", pending: "pending" };
const stepWord: Record<RollStep["status"], string> = { ok: "Done", running: "Running", failed: "Failed", skipped: "Skipped", pending: "Waiting" };
const nodeWord: Record<RollNode["status"], string> = { pending: "Waiting", running: "Running", done: "Done", skipped: "Left out", current: "Up to date", failed: "Failed" };

/** True when the newest run still needs the user: it is going, or it stopped and was not dismissed. */
export function needsAttention(r: RollRun | null | undefined): boolean {
  return !!r && (r.status === "running" || ((r.status === "failed" || r.status === "cancelled") && !r.ack));
}

/**
 * The rolling update of the datacenter (or of one node, when `nodes` is given): the plan first, then the run.
 * Nothing starts until the plan is shown and Start update is pressed.
 */
export default function RollingUpdate({ nodes, title, onClose }: { nodes?: number[]; title: string; onClose: () => void }) {
  const [status, setStatus] = useState<Status | undefined>();
  const [lost, setLost] = useState(false);
  const refresh = () =>
    get<Status>("/rolling-update").then(
      (s) => {
        setStatus(s);
        setLost(false);
      },
      () => setLost(true),
    );
  useEffect(() => {
    refresh();
  }, []);
  const running = status?.run?.status === "running";
  useEffect(() => {
    if (!running && !lost) return;
    const t = setInterval(refresh, 3000);
    return () => clearInterval(t);
  }, [running, lost]);

  const showRun = status && needsAttention(status.run);
  return (
    <Modal title={title} onClose={onClose} wide>
      {!status && <p className="muted">{lost ? "The controller does not answer. Trying again." : "Reading the state"}</p>}
      {status && showRun && status.run && <RunView run={status.run} lost={lost} onChange={refresh} onClose={onClose} />}
      {status && !showRun && <PlanView nodes={nodes} onStarted={refresh} onClose={onClose} />}
    </Modal>
  );
}

function PlanView({ nodes, onStarted, onClose }: { nodes?: number[]; onStarted: () => void; onClose: () => void }) {
  const [plan, setPlan] = useState<RollPlan | undefined>();
  const [choice, setChoice] = useState("");
  const load = useAction();
  const start = useAction();
  useEffect(() => {
    load.run(async () => setPlan(await post<RollPlan>("/rolling-update/plan", { nodes: nodes ?? [] })));
  }, []);
  const effective = (n: RollNodePlan) => (n.action === "update" && n.stuck.length > 0 && choice === "skip" ? "skip" : n.action);
  const willUpdate = plan ? plan.nodes.filter((n) => effective(n) === "update").length : 0;
  const blocked = !plan || plan.refusals.length > 0 || willUpdate === 0 || (plan.needsChoice && !choice);
  const go = () =>
    start.run(async () => {
      await post("/rolling-update", { nodes: nodes ?? [], unmovable: choice });
      onStarted();
    });
  return (
    <>
      <p>
        The nodes are updated one at a time. Each node is put in maintenance, its running virtual machines are moved to the others without stopping them, the updates are installed, the node is restarted if that is needed, and after it is checked
        and healthy again the virtual machines move back. The next node starts only when the one before is done. Any failure stops the run.
      </p>
      {load.busy && !plan && <p className="muted">Reading the nodes and their updates. This takes about half a minute.</p>}
      <ErrorBox error={load.error} />
      {plan && plan.refusals.length > 0 && (
        <div className="rollrefuse" role="alert">
          <strong>This cannot start yet</strong>
          {plan.refusals.map((r, i) => (
            <StatusLine key={i} status="error">
              {r}
            </StatusLine>
          ))}
        </div>
      )}
      {plan && (
        <div className="table-scroll">
          <table className="compact rollplan">
            <thead>
              <tr>
                <th>Node</th>
                <th>Updates</th>
                <th>Restart</th>
                <th>Virtual machines</th>
              </tr>
            </thead>
            <tbody>
              {plan.nodes.map((n) => (
                <PlanRow key={n.node} n={n} action={effective(n)} />
              ))}
            </tbody>
          </table>
        </div>
      )}
      {plan?.nodes.filter((n) => n.note && effective(n) === "update").map((n) => (
        <StatusLine key={n.node} status="warning">
          {n.name}: {n.note}
        </StatusLine>
      ))}
      {plan && plan.needsChoice && (
        <div role="radiogroup" aria-label="Virtual machines that cannot move" className="rollchoice">
          <strong>Some virtual machines cannot move to another node. What should happen to them?</strong>
          <label className="pref-opt">
            <input type="radio" name="roll-choice" checked={choice === "shutdown"} onChange={() => setChoice("shutdown")} />
            Shut them down for the update and start them again afterwards
          </label>
          <label className="pref-opt">
            <input type="radio" name="roll-choice" checked={choice === "skip"} onChange={() => setChoice("skip")} />
            Leave out the nodes they run on; those nodes are not updated
          </label>
        </div>
      )}
      <ErrorBox error={start.error} />
      <footer>
        <button onClick={onClose}>Close</button>
        <button className="primary" disabled={blocked || start.busy} onClick={go}>
          Start update
        </button>
      </footer>
    </>
  );
}

function PlanRow({ n, action }: { n: RollNodePlan; action: string }) {
  if (n.action === "current") {
    return (
      <tr>
        <td>{n.name}</td>
        <td colSpan={3} className="muted">
          Nothing to update
        </td>
      </tr>
    );
  }
  return (
    <tr>
      <td>
        {n.name}
        {n.local && <div className="muted">Runs the controller, updated last</div>}
      </td>
      <td>{action === "skip" ? <span className="muted">Left out</span> : n.updates}</td>
      <td title={n.rebootWhy}>{action === "skip" ? "" : n.reboot ? "Yes" : "No"}</td>
      <td>
        {action === "skip" && <div className="muted">{n.skipWhy}</div>}
        {action !== "skip" && n.moves.length === 0 && n.stuck.length === 0 && <span className="muted">None running</span>}
        {action !== "skip" &&
          n.moves.map((m) => (
            <div key={m.vm.uuid}>
              {m.vm.name} moves to {m.toName}
            </div>
          ))}
        {n.stuck.map((s) => (
          <CheckList key={s.vm.uuid} summary={false} items={[{ status: "warning", title: `${s.vm.name} cannot move.`, detail: s.reasons.join(" ") }]} />
        ))}
      </td>
    </tr>
  );
}

function RunView({ run, lost, onChange, onClose }: { run: RollRun; lost: boolean; onChange: () => void; onClose: () => void }) {
  const act = useAction();
  const confirm = useConfirm();
  const updating = run.nodes.filter((n) => n.action === "update");
  const steps = updating.flatMap((n) => n.steps);
  const done = steps.filter((s) => s.status === "ok" || s.status === "skipped").length;
  const running = run.status === "running";
  const call = (path: string) => act.run(async () => (await post(path), onChange()));
  const stop = async () => {
    if (await confirm.ask({ title: "Stop the update", message: "The update stops after the step that is running now. Nothing is undone: the page then says in what state the nodes are, and you can continue or put things back.", confirm: "Stop after this step" })) call("/rolling-update/cancel");
  };
  const heading = {
    running: run.cancel ? "Stopping after the current step" : run.restoring ? "Putting things back" : "Updating",
    ok: "Finished",
    failed: "Stopped after a failure",
    cancelled: run.restoring ? "Put back" : "Stopped",
  }[run.status];
  return (
    <>
      <p className="rollhead">
        <strong>{heading}</strong>
        {running && <span className="muted"> {done} of {steps.length} steps done</span>}
      </p>
      <Progress value={steps.length ? done / steps.length : 0} failed={run.status === "failed"} />
      {lost && <p className="muted">The controller does not answer. It may be restarting. The update carries on by itself when it is back.</p>}
      {(run.resumed ?? 0) > 0 && <p className="muted">The controller was restarted during this update and carried on by itself.</p>}
      {run.status === "ok" && <StatusLine status="ok">{run.summary || "All nodes are done."}</StatusLine>}
      {(run.status === "failed" || run.status === "cancelled") && (
        <div className="rollstop" role="alert">
          {run.error && <StatusLine status="error">{run.error}</StatusLine>}
          <p>
            <strong>State now:</strong> {run.condition}
          </p>
          <p className="muted">
            Continue runs the step again and goes on. Put things back ends maintenance on the node, starts the virtual machines that were shut down and moves the others back, and ends the update.
          </p>
        </div>
      )}
      <div className="rollnodes">
        {run.nodes.map((n, i) => (
          <NodeBlock key={n.node} n={n} current={i === run.cur && running} />
        ))}
      </div>
      {confirm.dialog}
      <ErrorBox error={act.error} />
      <footer>
        {running && (
          <button className="danger" disabled={act.busy || run.cancel} onClick={stop}>
            Stop after this step
          </button>
        )}
        {(run.status === "failed" || run.status === "cancelled") && (
          <>
            <button disabled={act.busy} onClick={() => call("/rolling-update/dismiss")} title="Leave things as they are; the alert goes away">
              Dismiss
            </button>
            <button disabled={act.busy} onClick={() => call("/rolling-update/restore")}>
              Put things back
            </button>
            <button className="primary" disabled={act.busy} onClick={() => call("/rolling-update/continue")}>
              Continue
            </button>
          </>
        )}
        {run.status === "ok" && (
          <button className="primary" onClick={() => call("/rolling-update/dismiss").then(onClose)}>
            Done
          </button>
        )}
        <button onClick={onClose}>Close</button>
      </footer>
    </>
  );
}

function NodeBlock({ n, current }: { n: RollNode; current: boolean }) {
  const open = current || n.status === "failed";
  return (
    <section className="rollnode">
      <h3>
        <span className={"badge es-" + (n.status === "done" || n.status === "current" ? "ok" : n.status === "failed" ? "failed" : "pending")}>{nodeWord[n.status]}</span> {n.name}
      </h3>
      {n.action === "skip" && <p className="muted">{n.skipWhy}</p>}
      {n.action === "current" && <p className="muted">There was nothing to update.</p>}
      {n.action === "update" && !open && n.status === "done" && (
        <p className="muted">
          {n.installed ?? 0} package(s) installed{n.reboot ? ", restarted" : ""}, checked.
          {n.heldBack && n.heldBack.length > 0 ? ` ${n.heldBack.length} held back by the package tool.` : ""}
        </p>
      )}
      {n.action === "update" && open && (
        <ul className="rollsteps">
          {n.steps.map((s) => (
            <li key={s.id}>
              <StatusDot status={stepStatus[s.status]} />
              <span>
                {s.title}: <strong>{stepWord[s.status]}</strong>
                {s.detail && <span className="muted"> {s.detail}</span>}
              </span>
            </li>
          ))}
        </ul>
      )}
      {open && n.checks && n.checks.length > 0 && (
        <CheckList summary label="Checks after the update" items={n.checks.map((c) => ({ status: c.status === "problem" ? "error" : c.status === "fixed" ? "warning" : "ok", detail: c.detail, key: c.id }))} />
      )}
    </section>
  );
}
