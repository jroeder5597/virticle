import Select from "../Select";
import { useEffect, useState } from "react";
import { post } from "../api";
import { ErrorBox, Field, useAction } from "../ui";
import { CloudProfile, profileUrl, useCloudProfiles } from "./CloudProfiles";

/** Cloud-init settings as the create-VM API takes them. */
export interface CloudInitValues {
  hostname: string;
  user: string;
  password: string;
  sshKeys: string;
  packages: string;
  rawUserData: string;
  /** Saved profile id, 0 for none. Empty fields above inherit from it. */
  profile: number;
}

export const emptyCloudInit: CloudInitValues = { hostname: "", user: "", password: "", sshKeys: "", packages: "", rawUserData: "", profile: 0 };

/** True when the user filled in anything beyond the defaults. */
export function cloudInitUsed(v: CloudInitValues): boolean {
  return !!(v.profile || v.user || v.password || v.sshKeys.trim() || v.packages.trim() || v.rawUserData.trim() || v.hostname);
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
    profile: v.profile || undefined,
  };
}

/** Collapsible cloud-init section for the create-VM and clone dialogs. */
export default function CloudInitFields(props: { vmName: string; value: CloudInitValues; onChange: (v: CloudInitValues) => void; profiles?: boolean; canSave?: boolean; preselect?: boolean }) {
  const { value: v, onChange } = props;
  const usable = props.profiles !== false;
  const [open, setOpen] = useState(false);
  const { profiles, reload } = useCloudProfiles(usable);
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
      <p className="hint">Provisions the first boot of a cloud image through a small seed disk. The image must have cloud-init installed.</p>
      {usable && profiles && profiles.length > 0 && (
        <Field label="Profile" hint="Fields you leave empty are taken from the profile. SSH keys and packages you enter are added to its.">
          <Select label="Profile" value={String(v.profile)} onChange={(x) => onChange({ ...v, profile: Number(x) })} options={[{ value: "0", label: "None" }, ...profiles.map((p) => ({ value: String(p.id), label: p.name + (p.default ? " (default)" : "") }))]} />
        </Field>
      )}
      {prof?.rawUserData.trim() && <p className="hint">The profile's raw user-data replaces the generated config; the fields below are ignored unless you enter your own raw user-data.</p>}
      <div className="grid2">
        <Field label="Hostname" hint="Defaults to the VM name.">
          <input value={v.hostname} onChange={set("hostname")} placeholder={props.vmName} disabled={raw} />
        </Field>
        <Field label="User">
          <input value={v.user} onChange={set("user")} placeholder={prof?.user || "e.g. admin"} pattern="[a-z_][a-z0-9_\-]*" disabled={raw} autoComplete="off" />
        </Field>
        <Field label="Password" hint="Stored on the seed disk as a SHA-512 hash.">
          <input type="password" value={v.password} onChange={set("password")} placeholder={prof?.hasPassword ? "(from the profile)" : ""} disabled={raw} autoComplete="new-password" />
        </Field>
        <Field label="Packages" hint="Separated by spaces or commas.">
          <input value={v.packages} onChange={set("packages")} placeholder={prof?.packages.length ? prof.packages.join(" ") : "qemu-guest-agent"} disabled={raw} />
        </Field>
      </div>
      <Field label="SSH public keys" hint="One per line. Added to the user above.">
        <textarea rows={3} value={v.sshKeys} onChange={set("sshKeys")} placeholder={keyHint} disabled={raw} spellCheck={false} />
      </Field>
      <Field label="Advanced: raw user-data" hint="If set, used as is and the fields above are ignored. Start with #cloud-config or a #! script.">
        <textarea rows={5} value={v.rawUserData} onChange={set("rawUserData")} spellCheck={false} />
      </Field>
      {props.canSave && usable && <SaveAsProfile v={v} onSaved={(id) => reload().then(() => onChange({ ...emptyCloudInit, hostname: v.hostname, profile: id }))} />}
    </details>
  );
}

/** Small inline form that stores the dialog's current settings as a new profile. */
function SaveAsProfile({ v, onSaved }: { v: CloudInitValues; onSaved: (id: number) => void }) {
  const [name, setName] = useState("");
  const [shown, setShown] = useState(false);
  const act = useAction();
  const lines = (s: string, sep: RegExp) => s.split(sep).map((x) => x.trim()).filter(Boolean);
  if (!shown) {
    return (
      <button type="button" className="link" onClick={() => setShown(true)}>
        Save these settings as a profile
      </button>
    );
  }
  const save = async () => {
    let id = 0;
    const ok = await act.run(async () => {
      const r = await post<CloudProfile>(profileUrl, {
        name,
        user: v.user,
        password: v.password,
        sshKeys: lines(v.sshKeys, /\n/),
        packages: lines(v.packages, /[\s,]+/),
        rawUserData: v.rawUserData,
      });
      id = r.id;
    });
    if (ok) onSaved(id);
  };
  return (
    <div className="field">
      <span>Profile name</span>
      <div className="actions">
        <input value={name} onChange={(e) => setName(e.target.value)} maxLength={64} />
        <button type="button" className="primary" disabled={act.busy || !name.trim()} onClick={save}>
          Save profile
        </button>
        <button type="button" onClick={() => setShown(false)}>
          Cancel
        </button>
      </div>
      <small>The values above (without the hostname) are saved. The password is stored as a hash.</small>
      <ErrorBox error={act.error} />
    </div>
  );
}
