import { useEffect, useState } from "react";
import { get, put, VMDetail } from "../api";
import Select from "../Select";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";
import { OneLine } from "./Disclosure";

export interface FwCaps {
  uefi: boolean;
  secureBoot: boolean;
  tpm: boolean;
  tpmModels: string[];
}

/**
 * The firmware a Linux guest gets in the New VM dialog. The server has the same constant for requests that name
 * none (defaultLinuxFirmware in internal/api/vmcreate.go); to make UEFI the default, change both.
 */
export const defaultLinuxFirmware: "bios" | "uefi" = "uefi";

export interface FwValues {
  osType: "linux" | "windows";
  firmware: "bios" | "uefi";
  secureBoot: boolean;
  tpm: boolean;
  diskBus: "virtio" | "sata";
  nicModel: "virtio" | "e1000e";
}

export const emptyFw: FwValues = { osType: "linux", firmware: defaultLinuxFirmware, secureBoot: false, tpm: false, diskBus: "virtio", nicModel: "virtio" };

/** What the form sends; the server fills in the same defaults, this keeps the UI honest about them. */
export const fwPayload = (v: FwValues) => ({ ...v });

/** The node's firmware abilities; undefined while loading. */
export function useFwCaps(node: number) {
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
  return { caps, error };
}

/** The defaults that follow the guest OS type, limited to what the node can do. */
export function osDefaults(osType: FwValues["osType"], caps?: FwCaps): FwValues {
  if (osType === "windows") {
    // Windows 11 wants UEFI, Secure Boot and a TPM; SATA and e1000e install without extra drivers.
    const uefi = !!caps?.uefi;
    return { osType, firmware: uefi ? "uefi" : "bios", secureBoot: uefi && !!caps?.secureBoot, tpm: !!caps?.tpm, diskBus: "sata", nicModel: "e1000e" };
  }
  const uefi = defaultLinuxFirmware === "uefi" && (!caps || caps.uefi);
  return { ...emptyFw, osType, firmware: uefi ? "uefi" : "bios" };
}

/** A choice the node cannot honour is dropped once its abilities are known. */
export function limitToCaps(v: FwValues, caps: FwCaps): FwValues {
  const firmware = v.firmware === "uefi" && !caps.uefi ? "bios" : v.firmware;
  const secureBoot = v.secureBoot && firmware === "uefi" && caps.secureBoot;
  const tpm = v.tpm && caps.tpm;
  return firmware === v.firmware && secureBoot === v.secureBoot && tpm === v.tpm ? v : { ...v, firmware, secureBoot, tpm };
}

/** Guest OS type of the New VM dialog. It sets the defaults of everything in the dialog's Advanced section. */
export function OsTypeField({ value, caps, onChange }: { value: FwValues; caps?: FwCaps; onChange: (v: FwValues) => void }) {
  return (
    <Field label="Guest OS type">
      <Select
        label="Guest OS type"
        value={value.osType}
        onChange={(v) => onChange(osDefaults(v as FwValues["osType"], caps))}
        options={[
          { value: "linux", label: "Linux / other" },
          { value: "windows", label: "Windows" },
        ]}
      />
    </Field>
  );
}

/** Firmware, Secure Boot, TPM and (for Windows) disk bus and network card, for the dialog's Advanced section. */
export function FirmwareFields({ value, caps, onChange }: { value: FwValues; caps?: FwCaps; onChange: (v: FwValues) => void }) {
  const set = (p: Partial<FwValues>) => onChange({ ...value, ...p });
  const win = value.osType === "windows";
  return (
    <>
      <div className="grid2">
        <Field label="Firmware">
          <Select
            label="Firmware"
            value={value.firmware}
            onChange={(v) => set({ firmware: v as FwValues["firmware"], secureBoot: v === "uefi" && value.secureBoot })}
            options={[
              { value: "bios", label: "BIOS" },
              { value: "uefi", label: "UEFI" + (caps && !caps.uefi ? " (not available on this node)" : ""), disabled: !!caps && !caps.uefi },
            ]}
          />
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
              <Select
                label="Disk bus"
                value={value.diskBus}
                onChange={(v) => set({ diskBus: v as FwValues["diskBus"] })}
                options={[
                  { value: "sata", label: "SATA" },
                  { value: "virtio", label: "VirtIO" },
                ]}
              />
            </Field>
            <Field label="Network card">
              <Select
                label="Network card"
                value={value.nicModel}
                onChange={(v) => set({ nicModel: v as FwValues["nicModel"] })}
                options={[
                  { value: "e1000e", label: "Intel e1000e" },
                  { value: "virtio", label: "VirtIO" },
                ]}
              />
            </Field>
          </div>
          {(value.diskBus === "virtio" || value.nicModel === "virtio") && <p className="note">VirtIO devices need the virtio-win driver during Windows setup.</p>}
        </>
      )}
    </>
  );
}

/** Firmware, Secure Boot and TPM of an existing VM (Hardware tab). */
export default function FirmwareCard({ d, base, onChanged, bare }: { d: VMDetail; base: string; onChanged: () => void; bare?: boolean }) {
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
    <section className={bare ? "hwbare" : "card"} id={bare ? undefined : "hw-firmware"} aria-label={bare ? "Firmware and TPM" : undefined}>
      {confirm.dialog}
      {!bare && (
        <div className="chead">
          <h3>Firmware and TPM</h3>
        </div>
      )}
      <dl className="hwdl">
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
      <OneLine
        text="Firmware is fixed for an existing VM."
        summary="Why not"
        details="The guest's boot setup depends on it. Create a new VM with the other firmware and attach the disk."
      />
      {outcome && <p className="muted">{outcome}</p>}
      <ErrorBox error={act.error} />
    </section>
  );
}
