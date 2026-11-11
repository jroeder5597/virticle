import { get, Task } from "../api";
import { useLive } from "../live";
import { useInventory } from "../inventory";
import { absTime, orderTasks, TaskNode, TaskStatus, TaskTarget } from "../taskView";
import { ErrorBox, PageTitle } from "../ui";
import "../tasksdock.css";
import { StatusLine } from "../Checks";

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
            {orderTasks(tasks.data ?? []).map((t) => (
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
                  <TaskStatus status={t.status} />
                  {t.status === "running" && (
                    <span className="dk-prog">
                      <span className="dk-bar">
                        <span style={{ width: `${Math.round(Math.min(1, t.progress) * 100)}%` }} />
                      </span>
                      <span className="num">{t.progress > 0 ? `${(t.progress * 100).toFixed(0)}%` : ""}</span>
                    </span>
                  )}
                  {t.error && <StatusLine status="error">{t.error}</StatusLine>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
