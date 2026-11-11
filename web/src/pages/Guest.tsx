// Guest-agent driven parts of the VM page: what the guest reports about
// itself, the "set up the agent" help for the detected OS, and the shutdown
// menu with its timeout.
import { useEffect, useState } from "react";
import { AgentHelp, get, GuestInfo, post, put, VMDetail } from "../api";
import { useLive } from "../live";
import { ErrorBox, useAction, useConfirm } from "../ui";

const utc = (off: number) => {
  const m = Math.abs(off) / 60;
  return `UTC${off < 0 ? "-" : "+"}${Math.floor(m / 60)}${m % 60 ? ":" + String(m % 60).padStart(2, "0") : ""}`;
};

/** Seconds as a short span: "5 s", "3 min", "2.5 h". */
function span(sec: number): string {
  const a = Math.abs(sec);
  return a < 120 ? `${Math.round(a)} s` : a < 7200 ? `${Math.round(a / 60)} min` : `${(a / 3600).toFixed(1)} h`;
}

/**
 * What only the guest agent can tell and the rest of the overview does not show: kernel, clock, users,
 * agent, vCPUs and every network interface with all its addresses. OS, host name and file systems are
 * on the Summary and Resource utilization cards.
 */
export function GuestCard({ d, g }: { d: VMDetail; g?: GuestInfo }) {
  if (d.agent !== "connected" || d.state !== "running") return null;
  const row = (label: string, value: React.ReactNode, mono = false) => (
    <>
      <dt>{label}</dt>
      <dd className={mono ? "mono wrap" : undefined}>{value}</dd>
    </>
  );
  return (
    <section className="card" id="guest-details" aria-label="Guest details">
      <h3>Guest details</h3>
      {!g && <p className="muted">Asking the guest agent</p>}
      {g && !g.available && <p className="muted">The guest agent did not answer: {g.error}</p>}
      {g?.available && (
        <dl>
          {g.os?.kernel && row("Kernel", `${g.os.kernel}${g.os.machine ? ` (${g.os.machine})` : ""}`, true)}
          {g.timezone && row("Time zone", `${g.timezone}${g.timezoneOffset !== undefined ? ` (${utc(g.timezoneOffset)})` : ""}`)}
          {g.clockOffsetSec !== undefined && row("Clock", `${span(g.clockOffsetSec)} ${g.clockOffsetSec > 0 ? "ahead of" : "behind"} the host`)}
          {g.users !== undefined &&
            row(
              "Logged-in users",
              g.users.length === 0 ? (
                "none"
              ) : (
                <>
                  {g.users.map((u, i) => (
                    <div key={i}>
                      {u.user} <span className="muted">since {new Date(u.loginTime * 1000).toLocaleString()}</span>
                    </div>
                  ))}
                </>
              ),
            )}
          {g.vcpusOnline !== undefined && row("vCPUs online", `${g.vcpusOnline} of ${g.vcpusTotal}`)}
          {g.agentVersion && row("Guest agent", `version ${g.agentVersion}`)}
          {g.quiesce !== undefined && row("Quiesced snapshots", g.quiesce ? "available" : "not available")}
          {g.interfaces?.map((n) => row(n.name, n.addresses.length ? n.addresses.map((a) => <div key={a}>{a}</div>) : <span className="muted">no address</span>, true))}
        </dl>
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
