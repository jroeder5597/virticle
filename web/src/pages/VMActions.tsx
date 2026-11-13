// The "Actions" menu of the VM page: power, hardware, snapshots, clone, template, migrate, XML, delete.
// Entries that do not apply to the VM's state are disabled with a reason, never hidden, so the
// menu keeps its shape. Destructive power actions ask for confirmation first.
import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { del, post, put, VMDetail } from "../api";
import { atLeast, useRole } from "../auth";
import { DropdownMenu, MenuEntry } from "../Menu";
import { powerConfirm, powerLabel } from "../power";
import { ErrorBox, Modal, useAction, useConfirm } from "../ui";
import { CloneDialog, ConvertDialog } from "./Clone";
import MigrateDialog from "./Migrate";
import { HardwareDialog, HwArea } from "./Hardware";
import { TakeSnapshot } from "./Snapshots";

/** Hardware areas of Edit Settings: menu label and the section its dialog shows. */
export const hardwareAreas: [string, HwArea][] = [
  ["CPU and memory", "cpu"],
  ["Disks", "disks"],
  ["Network interfaces", "nics"],
  ["CD-ROM", "cdrom"],
  ["Boot order", "boot"],
  ["Firmware and TPM", "firmware"],
  ["Passthrough devices", "passthrough"],
];

export default function VMActions(props: {
  d: VMDetail;
  base: string;
  /** Switch tab, then scroll the element with this id into view. */
  goto: (tab: string, card?: string) => void;
  onChanged: () => void;
  /** Runs a call with the page's busy and error state. */
  run: (fn: () => Promise<unknown>) => Promise<boolean>;
  busy: boolean;
}) {
  const { d, base, goto, run, busy } = props;
  const nav = useNavigate();
  const op = atLeast(useRole(), "operator");
  const confirm = useConfirm();
  const [cloning, setCloning] = useState(false);
  const [converting, setConverting] = useState(false);
  const [taking, setTaking] = useState(false);
  const [migrating, setMigrating] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [editing, setEditing] = useState<[string, HwArea]>();

  const off = d.state === "shutoff";
  const running = d.state === "running";
  const paused = d.state === "paused";
  const need = op ? undefined : "Needs the operator role";
  const act = async (a: string, body: { action: string } = { action: a }) => {
    const spec = powerConfirm(a, [d.name]);
    if (spec && !(await confirm.ask(spec))) return;
    run(() => post(`${base}/action`, body));
  };
  const when = (ok: boolean, why: string) => ({ disabled: !op || busy || !ok, title: need ?? (ok ? undefined : why) });
  const stateNow = off ? "The VM is shut off" : `The VM is ${d.state}`;

  const power: MenuEntry[] = [
    { label: powerLabel.start, run: () => act("start"), ...when(off && !d.template, d.template ? "A template cannot be started" : "The VM is already on") },
    { label: powerLabel.reboot, run: () => act("reboot"), ...when(running, stateNow) },
    { label: powerLabel.shutdown, run: () => act("shutdown"), ...when(running, stateNow) },
    "sep",
    { label: powerLabel.pause, run: () => act("pause"), ...when(running, stateNow) },
    { label: powerLabel.resume, run: () => act("resume"), ...when(paused, "The VM is not paused") },
    "sep",
    { label: powerLabel.poweroff, danger: true, run: () => act("poweroff"), ...when(!off, stateNow) },
    { label: powerLabel.reset, danger: true, run: () => act("reset"), ...when(!off, stateNow) },
    "sep",
    { label: "Sync guest clock", run: () => run(() => post(`${base}/clock/sync`)), ...when(running && d.agent === "connected", !running ? stateNow : "Needs the guest agent to be connected") },
  ];
  const settings: MenuEntry[] = hardwareAreas.map(([label, area]) => ({ label, run: () => setEditing([label, area]) }));
  const snapshots: MenuEntry[] = [
    { label: "Take snapshot", run: () => setTaking(true), disabled: !op, title: need },
    { label: "Manage snapshots", run: () => goto("snapshots") },
    { label: "Schedule", run: () => goto("snapshots", "snap-schedule") },
  ];
  const convertBack = () => run(async () => { await put(`${base}/template`, { template: false }); props.onChanged(); });
  const canConvert = off || (running && d.agent === "connected");
  const migrate: MenuEntry[] = [
    { label: "Migrate to another node", run: () => setMigrating(true), disabled: !op, title: need },
    { label: "Move a disk to another pool", run: () => goto("hardware", "hw-disks"), disabled: !op, title: need },
  ];
  const more: MenuEntry[] = [
    { label: "Edit XML", run: () => goto("xml") },
    "sep",
    { label: "Delete VM", danger: true, run: () => setDeleting(true), ...when(off, "Shut the VM down first") },
  ];

  // The old copy after a failover can only be deleted (everything else is refused).
  const stale = d.state === "stale";
  const items: MenuEntry[] = stale
    ? [{ label: "Delete VM", danger: true, run: () => setDeleting(true), disabled: !op, title: need }]
    : [
    { label: "Power", items: power, disabled: !op, title: need },
    { label: "Edit Settings", items: settings, disabled: !op, title: need },
    { label: "Snapshots", items: snapshots },
    { label: "Clone", run: () => setCloning(true), ...when(off, "Shut the VM down to clone it") },
    d.template
      ? { label: "Convert back to a VM", run: convertBack, ...when(off, "Shut the VM down first") }
      : { label: "Convert to template", run: () => setConverting(true), ...when(canConvert, "Shut the VM down first, or start it with the guest agent connected") },
    { label: "Migrate", items: migrate, disabled: !op, title: need },
    { label: "More", items: more },
    ];

  return (
    <>
      <DropdownMenu label="Actions" items={items} className="primary" />
      {confirm.dialog}
      {editing && <HardwareDialog area={editing[1]} title={editing[0]} d={d} base={base} onChanged={props.onChanged} onClose={() => setEditing(undefined)} />}
      {cloning && <CloneDialog d={d} base={base} onClose={() => setCloning(false)} />}
      {converting && <ConvertDialog d={d} base={base} onClose={() => setConverting(false)} onDone={props.onChanged} />}
      {taking && <TakeSnapshot d={d} base={base} onClose={() => setTaking(false)} onDone={props.onChanged} />}
      {migrating && <MigrateDialog d={d} onClose={() => setMigrating(false)} />}
      {deleting && <DeleteVM d={d} base={base} onClose={() => setDeleting(false)} onDeleted={() => nav("/datacenter/vms")} />}
    </>
  );
}

function DeleteVM(props: { d: VMDetail; base: string; onClose: () => void; onDeleted: () => void }) {
  const { d } = props;
  const [disks, setDisks] = useState(true);
  const act = useAction();
  const [leftDisks, setLeftDisks] = useState<string[]>();
  const [replicaNote, setReplicaNote] = useState<string>();
  const [replica, setReplica] = useState(true);
  const owned = d.disks.filter((k) => k.device === "disk" && !k.readOnly && k.source);
  if (leftDisks || replicaNote)
    return (
      <Modal title={`${d.name} was deleted`} onClose={props.onDeleted}>
        {replicaNote && <p>The replica on {d.replication?.targetName} was not deleted: {replicaNote}</p>}
        {leftDisks && leftDisks.length > 0 && (
          <>
            <p>These disks are not in any storage pool and were left in place:</p>
            <p className="mono wrap">{leftDisks.join(", ")}</p>
          </>
        )}
        <footer>
          <button autoFocus onClick={props.onDeleted}>Close</button>
        </footer>
      </Modal>
    );
  return (
    <Modal title={`Delete ${d.name}?`} onClose={props.onClose}>
      <p>The VM's definition{d.snapshots.length ? `, its ${d.snapshots.length} snapshot record(s)` : ""} and firmware settings are removed. This cannot be undone.</p>
      {owned.length > 0 && (
        <label className="check">
          <input type="checkbox" checked={disks} onChange={(e) => setDisks(e.target.checked)} />
          Also delete its disk{owned.length > 1 ? "s" : ""}: <span className="mono">{owned.map((k) => k.source).join(", ")}</span>
        </label>
      )}
      {d.replication && (
        <label className="check">
          <input type="checkbox" checked={replica} onChange={(e) => setReplica(e.target.checked)} />
          Also delete the replica on {d.replication.targetName}
        </label>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button autoFocus onClick={props.onClose}>Cancel</button>
        <button className="danger" disabled={act.busy} onClick={async () => {
            let left: string[] = [];
            let note = "";
            const q = [...(disks ? ["disks=1"] : []), ...(d.replication && replica ? ["replica=1"] : [])].join("&");
            const ok = await act.run(async () => {
              const r = await del<{ leftDisks: string[]; replicaNote?: string }>(`${props.base}${q ? "?" + q : ""}`);
              left = r.leftDisks ?? [];
              note = r.replicaNote ?? "";
            });
            if (!ok) return;
            if (left.length || note) {
              setLeftDisks(left);
              setReplicaNote(note || undefined);
            } else props.onDeleted();
          }}>
          Delete VM
        </button>
      </footer>
    </Modal>
  );
}
