import { Bell } from "lucide-react";
import { useEffect, useState } from "react";
import { NavLink } from "react-router-dom";
import { Alert, get } from "../api";
import { useLive } from "../live";

/** Current alerts; refetched whenever the server announces a change. */
export function useAlerts(): Alert[] {
  const r = useLive(() => get<{ alerts: Alert[] }>("/alerts"), "alerts", (e) => e.type === "alert");
  return r.data?.alerts ?? [];
}

function ago(since: number): string {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - since));
  if (s < 90) return "just now";
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

/** Bell in the navigation with a count; opens a panel listing the alerts. */
export function AlertBell() {
  const alerts = useAlerts();
  const [open, setOpen] = useState(false);
  const critical = alerts.filter((a) => a.severity === "critical").length;
  useEffect(() => {
    if (!open) return;
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [open]);
  return (
    <div className="alertbell">
      <button className="bell" onClick={() => setOpen(!open)} aria-label={`Alerts: ${alerts.length}`} aria-expanded={open} title="Alerts">
        <Bell size={16} aria-hidden />
        <span className="name">Alerts</span>
        {alerts.length > 0 && <span className={"count " + (critical ? "critical" : "warning")}>{alerts.length}</span>}
      </button>
      {open && (
        <div className="alertpanel" role="dialog" aria-label="Alerts">
          <header>
            <strong>Alerts</strong>
            <button className="ghost" onClick={() => setOpen(false)} aria-label="Close alerts">
              ✕
            </button>
          </header>
          {alerts.length === 0 ? (
            <div className="muted pad">Nothing needs attention.</div>
          ) : (
            <ul>
              {alerts.map((a) => (
                <li key={a.id} className={a.severity}>
                  <div className="atitle">
                    <span className={"sev " + a.severity}>{a.severity}</span> {a.link ? <NavLink to={a.link} onClick={() => setOpen(false)}>{a.title}</NavLink> : a.title}
                  </div>
                  <div className="amsg">{a.message}</div>
                  <small>since {ago(a.since)}</small>
                </li>
              ))}
            </ul>
          )}
          <footer className="muted">Checked every minute. Alerts clear themselves when the condition is gone.</footer>
        </div>
      )}
    </div>
  );
}

/** Small inline markers for the alerts that match, to put next to an object. */
export function AlertMarks({ alerts, match }: { alerts: Alert[]; match: (a: Alert) => boolean }) {
  const mine = alerts.filter(match);
  if (mine.length === 0) return null;
  return (
    <span className="marks">
      {mine.map((a) => (
        <span key={a.id} className={"mark " + a.severity} title={a.message}>
          {a.severity === "critical" ? "⛔" : "⚠"} {a.title}
        </span>
      ))}
    </span>
  );
}
