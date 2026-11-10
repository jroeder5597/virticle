import { useState } from "react";
import { Field } from "../ui";

/** Cloud-init settings as the create-VM API takes them. */
export interface CloudInitValues {
  hostname: string;
  user: string;
  password: string;
  sshKeys: string;
  packages: string;
  rawUserData: string;
}

export const emptyCloudInit: CloudInitValues = { hostname: "", user: "", password: "", sshKeys: "", packages: "", rawUserData: "" };

/** True when the user filled in anything beyond the defaults. */
export function cloudInitUsed(v: CloudInitValues): boolean {
  return !!(v.user || v.password || v.sshKeys.trim() || v.packages.trim() || v.rawUserData.trim() || v.hostname);
}

export function cloudInitPayload(v: CloudInitValues, vmName: string) {
  const lines = (s: string) => s.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
  return {
    hostname: v.hostname || vmName,
    user: v.user,
    password: v.password,
    sshKeys: v.sshKeys.split("\n").map((x) => x.trim()).filter(Boolean),
    packages: lines(v.packages.replace(/\s+/g, ",")),
    rawUserData: v.rawUserData,
  };
}

/** Collapsible cloud-init section for the create-VM dialog. */
export default function CloudInitFields(props: { vmName: string; value: CloudInitValues; onChange: (v: CloudInitValues) => void }) {
  const { value: v, onChange } = props;
  const [open, setOpen] = useState(false);
  const set = (k: keyof CloudInitValues) => (e: { target: { value: string } }) => onChange({ ...v, [k]: e.target.value });
  const raw = v.rawUserData.trim() !== "";
  return (
    <details className="cloudinit" open={open} onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary>Cloud-init{cloudInitUsed(v) ? " (configured)" : ""}</summary>
      <p className="hint">Provisions the first boot of a cloud image through a small seed disk. The image must have cloud-init installed.</p>
      <div className="grid2">
        <Field label="Hostname" hint="Defaults to the VM name.">
          <input value={v.hostname} onChange={set("hostname")} placeholder={props.vmName} disabled={raw} />
        </Field>
        <Field label="User">
          <input value={v.user} onChange={set("user")} placeholder="e.g. admin" pattern="[a-z_][a-z0-9_\-]*" disabled={raw} autoComplete="off" />
        </Field>
        <Field label="Password" hint="Stored on the seed disk as a SHA-512 hash.">
          <input type="password" value={v.password} onChange={set("password")} disabled={raw} autoComplete="new-password" />
        </Field>
        <Field label="Packages" hint="Separated by spaces or commas.">
          <input value={v.packages} onChange={set("packages")} placeholder="qemu-guest-agent" disabled={raw} />
        </Field>
      </div>
      <Field label="SSH public keys" hint="One per line. Added to the user above.">
        <textarea rows={3} value={v.sshKeys} onChange={set("sshKeys")} placeholder="ssh-ed25519 AAAA… me@host" disabled={raw} spellCheck={false} />
      </Field>
      <Field label="Advanced: raw user-data" hint="If set, used as is and the fields above are ignored. Start with #cloud-config or a #! script.">
        <textarea rows={5} value={v.rawUserData} onChange={set("rawUserData")} spellCheck={false} />
      </Field>
    </details>
  );
}
