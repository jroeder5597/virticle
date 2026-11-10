// Guest-agent driven parts of the VM page: what the guest reports about
// itself, the "set up the agent" help for the detected OS, and the shutdown
// menu with its timeout.
import { useEffect, useRef, useState } from "react";
import { AgentHelp, bytes, get, GuestInfo, post, put, VMDetail } from "../api";
import { usePoll } from "../charts";
import { useLive } from "../live";
import { ErrorBox, UsageBar, useAction } from "../ui";

/** OS, hostname and filesystem usage as the guest agent reports them. */
export function GuestCard({ d, base }: { d: VMDetail; base: string }) {
  const g = usePoll<GuestInfo>(d.agent === "connected" && d.state === "running" ? `${base}/guest` : null, 20000);
  if (d.agent !== "connected" || d.state !== "running") return null;
  return (
    <section className="card">
      <h3>Guest</h3>
      {!g && <p className="muted">Asking the guest agent…</p>}
      {g && !g.available && <p className="muted">The guest agent did not answer: {g.error}</p>}
      {g?.available && (
        <>
          <dl>
            <dt>Operating system</dt>
            <dd>{g.os?.prettyName || g.os?.name || g.os?.id || "unknown"}</dd>
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
            {g.hostname && (
              <>
                <dt>Host name</dt>
                <dd>{g.hostname}</dd>
              </>
            )}
            {g.timezone && (
              <>
                <dt>Time zone</dt>
                <dd>{g.timezone}</dd>
              </>
            )}
          </dl>
          {g.filesystems.length > 0 && (
            <>
              <h4>Filesystems</h4>
              {g.filesystems.map((f) => (
                <div className="fsbar" key={f.mountpoint}>
                  <div className="row">
                    <span className="mono">{f.mountpoint}</span>
                    <span className="muted">
                      {bytes(f.usedBytes)} of {bytes(f.totalBytes)}
                      {f.type ? ` · ${f.type}` : ""}
                    </span>
                  </div>
                  <UsageBar used={f.usedBytes} total={f.totalBytes} />
                </div>
              ))}
            </>
          )}
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
  const [pool, setPool] = useState(help.downloadPools[0] ?? "");
  useEffect(() => setPool((p) => (help.downloadPools.includes(p) ? p : (help.downloadPools[0] ?? ""))), [help.downloadPools]);
  const iso = help.virtioWin;

  const attach = () =>
    act.run(async () => {
      if (!iso) return;
      let target = help.cdrom ?? help.cdromInUse;
      if (!help.cdrom && help.cdromInUse && !window.confirm(`The CD-ROM drive ${help.cdromInUse} holds another medium. Replace it with the virtio-win ISO?`)) return;
      if (!target) target = (await post<{ target: string }>(`${base}/cdrom`)).target;
      await put(`${base}/cdrom/${encodeURIComponent(target)}`, { pool: iso.pool, volume: iso.volume });
    });
  const download = () =>
    act.run(async () => {
      if (!window.confirm(`Download ${help.downloadName} (about 700 MB) to pool ${pool} on the node? The node fetches it from ${help.downloadUrl}.`)) return;
      await post(`/nodes/${d.node}/pools/${encodeURIComponent(pool)}/volumes/download`, { name: help.downloadName, url: help.downloadUrl });
    });

  return (
    <div className="virtio">
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
            {act.busy ? "Downloading…" : "Download to pool"}
          </button>
        </div>
      ) : (
        <p className="muted">No virtio-win ISO found, and no directory-backed pool to download it to. Upload virtio-win.iso on the Storage page.</p>
      )}
      <ErrorBox error={act.error} />
    </div>
  );
}

/** Shut down with a split button: plain, with a force-off deadline, or straight to force off. */
export function ShutdownMenu({ d, busy, run }: { d: VMDetail; busy: boolean; run: (body: object) => Promise<boolean> }) {
  const [open, setOpen] = useState(false);
  const [waiting, setWaiting] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => !box.current?.contains(e.target as Node) && setOpen(false);
    const key = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", key);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", key);
    };
  }, [open]);
  const go = async (body: object, waits: boolean) => {
    setOpen(false);
    if (waits) setWaiting(true);
    await run(body);
    setWaiting(false);
  };
  return (
    <div className="menu" ref={box}>
      <button className="main" disabled={busy} onClick={() => go({ action: "shutdown" }, false)}>
        {waiting ? "Shutting down…" : "Shut down"}
      </button>
      <button className="split" disabled={busy} aria-label="More shutdown options" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
        ▾
      </button>
      {open && (
        <div className="menu-list" role="menu">
          <button role="menuitem" onClick={() => go({ action: "shutdown" }, false)}>
            Shut down
          </button>
          <button role="menuitem" onClick={() => go({ action: "shutdown", timeoutSec: 60, force: true }, true)}>
            Shut down, force off after 60 s
          </button>
          <button role="menuitem" onClick={() => window.confirm(`Force ${d.name} off? Unsaved data in the guest is lost.`) && go({ action: "poweroff" }, false)}>
            Force off
          </button>
        </div>
      )}
    </div>
  );
}
