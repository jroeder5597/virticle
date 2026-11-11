import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { get, post, put, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import CloudInitFields, { cloudInitPayload, cloudInitUsed, CloudInitValues, emptyCloudInit } from "./CloudInit";

interface CloneInfo {
  template: boolean;
  sealed: string;
  guest: "linux" | "windows";
  virtSysprep: boolean;
  sysprep?: string;
  sysprepProblem?: string;
}

/** The "Clone" button and its dialog. */
export function CloneButton({ d, base }: { d: VMDetail; base: string }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button disabled={d.state !== "shutoff"} title={d.state !== "shutoff" ? "Shut the VM down to clone it" : undefined} onClick={() => setOpen(true)}>
        Clone
      </button>
      {open && <CloneDialog d={d} base={base} onClose={() => setOpen(false)} />}
    </>
  );
}

function CloneDialog({ d, base, onClose }: { d: VMDetail; base: string; onClose: () => void }) {
  const nav = useNavigate();
  const act = useAction();
  const [info, setInfo] = useState<CloneInfo>();
  const [infoError, setInfoError] = useState<string>();
  const [name, setName] = useState(d.name + "-clone");
  const [linked, setLinked] = useState(false);
  const [start, setStart] = useState(false);
  const [sysprep, setSysprep] = useState(false);
  const [ci, setCi] = useState<CloudInitValues>(emptyCloudInit);
  const [result, setResult] = useState<string>();

  useEffect(() => {
    get<CloneInfo>(`${base}/clone-info`).then(setInfo, (e) => setInfoError(String(e.message ?? e)));
  }, [base]);

  const windows = info?.guest === "windows";
  // Profiles are Linux-only; for Windows the select is not offered.
  const customize = cloudInitUsed(windows ? { ...ci, profile: 0 } : ci);
  const submit = async () => {
    let uuid = "";
    let note = "";
    const ok = await act.run(async () => {
      const p = cloudInitPayload(ci, name);
      const body = {
        name,
        linked,
        start,
        sysprep,
        customize: customize ? { hostname: p.hostname, user: p.user, password: p.password, sshKeys: p.sshKeys, packages: p.packages, rawUserData: p.rawUserData, profile: windows ? undefined : p.profile } : undefined,
      };
      const r = await post<{ uuid: string; sysprep: string; startError?: string }>(`${base}/clone`, body);
      uuid = r.uuid;
      note = [r.sysprep && `Identity reset by: ${r.sysprep}.`, r.startError && `It was created but did not start: ${r.startError}`].filter(Boolean).join(" ");
    });
    if (!ok) return;
    if (note) setResult(note + "\n\nOpening the new VM.");
    setTimeout(() => nav(`/vm/${d.node}/${uuid}`), note ? 2500 : 0);
  };

  return (
    <Modal title={`Clone ${d.name}`} onClose={onClose}>
      <Field label="Name of the clone">
        <input value={name} onChange={(e) => setName(e.target.value)} autoFocus />
      </Field>
      <div className="field">
        <span>Disks</span>
        <label className="check">
          <input type="radio" name="clonemode" checked={!linked} onChange={() => setLinked(false)} />
          Full clone: independent copy
        </label>
        <label className="check">
          <input type="radio" name="clonemode" checked={linked} disabled={!d.template} onChange={() => setLinked(true)} />
          Linked clone: fast, small, needs the template
        </label>
        <small>{d.template ? "A linked clone stores only its changes and keeps reading the template's disks, so the template cannot be deleted while it exists." : "Linked clones need a template as the source, so its disks can no longer change. Use \"Convert to template\" first."}</small>
      </div>
      <label className="check">
        <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />
        Start the clone when it is ready
      </label>
      <div className="field">
        <label className="check">
          <input type="checkbox" checked={sysprep} disabled={!info || !!info.sysprepProblem} onChange={(e) => setSysprep(e.target.checked)} />
          Sysprep: give the clone a new identity
          {info?.sysprep && <span className="badge">via {info.sysprep}</span>}
        </label>
        {info?.sysprep === "virt-sysprep" && (
          <small>virt-sysprep runs on the clone's disks on the node (machine-id, SSH host keys, logs, DHCP leases and MAC rules are reset). It works on any Linux image.</small>
        )}
        {info?.sysprep === "sealed template" && (
          <small>
            The template was sealed
            {windows ? " with sysprep /generalize: Windows creates a new SID on the clone's first boot." : ": the clone creates a new machine-id and SSH host keys on its first boot."}
          </small>
        )}
        {info?.sysprepProblem && <small>{info.sysprepProblem}</small>}
        {!info && !infoError && <small>Checking what the node supports</small>}
        {infoError && <small>{infoError}</small>}
      </div>
      <CloudInitFields vmName={name} value={ci} onChange={setCi} profiles={!!info && !windows} />
      <p className="hint muted">
        {windows
          ? "Credentials are injected through an unattend.xml on a small ISO (computer name, local administrator and password; no SSH keys or packages). Needs a generalized (sealed) Windows guest."
          : "Credentials are injected with a fresh cloud-init seed, so the guest must have cloud-init installed. Without credentials the clone gets no seed disk."}
      </p>
      <ErrorBox error={act.error} />
      {result && <p className="note">{result}</p>}
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || !name} onClick={submit}>
          {act.busy ? "Cloning" : "Clone"}
        </button>
      </footer>
    </Modal>
  );
}

/** Template conversion buttons for the VM header. */
export function TemplateActions({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const act = useAction();
  const [sealing, setSealing] = useState(false);
  const set = (template: boolean) => act.run(async () => { await put(`${base}/template`, { template }); onChanged(); });
  return (
    <>
      {d.state === "shutoff" && !d.template && (
        <button disabled={act.busy} onClick={() => set(true)} title="Mark as a template: it can be cloned (also linked) but not started">
          Convert to template
        </button>
      )}
      {d.state === "shutoff" && d.template && (
        <button disabled={act.busy} onClick={() => set(false)}>
          Convert back to VM
        </button>
      )}
      {d.state === "running" && d.agent === "connected" && !d.template && (
        <button disabled={act.busy} onClick={() => setSealing(true)}>
          Seal and convert to template
        </button>
      )}
      {act.error && <span className="error">{act.error}</span>}
      {sealing && <SealDialog d={d} base={base} onClose={() => setSealing(false)} onDone={onChanged} />}
    </>
  );
}

function SealDialog({ d, base, onClose, onDone }: { d: VMDetail; base: string; onClose: () => void; onDone: () => void }) {
  const act = useAction();
  return (
    <Modal title={`Seal ${d.name}`} onClose={onClose}>
      <p>
        This runs a script inside the guest through the guest agent, shuts the guest down and marks the VM as a template. Clones of it then get a new identity on their
        first boot.
      </p>
      <ul>
        <li>Linux: clears the machine-id, deletes the SSH host keys, runs <code>cloud-init clean</code> if present, removes DHCP leases, logs and shell history.</li>
        <li>Windows: runs <code>sysprep /generalize /oobe /shutdown</code>, which takes minutes and cannot be undone for this installation.</li>
      </ul>
      <p className="note">The guest is no longer usable as a normal machine afterwards. Seal a copy if you want to keep this one.</p>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="danger"
          disabled={act.busy}
          onClick={async () => {
            if (await act.run(() => post(`${base}/seal`))) {
              onDone();
              onClose();
            }
          }}
        >
          {act.busy ? "Sealing" : "Seal and convert"}
        </button>
      </footer>
    </Modal>
  );
}
