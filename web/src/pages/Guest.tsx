// Guest-agent driven parts of the VM page: what the guest reports about
// itself, the "set up the agent" help for the detected OS, and the shutdown
// menu with its timeout.
import { useEffect, useState } from "react";
import { AgentHelp, get, GuestInfo, post, put, VMDetail } from "../api";
import { useLive } from "../live";
import { ErrorBox, useAction, useConfirm } from "../ui";

/** OS, hostname and filesystem usage as the guest agent reports them. */
export function GuestCard({ d, g }: { d: VMDetail; g?: GuestInfo }) {
  if (d.agent !== "connected" || d.state !== "running") return null;
  return (
    <section className="card">
      <h3>Guest</h3>
      {!g && <p className="muted">Asking the guest agent</p>}
      {g && !g.available && <p className="muted">The guest agent did not answer: {g.error}</p>}
      {g?.available && (
        <>
          <dl>
            {g.os?.version && (
              <>
                <dt>Version</dt>
                <dd>{g.os.version}</dd>
              </>
            )}
            {g.os?.kernel && (
              <>
                <dt>Kernel</dt>
                <dd className="mono">
                  {g.os.kernel}
                  {g.os.machine ? ` (${g.os.machine})` : ""}
                </dd>
              </>
            )}
            {g.timezone && (
              <>
                <dt>Time zone</dt>
                <dd>{g.timezone}</dd>
              </>
            )}
          </dl>
        </>
      )}
    </section>
  );
}

/** OS-specific next steps for a guest without a working agent; for Windows, the virtio-win ISO. */
export function useAgentHelp(base: string, enabled: boolean) {
  // Reloads by itself when a task (such as the ISO download) finishes.
  return useLive(
    () => (enabled ? get<AgentHelp>(`${base}/agent-help`) : Promise.resolve(undefined)),
    `${base}/${enabled}`,
    (e) => (e.type === "task" ? e.task?.status !== "running" : e.type === "domain"),
  ).data;
}

export function VirtioWin({ d, base, help }: { d: VMDetail; base: string; help: AgentHelp }) {
  const act = useAction();
  const confirm = useConfirm();
  const [pool, setPool] = useState(help.downloadPools[0] ?? "");
  useEffect(() => setPool((p) => (help.downloadPools.includes(p) ? p : (help.downloadPools[0] ?? ""))), [help.downloadPools]);
  const iso = help.virtioWin;

  const attach = () =>
    act.run(async () => {
      if (!iso) return;
      let target = help.cdrom ?? help.cdromInUse;
      if (!help.cdrom && help.cdromInUse && !(await confirm.ask({ title: "Replace the CD-ROM medium", message: `The CD-ROM drive ${help.cdromInUse} of ${d.name} holds another medium. It is replaced with the virtio-win ISO.`, confirm: "Replace medium" }))) return;
      if (!target) target = (await post<{ target: string }>(`${base}/cdrom`)).target;
      await put(`${base}/cdrom/${encodeURIComponent(target)}`, { pool: iso.pool, volume: iso.volume });
    });
  const download = () =>
    act.run(async () => {
      if (!(await confirm.ask({ title: "Download virtio-win", message: `${help.downloadName} (about 700 MB) is downloaded to pool ${pool} on the node. The node fetches it from ${help.downloadUrl}.`, confirm: "Download", danger: false }))) return;
      await post(`/nodes/${d.node}/pools/${encodeURIComponent(pool)}/volumes/download`, { name: help.downloadName, url: help.downloadUrl });
    });

  return (
    <div className="virtio">
      {confirm.dialog}
      <p>{help.source === "libosinfo" ? "This VM is registered as a Windows guest." : "This VM looks like a Windows guest."} The guest agent comes with the virtio-win drivers.</p>
      {help.attached ? (
        <p>
          The virtio-win ISO is attached as <code>{help.attached}</code>. Open it in the guest and run <code>virtio-win-guest-tools.exe</code>.
        </p>
      ) : iso ? (
        <div className="actions">
          <button className="primary" disabled={act.busy} onClick={attach}>
            Attach virtio-win ISO
          </button>
          <span className="muted">
            {iso.volume} in pool {iso.pool}
            {help.cdrom ? ` → empty drive ${help.cdrom}` : help.cdromInUse ? ` → replaces the medium in ${help.cdromInUse}` : " → a new CD-ROM drive"}
          </span>
        </div>
      ) : help.downloadPools.length > 0 ? (
        <div className="actions">
          <span className="muted">No virtio-win ISO found in the node's storage pools.</span>
          <select value={pool} onChange={(e) => setPool(e.target.value)} style={{ width: "auto" }} aria-label="Pool for the download">
            {help.downloadPools.map((p) => (
              <option key={p}>{p}</option>
            ))}
          </select>
          <button disabled={act.busy || !pool} onClick={download}>
            {act.busy ? "Downloading" : "Download to pool"}
          </button>
        </div>
      ) : (
        <p className="muted">No virtio-win ISO found, and no directory-backed pool to download it to. Upload virtio-win.iso on the Storage page.</p>
      )}
      <ErrorBox error={act.error} />
    </div>
  );
}
