import { get, Task } from "../api";
import { useLive } from "../live";
import { ErrorBox, PageTitle, StateBadge } from "../ui";

const time = (unix: number) => new Date(unix * 1000).toLocaleString();

export default function Tasks() {
  const tasks = useLive(() => get<Task[]>("/tasks"), "tasks", (e) => e.type === "task" || e.detail === "resync");
  return (
    <>
      <div className="head">
        <PageTitle>Tasks</PageTitle>
      </div>
      <ErrorBox error={tasks.error} />
      {tasks.data && tasks.data.length === 0 ? (
        <div className="empty">Nothing has been done yet. Every action taken here is recorded with its result.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Started</th>
              <th>Action</th>
              <th>Target</th>
              <th>Node</th>
              <th>By</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {(tasks.data ?? []).map((t) => (
              <tr key={t.id}>
                <td>{time(t.started)}</td>
                <td>{t.kind}</td>
                <td>{t.target}</td>
                <td>{t.node}</td>
                <td>{t.user}</td>
                <td>
                  <StateBadge state={t.status} />
                  {t.status === "running" && t.progress > 0 && <span className="muted"> {(t.progress * 100).toFixed(0)}%</span>}
                  {t.error && <div className="rowerror">{t.error}</div>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
