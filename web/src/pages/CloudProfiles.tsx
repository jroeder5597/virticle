import { KeyRound } from "lucide-react";
import { FormEvent, useCallback, useEffect, useState } from "react";
import { del, get, post, put } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";

/** A saved cloud-init profile as the API returns it: never a password, only whether there is one. */
export interface CloudProfile {
  id: number;
  name: string;
  description: string;
  user: string;
  hasPassword: boolean;
  sshKeys: string[];
  packages: string[];
  rawUserData: string;
  default: boolean;
}

export const profileUrl = "/cloudinit-profiles";

/** Loads the profiles once; `reload` fetches them again. Nothing polls. */
export function useCloudProfiles(enabled = true) {
  const [profiles, setProfiles] = useState<CloudProfile[] | undefined>();
  const [error, setError] = useState<string | undefined>();
  const reload = useCallback(
    () => get<{ profiles: CloudProfile[] }>(profileUrl).then((r) => (setProfiles(r.profiles), setError(undefined), r.profiles), (e) => (setError(String(e.message ?? e)), undefined)),
    [],
  );
  useEffect(() => {
    if (enabled) reload();
  }, [reload, enabled]);
  return { profiles, error, reload };
}

export default function CloudProfiles() {
  const { profiles, error, reload } = useCloudProfiles();
  const [editing, setEditing] = useState<CloudProfile | "new" | undefined>();
  const [deleting, setDeleting] = useState<CloudProfile | undefined>();
  return (
    <>
      <div className="head">
        <h1>Cloud-init profiles</h1>
        <button className="primary" onClick={() => setEditing("new")}>
          New profile
        </button>
      </div>
      <p className="muted">
        Save user, password, SSH keys and packages once and pick them when you create or clone a VM. Passwords are stored as hashes only. Editing or deleting a profile does not change VMs
        that were made from it.
      </p>
      <ErrorBox error={error} />
      {profiles && profiles.length === 0 && (
        <div className="empty">
          <KeyRound size={20} aria-hidden />
          <p>No profiles yet.</p>
        </div>
      )}
      {profiles && profiles.length > 0 && (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>User</th>
                <th>Password</th>
                <th>SSH keys</th>
                <th>Packages</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {profiles.map((p) => (
                <tr key={p.id}>
                  <td>
                    {p.name} {p.default && <span className="badge">default</span>}
                    {p.description && <div className="muted">{p.description}</div>}
                  </td>
                  <td className="mono">{p.rawUserData ? "(raw user-data)" : p.user || "-"}</td>
                  <td>{p.hasPassword ? "set" : "-"}</td>
                  <td>{p.sshKeys.length || "-"}</td>
                  <td className="mono">{p.packages.join(" ") || "-"}</td>
                  <td className="actions">
                    <button onClick={() => setEditing(p)}>Edit</button>
                    <button className="danger" onClick={() => setDeleting(p)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {editing && <ProfileDialog profile={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} onDone={reload} />}
      {deleting && <DeleteDialog profile={deleting} onClose={() => setDeleting(undefined)} onDone={reload} />}
    </>
  );
}

function DeleteDialog({ profile, onClose, onDone }: { profile: CloudProfile; onClose: () => void; onDone: () => void }) {
  const act = useAction();
  return (
    <Modal title={`Delete ${profile.name}`} onClose={onClose}>
      <p>VMs that were created or cloned from this profile keep their settings. Only the saved profile is removed.</p>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="danger"
          disabled={act.busy}
          onClick={async () => {
            if (await act.run(() => del(`${profileUrl}/${profile.id}`))) {
              onDone();
              onClose();
            }
          }}
        >
          Delete
        </button>
      </footer>
    </Modal>
  );
}

const lines = (s: string) => s.split("\n").map((x) => x.trim()).filter(Boolean);

function ProfileDialog({ profile, onClose, onDone }: { profile?: CloudProfile; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState(profile?.name ?? "");
  const [description, setDescription] = useState(profile?.description ?? "");
  const [user, setUser] = useState(profile?.user ?? "");
  const [password, setPassword] = useState("");
  const [clearPass, setClearPass] = useState(false);
  const [keys, setKeys] = useState((profile?.sshKeys ?? []).join("\n"));
  const [packages, setPackages] = useState((profile?.packages ?? []).join(" "));
  const [raw, setRaw] = useState(profile?.rawUserData ?? "");
  const [isDefault, setDefault] = useState(profile?.default ?? false);
  const act = useAction();
  const rawSet = raw.trim() !== "";
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body = {
      name,
      description,
      user,
      password,
      clearPassword: clearPass,
      sshKeys: lines(keys),
      packages: packages.split(/[\s,]+/).filter(Boolean),
      rawUserData: raw,
      default: isDefault,
    };
    if (await act.run(() => (profile ? put(`${profileUrl}/${profile.id}`, body) : post(profileUrl, body)))) {
      onDone();
      onClose();
    }
  };
  return (
    <Modal title={profile ? `Edit ${profile.name}` : "New cloud-init profile"} onClose={onClose} wide>
      <form onSubmit={submit}>
        <div className="grid2">
          <Field label="Name">
            <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus maxLength={64} />
          </Field>
          <Field label="Description">
            <input value={description} onChange={(e) => setDescription(e.target.value)} maxLength={500} />
          </Field>
          <Field label="Default user">
            <input value={user} onChange={(e) => setUser(e.target.value)} placeholder="e.g. admin" pattern="[a-z_][a-z0-9_\-]*" disabled={rawSet} autoComplete="off" />
          </Field>
          <Field label="Password" hint={profile?.hasPassword ? "A password is set. Type a new one to replace it." : "Stored as a SHA-512 hash; the plain text is not kept."}>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder={profile?.hasPassword && !clearPass ? "(unchanged)" : ""}
              disabled={rawSet || clearPass}
              autoComplete="new-password"
            />
          </Field>
        </div>
        {profile?.hasPassword && (
          <label className="check">
            <input type="checkbox" checked={clearPass} onChange={(e) => setClearPass(e.target.checked)} />
            Remove the stored password
          </label>
        )}
        <Field label="Packages" hint="Separated by spaces or commas.">
          <input value={packages} onChange={(e) => setPackages(e.target.value)} placeholder="e.g. htop curl" disabled={rawSet} />
        </Field>
        <Field label="SSH public keys" hint="One per line. Added to the user above.">
          <textarea rows={3} value={keys} onChange={(e) => setKeys(e.target.value)} placeholder="ssh-ed25519 AAAA me@host" disabled={rawSet} spellCheck={false} />
        </Field>
        <Field label="Advanced: raw user-data" hint="If set, used as is and the fields above are ignored. Start with #cloud-config or a #! script.">
          <textarea rows={4} value={raw} onChange={(e) => setRaw(e.target.value)} spellCheck={false} />
        </Field>
        <label className="check">
          <input type="checkbox" checked={isDefault} onChange={(e) => setDefault(e.target.checked)} />
          Default profile: preselected when creating or cloning a VM
        </label>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Save
          </button>
        </footer>
      </form>
    </Modal>
  );
}
