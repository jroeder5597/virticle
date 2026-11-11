import { FormEvent, useEffect, useState } from "react";
import { NavLink, Route, Routes } from "react-router-dom";
import { get, post, Role, setUnauthorizedHandler, Task } from "./api";
import { startEvents, stopEvents, useEvents } from "./live";
import { ErrorBox, Field, useAction } from "./ui";
import VMs from "./pages/VMs";
import VMDetailPage from "./pages/VMDetail";
import Storage from "./pages/Storage";
import { BackupsSection } from "./pages/Backups";
import Networks from "./pages/Networks";
import Nodes from "./pages/Nodes";
import NodeDetail from "./pages/NodeDetail";
import Tasks from "./pages/Tasks";
import Users from "./pages/Users";
import Audit from "./pages/Audit";
import Account from "./pages/Account";
import { AlertBell } from "./pages/Alerts";
import { RoleContext } from "./auth";
import { ThemeToggle } from "./theme";

type Session = { needsSetup: boolean; user: string; role: Role };

export default function App() {
  const [session, setSession] = useState<Session | undefined>();
  const refresh = () => get<Session>("/status").then(setSession, () => setSession({ needsSetup: false, user: "", role: "viewer" }));
  useEffect(() => {
    setUnauthorizedHandler(() => setSession((s) => (s ? { ...s, user: "" } : s)));
    refresh();
  }, []);
  useEffect(() => {
    if (session?.user) {
      startEvents();
      return stopEvents;
    }
  }, [session?.user]);

  if (!session) return null;
  if (!session.user) return <SignIn setup={session.needsSetup} onDone={refresh} />;
  return (
    <div className="shell">
      <nav>
        <div className="brand">virt-proj</div>
        <NavLink to="/" end>
          Virtual machines
        </NavLink>
        <NavLink to="/storage">Storage</NavLink>
        <NavLink to="/backups">Backups</NavLink>
        <NavLink to="/networks">Networks</NavLink>
        <NavLink to="/nodes">Nodes</NavLink>
        <NavLink to="/tasks">Tasks</NavLink>
        {session.role === "admin" && <NavLink to="/users">Users</NavLink>}
        {session.role === "admin" && <NavLink to="/audit">Audit</NavLink>}
        <div className="spacer" />
        <AlertBell />
        <div className="who">
          <NavLink to="/account" title={`Signed in as ${session.role}`}>
            {session.user}
          </NavLink>
          <ThemeToggle />
          <button className="link" onClick={() => post("/logout").then(refresh)}>
            Sign out
          </button>
        </div>
      </nav>
      <RoleContext.Provider value={session.role}>
      <main className={session.role}>
        <Routes>
          <Route path="/" element={<VMs />} />
          <Route path="/vm/:node/:uuid" element={<VMDetailPage />} />
          <Route path="/node/:id/:tab?" element={<NodeDetail />} />
          <Route path="/storage" element={<Storage />} />
          <Route path="/backups" element={<BackupsSection />} />
          <Route path="/networks" element={<Networks />} />
          <Route path="/nodes" element={<Nodes />} />
          <Route path="/tasks" element={<Tasks />} />
          <Route path="/account" element={<Account />} />
          {session.role === "admin" && <Route path="/users" element={<Users />} />}
          {session.role === "admin" && <Route path="/audit" element={<Audit />} />}
          <Route path="*" element={<div className="empty">There is nothing at this address, or your role does not allow it.</div>} />
        </Routes>
      </main>
      </RoleContext.Provider>
      <Toasts />
    </div>
  );
}

function SignIn({ setup, onDone }: { setup: boolean; onDone: () => void }) {
  const [username, setUsername] = useState(setup ? "admin" : "");
  const [password, setPassword] = useState("");
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await act.run(() => post(setup ? "/setup" : "/login", { username, password }))) onDone();
  };
  return (
    <div className="signin">
      <form onSubmit={submit} className="card">
        <h1>virt-proj</h1>
        <p className="muted">{setup ? "Create the administrator account to get started." : "Sign in to manage your virtual machines."}</p>
        <Field label="Username">
          <input value={username} onChange={(e) => setUsername(e.target.value)} autoFocus autoComplete="username" />
        </Field>
        <Field label="Password" hint={setup ? "At least 10 characters." : undefined}>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete={setup ? "new-password" : "current-password"} />
        </Field>
        <ErrorBox error={act.error} />
        <button className="primary" disabled={act.busy}>
          {setup ? "Create account" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

/** Shows failed tasks as they happen, wherever the user is. */
function Toasts() {
  const [failed, setFailed] = useState<Task[]>([]);
  useEvents((e) => {
    if (e.type === "task" && e.task?.status === "failed") {
      const t = e.task;
      setFailed((f) => [...f.filter((x) => x.id !== t.id), t].slice(-3));
      setTimeout(() => setFailed((f) => f.filter((x) => x.id !== t.id)), 10000);
    }
  });
  return (
    <div className="toasts">
      {failed.map((t) => (
        <div key={t.id} className="toast" onClick={() => setFailed((f) => f.filter((x) => x.id !== t.id))}>
          <strong>
            {t.kind} {t.target} failed
          </strong>
          <div>{t.error}</div>
        </div>
      ))}
    </div>
  );
}
