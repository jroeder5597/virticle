import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { get, post, put, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import CloudInitFields, { cloudInitPayload, cloudInitUsed, CloudInitValues, emptyCloudInit } from "./CloudInit";
import DialogAdvanced from "./DialogAdvanced";
import { StatusLine } from "../Checks";

interface CloneInfo {
  template: boolean;
  sealed: string;
  guest: "linux" | "windows";
  virtSysprep: boolean;
  sysprep?: string;
  sysprepProblem?: string;
}

/**
 * A template is cloned linked unless "Full copy" is ticked in Advanced; any other VM is always a full copy.
 * A clone of a template gets a new identity when the node can do it; Advanced has the opt-out.
 */
export function CloneDialog({ d, base, onClose }: { d: VMDetail; base: string; onClose: () => void }) {
  const nav = useNavigate();
  const act = useAction();
  const [info, setInfo] = useState<CloneInfo>();
  const [infoError, setInfoError] = useState<string>();
  const [name, setName] = useState(d.name + "-clone");
  const [fullCopy, setFullCopy] = useState(false);
  const [start, setStart] = useState(false);
  // undefined: not touched, so the default applies (on for a template when the node can do it).
  const [sysprepChoice, setSysprepChoice] = useState<boolean>();
  const [ci, setCi] = useState<CloudInitValues>(emptyCloudInit);
  const [result, setResult] = useState<string>();

  useEffect(() => {
    get<CloneInfo>(`${base}/clone-info`).then(setInfo, (e) => setInfoError(String(e.message ?? e)));
  }, [base]);

  const windows = info?.guest === "windows";
  const linked = d.template && !fullCopy;
  const canSysprep = !!info && !info.sysprepProblem;
  const sysprep = canSysprep && (sysprepChoice ?? d.template);
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
      {d.template && <p className="muted">{linked ? "The clone is fast and small, and the template cannot be deleted while it exists." : "The clone is an independent copy of the template's disks."}</p>}
      <label className="check">
        <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} />
        Start the clone when it is ready
      </label>
      {d.template && info?.sysprepProblem && <p className="muted">{info.sysprepProblem}</p>}
      {!info && !infoError && <p className="muted">Checking what the node supports</p>}
      {infoError && <p className="muted">{infoError}</p>}
      <CloudInitFields value={ci} onChange={setCi} profiles={!!info && !windows} />
      <DialogAdvanced>
        {d.template && (
          <label className="check">
            <input type="checkbox" checked={fullCopy} onChange={(e) => setFullCopy(e.target.checked)} />
            Full copy
          </label>
        )}
        <div className="field">
          <label className="check">
            <input type="checkbox" checked={sysprep} disabled={!canSysprep} onChange={(e) => setSysprepChoice(e.target.checked)} />
            Give the clone a new identity
            {info?.sysprep && <span className="badge">via {info.sysprep}</span>}
          </label>
          {!d.template && info?.sysprepProblem && <small>{info.sysprepProblem}</small>}
        </div>
      </DialogAdvanced>
      <ErrorBox error={act.error} />
      {result && <StatusLine status="info">{result}</StatusLine>}
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || !name} onClick={submit}>
          {act.busy ? "Cloning" : "Clone"}
        </button>
      </footer>
    </Modal>
  );
}

/**
 * Convert to template. A running VM whose guest agent answers can also have its identity reset (default on): the
 * guest is sealed, shut down and marked as a template in one go. A VM that is off is only marked.
 */
export function ConvertDialog({ d, base, onClose, onDone }: { d: VMDetail; base: string; onClose: () => void; onDone: () => void }) {
  const act = useAction();
  const canReset = d.state === "running" && d.agent === "connected";
  const [reset, setReset] = useState(true);
  const seal = canReset && reset;
  const blocked = d.state !== "shutoff" && !seal;
  return (
    <Modal title={`Convert ${d.name} to a template`} onClose={onClose}>
      <p>A template can be cloned but not started.</p>
      {canReset && (
        <label className="check">
          <input type="checkbox" checked={reset} onChange={(e) => setReset(e.target.checked)} />
          Also reset its identity
        </label>
      )}
      {seal && <StatusLine status="warning">This shuts the VM down and cannot be undone for this guest; reset a copy to keep it usable.</StatusLine>}
      {blocked && <p className="muted">Shut the VM down first to convert it without resetting its identity.</p>}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className={seal ? "danger" : "primary"}
          disabled={act.busy || blocked}
          onClick={async () => {
            if (await act.run(() => (seal ? post(`${base}/seal`) : put(`${base}/template`, { template: true })))) {
              onDone();
              onClose();
            }
          }}
        >
          {act.busy ? "Converting" : "Convert to template"}
        </button>
      </footer>
    </Modal>
  );
}
