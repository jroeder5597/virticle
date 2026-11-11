import { get, Task } from "../api";
import { useLive } from "../live";
import { useInventory } from "../inventory";
import { absTime, TaskNode, TaskTarget } from "../taskView";
import { ErrorBox, PageTitle, StateBadge } from "../ui";

export default function Tasks() {
  const inv = useInventory();
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
                <td>{absTime(t.started)}</td>
                <td>{t.kind}</td>
                <td>
                  <TaskTarget task={t} inv={inv} />
                </td>
                <td>
                  <TaskNode task={t} inv={inv} />
                </td>
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
