import { FormEvent, useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ApiToken, del, get, post, Role } from "../api";
import { atLeast, useRole } from "../auth";
import { ErrorBox, Field, useAction } from "../ui";
import { ConsolePrefCard } from "./ConsolePreview";

const when = (unix?: number) => (unix ? new Date(unix * 1000).toLocaleString() : "");

export default function Account() {
  const role = useRole();
  return (
    <>
      <div className="head">
        <h1>My account</h1>
        <span className="badge">{role}</span>
      </div>
      <div className="cards">
        <Password />
        <Tokens role={role} />
        {atLeast(role, "operator") && (
          <div className="card">
            <h2>Cloud-init profiles</h2>
            <p className="muted">Saved user, password, SSH keys and packages for new and cloned VMs.</p>
            <Link to="/cloudinit-profiles">Manage profiles</Link>
          </div>
        )}
        <ConsolePrefCard />
      </div>
    </>
  );
}

function Password() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [done, setDone] = useState(false);
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setDone(false);
    if (await act.run(() => post("/account/password", { current, new: next }))) {
      setDone(true);
      setCurrent("");
      setNext("");
    }
  };
  return (
    <form className="card" onSubmit={submit}>
      <h2>Change password</h2>
      <p className="muted">Your other signed-in sessions end when you change it.</p>
      <Field label="Current password">
        <input type="password" value={current} onChange={(e) => setCurrent(e.target.value)} required autoComplete="current-password" />
      </Field>
      <Field label="New password" hint="At least 10 characters.">
        <input type="password" value={next} onChange={(e) => setNext(e.target.value)} required autoComplete="new-password" />
      </Field>
      <ErrorBox error={act.error} />
      {done && <p>Password changed.</p>}
      <button className="primary" disabled={act.busy}>
        Change password
      </button>
    </form>
  );
}

function Tokens({ role }: { role: Role }) {
  const [tokens, setTokens] = useState<ApiToken[]>([]);
  const [name, setName] = useState("");
  const [tokRole, setTokRole] = useState<Role>(role);
  const [days, setDays] = useState("90");
  const [secret, setSecret] = useState<string | undefined>();
  const [copied, setCopied] = useState(false);
  const act = useAction();
  const reload = useCallback(() => get<ApiToken[]>("/account/tokens").then(setTokens, () => {}), []);
  useEffect(() => {
    reload();
  }, [reload]);
  const create = async (e: FormEvent) => {
    e.preventDefault();
    setSecret(undefined);
    setCopied(false);
    let made = "";
    if (await act.run(async () => (made = (await post<{ secret: string }>("/account/tokens", { name, role: tokRole, expiresDays: Number(days) || 0 })).secret))) {
      setSecret(made);
      setName("");
      reload();
    }
  };
  return (
    <div className="card">
      <h2>API tokens</h2>
      <p className="muted">
        Send a token as <code>Authorization: Bearer TOKEN</code>. A token has your role or a lower one and works until it expires or you revoke it. It cannot manage tokens or your password.
      </p>
      {secret && (
        <div className="secretbox">
          <div>
            <strong>Copy this token now; it is not shown again.</strong>
            <div>
              <code>{secret}</code>
            </div>
          </div>
          <button type="button" onClick={() => navigator.clipboard?.writeText(secret).then(() => setCopied(true))}>
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
      )}
      {tokens.length > 0 && (
        <table className="compact">
          <thead>
            <tr>
              <th>Name</th>
              <th>Role</th>
              <th>Expires</th>
              <th>Last used</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {tokens.map((t) => (
              <tr key={t.id}>
                <td>{t.name}</td>
                <td>{t.role}</td>
                <td>{t.expires ? when(t.expires) : "never"}</td>
                <td>{when(t.lastUsed) || "never"}</td>
                <td className="actions">
                  <button className="danger" disabled={act.busy} onClick={() => window.confirm(`Revoke token ${t.name}?`) && act.run(() => del(`/account/tokens/${t.id}`)).then(reload)}>
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form onSubmit={create}>
        <h3>New token</h3>
        <div className="row">
          <Field label="Name">
            <input value={name} onChange={(e) => setName(e.target.value)} required placeholder="ci-pipeline" />
          </Field>
          <Field label="Role">
            <select value={tokRole} onChange={(e) => setTokRole(e.target.value as Role)}>
              {(["admin", "operator", "viewer"] as Role[]).filter((r) => atLeast(role, r)).map((r) => (
                <option key={r}>{r}</option>
              ))}
            </select>
          </Field>
          <Field label="Expires in (days)" hint="0 = never">
            <input type="number" min={0} max={3650} value={days} onChange={(e) => setDays(e.target.value)} />
          </Field>
        </div>
        <ErrorBox error={act.error} />
        <button className="primary" disabled={act.busy}>
          Create token
        </button>
      </form>
    </div>
  );
}
