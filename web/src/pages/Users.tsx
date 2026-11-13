import { FormEvent, useCallback, useEffect, useState } from "react";
import { del, get, post, put, Role, User } from "../api";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";

const roles: Role[] = ["admin", "operator", "viewer"];
const when = (unix: number) => new Date(unix * 1000).toLocaleDateString();

export default function Users() {
  const [users, setUsers] = useState<User[] | undefined>();
  const [loadError, setLoadError] = useState<string | undefined>();
  const [adding, setAdding] = useState(false);
  const [resetting, setResetting] = useState<User | undefined>();
  const sources = { local: "Local", oidc: "Identity provider", ldap: "Directory" };
  const act = useAction();
  const confirm = useConfirm();
  const reload = useCallback(() => get<User[]>("/users").then(setUsers, (e) => setLoadError(String(e.message ?? e))), []);
  useEffect(() => {
    reload();
  }, [reload]);
  const change = (u: User, body: object) => act.run(() => put(`/users/${u.id}`, body)).then(reload);

  return (
    <>
      <div className="head">
        <h1>Users</h1>
        <button className="primary" onClick={() => setAdding(true)}>
          Add user
        </button>
      </div>
      <p className="muted">
        Administrators manage everything, including users and nodes. Operators manage virtual machines, storage and networks. Viewers can look but not change anything, and cannot open consoles.
      </p>
      <ErrorBox error={loadError ?? act.error} />
      <table className="users">
        <thead>
          <tr>
            <th>User</th>
            <th>Source</th>
            <th>Role</th>
            <th>Two-step</th>
            <th>State</th>
            <th>Created</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(users ?? []).map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>{sources[u.source] ?? u.source}</td>
              <td>
                {u.source === "local" ? (
                  <Select auto label={`Role of ${u.username}`} value={u.role} disabled={act.busy} onChange={(r) => change(u, { role: r })} options={roles.map((r) => ({ value: r, label: r }))} />
                ) : (
                  <span title="Set by the user's groups at each sign-in">{u.role}</span>
                )}
              </td>
              <td>{u.source === "oidc" ? <span className="muted">Provider</span> : u.twoStep ? <span className="badge s-running">On</span> : <span className="badge s-shutoff">Off</span>}</td>
              <td>{u.disabled ? <span className="badge s-shutoff">Disabled</span> : <span className="badge s-running">Active</span>}</td>
              <td>{when(u.created)}</td>
              <td className="actions">
                <button disabled={act.busy} onClick={() => change(u, { disabled: !u.disabled })}>
                  {u.disabled ? "Enable" : "Disable"}
                </button>
                {u.source === "local" && (
                  <button disabled={act.busy} onClick={() => setResetting(u)}>
                    Reset password
                  </button>
                )}
                {u.twoStep && (
                  <button disabled={act.busy} onClick={async () => (await confirm.ask({ title: `Reset two-step sign-in of ${u.username}`, message: `Two-step sign-in is turned off for ${u.username}, who is signed out everywhere. Use this when the phone is lost. They can set it up again after signing in.`, confirm: "Reset two-step sign-in" })) && act.run(() => post(`/users/${u.id}/reset-2fa`)).then(reload)}>
                    Reset two-step sign-in
                  </button>
                )}
                <button className="danger" disabled={act.busy} onClick={async () => (await confirm.ask({ title: `Delete user ${u.username}`, message: `The user ${u.username} is deleted. Their API tokens stop working.`, confirm: "Delete user" })) && act.run(() => del(`/users/${u.id}`)).then(reload)}>
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {confirm.dialog}
      {adding && <AddUser onClose={() => setAdding(false)} onDone={reload} />}
      {resetting && <ResetPassword user={resetting} onClose={() => setResetting(undefined)} />}
    </>
  );
}

function AddUser({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("operator");
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await act.run(() => post("/users", { username, password, role }))) {
      onDone();
      onClose();
    }
  };
  return (
    <Modal title="Add user" onClose={onClose}>
      <form onSubmit={submit}>
        <Field label="Username" hint="Letters, digits and . _ @ -">
          <input value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus autoComplete="off" />
        </Field>
        <Field label="Password" hint="At least 10 characters. The user can change it under My account.">
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="new-password" />
        </Field>
        <Field label="Role">
          <Select label="Role" value={role} onChange={(r) => setRole(r as Role)} options={roles.map((r) => ({ value: r, label: r }))} />
        </Field>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Add
          </button>
        </footer>
      </form>
    </Modal>
  );
}

function ResetPassword({ user, onClose }: { user: User; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await act.run(() => put(`/users/${user.id}`, { password }))) onClose();
  };
  return (
    <Modal title={`Reset password of ${user.username}`} onClose={onClose}>
      <form onSubmit={submit}>
        <p className="muted">All of this user's signed-in sessions end.</p>
        <Field label="New password" hint="At least 10 characters.">
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus autoComplete="new-password" />
        </Field>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Reset
          </button>
        </footer>
      </form>
    </Modal>
  );
}
