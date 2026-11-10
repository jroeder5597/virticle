import { FormEvent, useCallback, useEffect, useState } from "react";
import { del, get, post, put, Role, User } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";

const roles: Role[] = ["admin", "operator", "viewer"];
const when = (unix: number) => new Date(unix * 1000).toLocaleDateString();

export default function Users() {
  const [users, setUsers] = useState<User[] | undefined>();
  const [loadError, setLoadError] = useState<string | undefined>();
  const [adding, setAdding] = useState(false);
  const [resetting, setResetting] = useState<User | undefined>();
  const act = useAction();
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
      <table>
        <thead>
          <tr>
            <th>User</th>
            <th>Role</th>
            <th>State</th>
            <th>Created</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(users ?? []).map((u) => (
            <tr key={u.id}>
              <td>{u.username}</td>
              <td>
                <select value={u.role} disabled={act.busy} onChange={(e) => change(u, { role: e.target.value })} aria-label={`Role of ${u.username}`}>
                  {roles.map((r) => (
                    <option key={r}>{r}</option>
                  ))}
                </select>
              </td>
              <td>{u.disabled ? <span className="badge s-shutoff">disabled</span> : <span className="badge s-running">active</span>}</td>
              <td>{when(u.created)}</td>
              <td className="actions">
                <button disabled={act.busy} onClick={() => change(u, { disabled: !u.disabled })}>
                  {u.disabled ? "Enable" : "Disable"}
                </button>
                <button disabled={act.busy} onClick={() => setResetting(u)}>
                  Reset password
                </button>
                <button className="danger" disabled={act.busy} onClick={() => window.confirm(`Delete user ${u.username}? Their API tokens stop working.`) && act.run(() => del(`/users/${u.id}`)).then(reload)}>
                  Delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
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
          <select value={role} onChange={(e) => setRole(e.target.value as Role)}>
            {roles.map((r) => (
              <option key={r}>{r}</option>
            ))}
          </select>
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
