import Select from "../Select";
import { useEffect, useState } from "react";
import { Field } from "../ui";
import { CloudProfile, useCloudProfiles } from "./CloudProfiles";

/** Cloud-init settings as the create-VM API takes them. The host name is always the VM's name. */
export interface CloudInitValues {
  user: string;
  password: string;
  sshKeys: string;
  packages: string;
  rawUserData: string;
  /** Saved profile id, 0 for none. Empty fields above inherit from it. */
  profile: number;
}

export const emptyCloudInit: CloudInitValues = { user: "", password: "", sshKeys: "", packages: "", rawUserData: "", profile: 0 };

/** True when the user filled in anything beyond the defaults. */
export function cloudInitUsed(v: CloudInitValues): boolean {
  return !!(v.profile || v.user || v.password || v.sshKeys.trim() || v.packages.trim() || v.rawUserData.trim());
}

export function cloudInitPayload(v: CloudInitValues, vmName: string) {
  const lines = (s: string) => s.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean);
  return {
    hostname: vmName,
    user: v.user,
    password: v.password,
    sshKeys: v.sshKeys.split("\n").map((x) => x.trim()).filter(Boolean),
    packages: lines(v.packages.replace(/\s+/g, ",")),
    rawUserData: v.rawUserData,
    profile: v.profile || undefined,
  };
}

/** Collapsible cloud-init section for the create-VM and clone dialogs. */
export default function CloudInitFields(props: { value: CloudInitValues; onChange: (v: CloudInitValues) => void; profiles?: boolean; preselect?: boolean }) {
  const { value: v, onChange } = props;
  const usable = props.profiles !== false;
  const [open, setOpen] = useState(false);
  const { profiles } = useCloudProfiles(usable);
  const prof: CloudProfile | undefined = usable ? profiles?.find((p) => p.id === v.profile) : undefined;
  // Preselect the default profile once, when the list arrives.
  const [preselected, setPreselected] = useState(false);
  useEffect(() => {
    if (!usable || !profiles || preselected) return;
    setPreselected(true);
    const d = profiles.find((p) => p.default);
    // Only where asked: a clone keeps "no credentials, no seed disk" unless the user picks a profile.
    if (props.preselect && d && !v.profile) {
      onChange({ ...v, profile: d.id });
      setOpen(true);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [profiles, usable]);
  const set = (k: keyof CloudInitValues) => (e: { target: { value: string } }) => onChange({ ...v, [k]: e.target.value });
  const raw = v.rawUserData.trim() !== "" || !!prof?.rawUserData.trim();
  const keyHint = prof?.sshKeys.length ? `${prof.sshKeys.length} key${prof.sshKeys.length > 1 ? "s" : ""} from the profile are added` : "ssh-ed25519 AAAA me@host";
  return (
    <details className="cloudinit" open={open} onToggle={(e) => setOpen((e.target as HTMLDetailsElement).open)}>
      <summary>
        Cloud-init{cloudInitUsed(v) ? " (configured)" : ""}
        {prof ? `: ${prof.name}` : ""}
      </summary>
      <p className="hint">{usable ? "The image must have cloud-init installed. The guest agent is installed too." : "Sets the administrator account on the clone's first boot."}</p>
      {usable && profiles && profiles.length > 0 && (
        <Field label="Profile" hint="Fields you leave empty are taken from the profile.">
          <Select label="Profile" value={String(v.profile)} onChange={(x) => onChange({ ...v, profile: Number(x) })} options={[{ value: "0", label: "None" }, ...profiles.map((p) => ({ value: String(p.id), label: p.name + (p.default ? " (default)" : "") }))]} />
        </Field>
      )}
      {prof?.rawUserData.trim() && <p className="hint">The profile's raw user-data replaces the generated config.</p>}
      <div className="grid2">
        <Field label="User">
          <input value={v.user} onChange={set("user")} placeholder={prof?.user || "e.g. admin"} pattern="[a-z_][a-z0-9_\-]*" disabled={raw} autoComplete="off" />
        </Field>
        <Field label="Password">
          <input type="password" value={v.password} onChange={set("password")} placeholder={prof?.hasPassword ? "(from the profile)" : ""} disabled={raw} autoComplete="new-password" />
        </Field>
      </div>
      <Field label="SSH public keys" hint="One per line. Added to the user above.">
        <textarea rows={3} value={v.sshKeys} onChange={set("sshKeys")} placeholder={keyHint} disabled={raw} spellCheck={false} />
      </Field>
      <details className="cloudinit">
        <summary>Packages and raw user-data</summary>
        <Field label="Packages" hint="Separated by spaces or commas.">
          <input value={v.packages} onChange={set("packages")} placeholder={prof?.packages.length ? prof.packages.join(" ") : "e.g. htop curl"} disabled={raw} />
        </Field>
        <Field label="Raw user-data" hint="If set, used as it is and the fields above are ignored. Start with #cloud-config or a #! script.">
          <textarea rows={5} value={v.rawUserData} onChange={set("rawUserData")} spellCheck={false} />
        </Field>
      </details>
    </details>
  );
}
