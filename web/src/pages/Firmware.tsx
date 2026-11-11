import { useEffect, useState } from "react";
import { get, put, VMDetail } from "../api";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";

export interface FwCaps {
  uefi: boolean;
  secureBoot: boolean;
  tpm: boolean;
  tpmModels: string[];
}

export interface FwValues {
  osType: "linux" | "windows";
  firmware: "bios" | "uefi";
  secureBoot: boolean;
  tpm: boolean;
  diskBus: "virtio" | "sata";
  nicModel: "virtio" | "e1000e";
}

export const emptyFw: FwValues = { osType: "linux", firmware: "bios", secureBoot: false, tpm: false, diskBus: "virtio", nicModel: "virtio" };

/** What the form sends; the server fills in the same defaults, this keeps the UI honest about them. */
export const fwPayload = (v: FwValues) => ({ ...v });

/** Firmware, TPM and OS-type choices of the create form. Offers only what the node supports. */
export function FirmwareFields({ node, value, onChange }: { node: number; value: FwValues; onChange: (v: FwValues) => void }) {
  const [caps, setCaps] = useState<FwCaps | undefined>();
  const [error, setError] = useState<string | undefined>();
  useEffect(() => {
    let live = true;
    setCaps(undefined);
    get<FwCaps>(`/nodes/${node}/firmware-caps`)
      .then((c) => live && (setCaps(c), setError(undefined)))
      .catch((e) => live && setError(String(e.message ?? e)));
    return () => {
      live = false;
    };
  }, [node]);
  const set = (p: Partial<FwValues>) => onChange({ ...value, ...p });
  const win = value.osType === "windows";
  const setOS = (osType: FwValues["osType"]) => {
    if (osType === "windows") {
      // Windows 11 wants UEFI, Secure Boot and a TPM; SATA and e1000e install without extra drivers.
      const uefi = !!caps?.uefi;
      onChange({ osType, firmware: uefi ? "uefi" : "bios", secureBoot: uefi && !!caps?.secureBoot, tpm: !!caps?.tpm, diskBus: "sata", nicModel: "e1000e" });
    } else {
      onChange({ ...emptyFw, osType });
    }
  };
  return (
    <>
      <ErrorBox error={error} />
      <div className="grid2">
        <Field label="Guest OS type" hint={win ? "Hyper-V enlightenments and a local-time clock are added." : undefined}>
          <select value={value.osType} onChange={(e) => setOS(e.target.value as FwValues["osType"])}>
            <option value="linux">Linux / other</option>
            <option value="windows">Windows</option>
          </select>
        </Field>
        <Field label="Firmware">
          <select value={value.firmware} onChange={(e) => set({ firmware: e.target.value as FwValues["firmware"], secureBoot: e.target.value === "uefi" && value.secureBoot })}>
            <option value="bios">BIOS</option>
            <option value="uefi" disabled={caps && !caps.uefi}>
              UEFI{caps && !caps.uefi ? " (not available on this node)" : ""}
            </option>
          </select>
        </Field>
        <label className="check">
          <input type="checkbox" checked={value.secureBoot} disabled={value.firmware !== "uefi" || !caps?.secureBoot} onChange={(e) => set({ secureBoot: e.target.checked })} /> Secure Boot
          {caps && !caps.secureBoot && <small className="muted"> not available on this node</small>}
        </label>
        <label className="check">
          <input type="checkbox" checked={value.tpm} disabled={!caps?.tpm} onChange={(e) => set({ tpm: e.target.checked })} /> Virtual TPM 2.0
          {caps && !caps.tpm && <small className="muted"> needs swtpm on the node</small>}
        </label>
      </div>
      {win && (
        <>
          <div className="grid2">
            <Field label="Disk bus">
              <select value={value.diskBus} onChange={(e) => set({ diskBus: e.target.value as FwValues["diskBus"] })}>
                <option value="sata">SATA</option>
                <option value="virtio">VirtIO</option>
              </select>
            </Field>
            <Field label="Network card">
              <select value={value.nicModel} onChange={(e) => set({ nicModel: e.target.value as FwValues["nicModel"] })}>
                <option value="e1000e">Intel e1000e</option>
                <option value="virtio">VirtIO</option>
              </select>
            </Field>
          </div>
          {(value.diskBus === "virtio" || value.nicModel === "virtio") && (
            <p className="note">VirtIO devices need the virtio-win driver during Windows setup: attach the driver ISO as a second CD-ROM and load the storage driver when the installer asks for it. SATA and e1000e work without drivers.</p>
          )}
        </>
      )}
    </>
  );
}

/** Firmware, Secure Boot and TPM of an existing VM (Hardware tab). */
export default function FirmwareCard({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const act = useAction();
  const confirm = useConfirm();
  const [caps, setCaps] = useState<FwCaps | undefined>();
  const [outcome, setOutcome] = useState<string | undefined>();
  useEffect(() => {
    get<FwCaps>(`/nodes/${d.node}/firmware-caps`).then(setCaps).catch(() => {});
  }, [d.node]);
  const off = d.state === "shutoff";
  const fw = d.fw;
  const hasTPM = !!fw?.tpmModel || d.tpm;
  const toggle = (enabled: boolean) =>
    act.run(async () => {
      const r = await put<{ note?: string }>(`${base}/tpm`, { enabled });
      setOutcome(`TPM ${enabled ? "added" : "removed"}. ${r.note ?? ""}`.trim());
      onChanged();
    });
  return (
    <section className="card" id="hw-firmware">
      {confirm.dialog}
      <h3>Firmware and TPM</h3>
      <dl>
        <dt>Firmware</dt>
        <dd>{fw?.firmware === "uefi" ? "UEFI" : "BIOS"}</dd>
        <dt>Secure Boot</dt>
        <dd>{fw?.firmware !== "uefi" ? "n/a (BIOS)" : fw.secureBoot ? "enabled" : "disabled"}</dd>
        {fw?.nvram && (
          <>
            <dt>NVRAM</dt>
            <dd className="mono wrap">{fw.nvram}</dd>
          </>
        )}
        <dt>TPM</dt>
        <dd>{hasTPM ? `emulated ${fw?.tpmVersion ? "TPM " + fw.tpmVersion : "TPM"}${fw?.tpmModel ? ` (${fw.tpmModel})` : ""}` : "none"}</dd>
      </dl>
      <div className="actions" style={{ marginTop: 10 }}>
        {hasTPM ? (
          <button disabled={!off || act.busy} title={off ? undefined : "Shut the VM down first"} onClick={async () => (await confirm.ask({ title: "Remove TPM", message: `The TPM is removed from ${d.name}. Disk encryption keys sealed to it (BitLocker, LUKS+TPM) will not unlock.`, confirm: "Remove TPM" })) && toggle(false)}>
            Remove TPM
          </button>
        ) : (
          <button disabled={!off || act.busy || (caps && !caps.tpm)} title={off ? (caps && !caps.tpm ? "This node has no swtpm" : undefined) : "Shut the VM down first"} onClick={() => toggle(true)}>
            Add TPM
          </button>
        )}
      </div>
      <p className="muted" style={{ marginBottom: 0 }}>
        Switching between BIOS and UEFI is not offered for an existing VM: the guest's boot setup depends on it. Create a new VM with the other firmware and attach the disk instead.
      </p>
      {outcome && <p className="muted">{outcome}</p>}
      <ErrorBox error={act.error} />
    </section>
  );
}
