import { Menu, User, LogOut } from "lucide-react";
import { FormEvent, useEffect, useRef, useState } from "react";
import { Navigate, NavLink, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import Tree from "./Tree";
import "./shell.css";
import { get, post, Role, setUnauthorizedHandler, Task } from "./api";
import { startEvents, stopEvents, useEvents } from "./live";
import { ErrorBox, Field, useAction } from "./ui";
import Datacenter from "./pages/Datacenter";
import VMDetailPage from "./pages/VMDetail";
import NodeDetail from "./pages/NodeDetail";
import Users from "./pages/Users";
import Audit from "./pages/Audit";
import WebAccess from "./pages/WebAccess";
import ControllerBackup from "./pages/ControllerBackup";
import Account from "./pages/Account";
import CloudProfiles from "./pages/CloudProfiles";
import ConsolePage from "./pages/ConsolePage";
import { AlertBell, AlertSettings } from "./pages/Alerts";
import { DropdownMenu } from "./Menu";
import { RoleContext } from "./auth";
import { ThemeToggle } from "./theme";
import TasksDock, { useDockOpen } from "./TasksDock";
import { bindPrefs, loadPrefs, usePref } from "./prefs";
import Resizer, { clampSize } from "./Resizer";

const SIDEBAR_MIN = 180;
const DOCK_MIN = 96;
const sidebarMax = () => Math.min(640, window.innerWidth * 0.5);
const dockMax = () => window.innerHeight * 0.7;
const px = (v: string | undefined) => (v && Number(v) > 0 ? Number(v) : undefined);

/** The administrator's pages, behind one menu in the top bar. */
const SETTINGS = [
  { label: "Users", to: "/users" },
  { label: "Audit", to: "/audit" },
  { label: "Web access", to: "/web-access" },
  { label: "Alerts", to: "/alerts-settings" },
  { label: "Backup", to: "/controller-backup" },
];

type Session = { needsSetup: boolean; user: string; role: Role };

export default function App() {
  const [session, setSession] = useState<Session | undefined>();
  const [drawer, setDrawer] = useState(false);
  bindPrefs(session?.user ?? ""); // before anything reads a preference, so the first paint uses the local copy
  const dockOpen = useDockOpen();
  const sideW = px(usePref("sidebarW"));
  const dockH = px(usePref("dockH"));
  // Saved pane sizes, kept inside what this window allows. Unset: the stylesheet defaults.
  const sizes: Record<string, string> = {};
  if (sideW) sizes["--sidebar-w"] = clampSize(sideW, SIDEBAR_MIN, sidebarMax()) + "px";
  if (dockH && dockOpen) sizes["--dock-h"] = clampSize(dockH, DOCK_MIN, dockMax()) + "px";
  const scroller = useRef<HTMLDivElement>(null);
  const loc = useLocation();
  const navigate = useNavigate();
  useEffect(() => setDrawer(false), [loc.pathname, loc.search]);
  useEffect(() => {
    scroller.current?.scrollTo(0, 0);
  }, [loc.pathname]);
  useEffect(() => {
    if (!drawer) return;
    const k = (e: KeyboardEvent) => e.key === "Escape" && setDrawer(false);
    window.addEventListener("keydown", k);
    return () => window.removeEventListener("keydown", k);
  }, [drawer]);
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

  useEffect(() => {
    if (session?.user) void loadPrefs();
  }, [session?.user]);
  if (!session) return null;
  if (!session.user) return <SignIn setup={session.needsSetup} onDone={refresh} />;
  if (loc.pathname.startsWith("/console/"))
    return (
      <RoleContext.Provider value={session.role}>
        <Routes>
          <Route path="/console/:node/:uuid" element={<ConsolePage />} />
        </Routes>
      </RoleContext.Provider>
    );
  return (
    <RoleContext.Provider value={session.role}>
    <div className={"app" + (drawer ? " drawer-open" : "") + (dockOpen ? " dock-open" : "")} style={sizes as React.CSSProperties}>
      <header className="topbar">
        <button className="menu-btn" aria-label="Open navigation" aria-expanded={drawer} onClick={() => setDrawer(!drawer)}>
          <Menu size={18} />
        </button>
        <div className="brand">Virticle</div>
        <div className="toplinks" role="navigation" aria-label="Main">
        <NavLink to="/datacenter/vms">Virtual machines</NavLink>
        <NavLink to="/datacenter/storage">Storage</NavLink>
        <NavLink to="/datacenter/network">Networks</NavLink>
        <NavLink to="/datacenter/backups">Backups</NavLink>
        <NavLink to="/datacenter/nodes">Nodes</NavLink>
        <NavLink to="/datacenter/tasks">Tasks</NavLink>
        {session.role === "admin" && (
          <DropdownMenu
            className={"settings-menu" + (SETTINGS.some((x) => loc.pathname.startsWith(x.to)) ? " active" : "")}
            label="Settings"
            items={SETTINGS.map((x) => ({ label: x.label, run: () => navigate(x.to) }))}
          />
        )}
        </div>
        <div className="spacer" />
        <AlertBell />
        <ThemeToggle />
        <NavLink className="acct" to="/account" aria-label={session.user} title={`Signed in as ${session.role}`}>
          <User size={16} aria-hidden />
          <span className="name">{session.user}</span>
        </NavLink>
        <button className="link signout" aria-label="Sign out" onClick={() => post("/logout").then(refresh)}>
          <LogOut size={16} aria-hidden />
          <span className="name">Sign out</span>
        </button>
      </header>
      <aside className="sidebar" aria-label="Resources">
        <div className="drawer-links" role="navigation" aria-label="Main menu">
        <NavLink to="/datacenter/vms">Virtual machines</NavLink>
        <NavLink to="/datacenter/storage">Storage</NavLink>
        <NavLink to="/datacenter/network">Networks</NavLink>
        <NavLink to="/datacenter/backups">Backups</NavLink>
        <NavLink to="/datacenter/nodes">Nodes</NavLink>
        <NavLink to="/datacenter/tasks">Tasks</NavLink>
        {session.role === "admin" && <NavLink to="/users">Users</NavLink>}
        {session.role === "admin" && <NavLink to="/audit">Audit</NavLink>}
        {session.role === "admin" && <NavLink to="/web-access">Web access</NavLink>}
        {session.role === "admin" && <NavLink to="/alerts-settings">Alerts</NavLink>}
        {session.role === "admin" && <NavLink to="/controller-backup">Backup</NavLink>}
        </div>
        <Tree />
        <Resizer axis="x" cssVar="--sidebar-w" pref="sidebarW" min={SIDEBAR_MIN} max={sidebarMax} current={() => document.querySelector(".sidebar")?.getBoundingClientRect().width ?? 260} label="Resize the resource tree" />
      </aside>
      {drawer && <div className="scrim" onClick={() => setDrawer(false)} />}
      <div className="content">
      <div className="content-scroll" ref={scroller}>
      <main className={session.role}>
        <Routes>
          <Route path="/datacenter/:tab?" element={<Datacenter />} />
          {/* Old addresses keep working; query strings (?new=, ?pool=&catalogue=1) are kept. */}
          <Route path="/" element={<Legacy to="/datacenter" whenQuery="new" otherwise="/datacenter/vms" />} />
          <Route path="/storage" element={<Legacy to="/datacenter/storage" />} />
          <Route path="/networks" element={<Legacy to="/datacenter/network" />} />
          <Route path="/backups" element={<Legacy to="/datacenter/backups" />} />
          <Route path="/nodes" element={<Legacy to="/datacenter/nodes" />} />
          <Route path="/tasks" element={<Legacy to="/datacenter/tasks" />} />
          <Route path="/vm/:node/:uuid" element={<VMDetailPage />} />
          <Route path="/node/:id/:tab?" element={<NodeDetail />} />
          <Route path="/account" element={<Account />} />
          {session.role !== "viewer" && <Route path="/cloudinit-profiles" element={<CloudProfiles />} />}
          {session.role === "admin" && <Route path="/users" element={<Users />} />}
          {session.role === "admin" && <Route path="/audit" element={<Audit />} />}
          {session.role === "admin" && <Route path="/web-access" element={<WebAccess />} />}
          {session.role === "admin" && <Route path="/alerts-settings" element={<AlertSettings />} />}
          {session.role === "admin" && <Route path="/controller-backup" element={<ControllerBackup />} />}
          <Route path="*" element={<div className="empty">There is nothing at this address, or your role does not allow it.</div>} />
        </Routes>
      </main>
      </div>
      {dockOpen && <Resizer axis="y" cssVar="--dock-h" pref="dockH" min={DOCK_MIN} max={dockMax} current={() => document.querySelector(".dock")?.getBoundingClientRect().height ?? 200} label="Resize the tasks panel" />}
      <TasksDock />
      </div>
      <Toasts />
    </div>
    </RoleContext.Provider>
  );
}

/** Redirect from an old address. `/` goes to the summary, or to the VM list when it carries ?new= (Add VM). */
function Legacy({ to, whenQuery, otherwise }: { to: string; whenQuery?: string; otherwise?: string }) {
  const { search } = useLocation();
  const dest = whenQuery && otherwise && new URLSearchParams(search).has(whenQuery) ? otherwise : to;
  return <Navigate to={{ pathname: dest, search }} replace />;
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
        <h1>Virticle</h1>
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
