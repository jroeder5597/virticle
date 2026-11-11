import { FormEvent, useCallback, useEffect, useState } from "react";
import { AuditEntry, get } from "../api";
import Select from "../Select";
import { ErrorBox } from "../ui";

const when = (unix: number) => new Date(unix * 1000).toLocaleString();

export default function Audit() {
  const [user, setUser] = useState("");
  const [method, setMethod] = useState("");
  const [path, setPath] = useState("");
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [next, setNext] = useState(0);
  const [error, setError] = useState<string | undefined>();

  const load = useCallback(
    async (before: number) => {
      const q = new URLSearchParams({ limit: "100" });
      if (user) q.set("user", user);
      if (method) q.set("method", method);
      if (path) q.set("path", path);
      if (before) q.set("before", String(before));
      try {
        const r = await get<{ entries: AuditEntry[]; next: number }>("/audit?" + q);
        setEntries((old) => (before ? [...old, ...r.entries] : r.entries));
        setNext(r.next);
        setError(undefined);
      } catch (e: any) {
        setError(String(e.message ?? e));
      }
    },
    [user, method, path],
  );
  useEffect(() => {
    load(0);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  const search = (e: FormEvent) => {
    e.preventDefault();
    load(0);
  };

  return (
    <>
      <div className="head">
        <h1>Audit log</h1>
      </div>
      <p className="muted">Who asked for what: every change, console opening and sign-in. Request contents are never recorded. The Tasks page shows what happened on the nodes.</p>
      <form className="row" onSubmit={search}>
        <input placeholder="User" value={user} onChange={(e) => setUser(e.target.value)} aria-label="Filter by user" />
        <Select auto label="Filter by method" value={method} onChange={setMethod} options={[{ value: "", label: "Any method" }, ...["POST", "PUT", "DELETE", "GET"].map((m) => ({ value: m, label: m }))]} />
        <input placeholder="Path contains" value={path} onChange={(e) => setPath(e.target.value)} aria-label="Filter by path" />
        <button className="primary">Search</button>
      </form>
      <ErrorBox error={error} />
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>User</th>
            <th>Address</th>
            <th>Request</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id}>
              <td>{when(e.time)}</td>
              <td>
                {e.user || "-"}
                {e.token && <span className="muted"> (token {e.token})</span>}
              </td>
              <td className="mono">{e.ip}</td>
              <td className="mono">
                {e.method} {e.path}
              </td>
              <td>
                <span className={"badge " + (e.status >= 400 ? "s-crashed" : "s-running")}>{e.status}</span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {entries.length === 0 && !error && <div className="empty">No matching entries.</div>}
      {next > 0 && (
        <p>
          <button onClick={() => load(next)}>Load older entries</button>
        </p>
      )}
    </>
  );
}
