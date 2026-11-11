// The Overview tab of the VM page: a fixed-layout Status card, then the console preview and detail cards.
import { useEffect, useState } from "react";
import { bytes, get, GuestInfo, Network, post, put, VMDetail } from "../api";
import { useRole } from "../auth";
import { pct, rate, Spark, usePoll, VMPoint } from "../charts";
import { ErrorBox, StateBadge, UsageBar, useAction } from "../ui";
import { ConsoleCard } from "./ConsolePreview";
import { GuestCard, useAgentHelp, VirtioWin } from "./Guest";
import "./status.css";

const last = <T,>(a: T[]): T | undefined => a[a.length - 1];
const dash = "–";
const kib = (v: number | null | undefined) => (v == null ? dash : bytes(v * 1024));

/** A name for the guest from the network it is attached to: its DHCP lease's host name and the network's DNS domain. */
function useLeaseName(d: VMDetail) {
  const [name, setName] = useState<{ host?: string; domain?: string; network?: string }>();
  const ips = d.nics.map((n) => n.ips.join(",")).join(";");
  const wanted = d.state === "running" && d.nics.some((n) => n.type === "network");
  useEffect(() => {
    setName(undefined);
    if (!wanted) return;
    let live = true;
    get<Network[]>(`/nodes/${d.node}/networks`).then(
      (nets) => {
        if (!live) return;
        for (const n of d.nics) {
          const net = nets.find((x) => x.name === n.source);
          const lease = net?.leases?.find((l) => l.mac.toLowerCase() === n.mac.toLowerCase() && l.hostname && l.hostname !== "*");
          if (net && (lease || net.domain)) {
            setName({ host: lease?.hostname, domain: net.domain, network: net.name });
            if (lease) return;
          }
        }
      },
      () => {},
    );
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [d.node, d.uuid, wanted, ips]);
  return name;
}

type Stats = { points: VMPoint[] } | undefined;

/** Power state, guest OS, agent, DNS name and IP addresses. Rows keep their place whether or not values have arrived. */
function SummaryCard({ d, g }: { d: VMDetail; g?: GuestInfo }) {
  const running = d.state === "running";
  const lease = useLeaseName(d);
  const agent = d.agent === "connected" && running;
  const gHost = agent && g?.available ? g.hostname : undefined;
  const leaseName = lease?.host ? (lease.domain ? `${lease.host}.${lease.domain}` : lease.host) : undefined;
  const dns = gHost ?? leaseName;
  const os = agent && g?.available ? g.os?.prettyName || g.os?.name || g.os?.id : undefined;
  const agentText = d.agent === "connected" ? "connected" : d.agent === "absent" ? "not set up" : "not responding";
  const ips = d.nics.flatMap((n) => n.ips);
  return (
    <section className="card" id="summary" aria-label="Summary">
      <h3>Summary</h3>
      <dl className="ov-sum">
        <dt>Power State</dt>
        <dd>
          <StateBadge state={d.state} />
        </dd>
        <dt>Guest OS</dt>
        <dd>{os ?? "Not reported"}</dd>
        <dt>QEMU Guest Agent</dt>
        <dd>
          <span className={"badge s-agent-" + d.agent}>{agentText}</span>
        </dd>
        <dt>DNS Name</dt>
        <dd className="mono">{dns ?? (!running ? dash : !agent ? "Not reported: needs the guest agent" : !g ? "Asking the guest agent" : "Not reported by the guest")}</dd>
        <dt>IP Addresses</dt>
        <dd className="mono">
          {ips.length ? ips.join("  ") : running ? "no address yet" : dash}
          <div className="st-note muted">{!running ? "The VM is not running" : d.ipSource === "agent" ? "From the guest agent" : d.ipSource === "lease" ? "From the DHCP leases of the network (no guest agent)" : " "}</div>
        </dd>
      </dl>
    </section>
  );
}

/** CPU and memory with sparklines, storage on the host and in the guest, current I/O. Fixed layout. */
function UtilCard({ d, s, g }: { d: VMDetail; s: Stats; g?: GuestInfo }) {
  const running = d.state === "running";
  const pts = s?.points ?? [];
  const t = pts.map((p) => p.t);
  const cur = last(pts);
  const memTotal = cur?.memTotalKiB ?? 0;
  const memPct = cur?.memUsedKiB != null && memTotal ? (cur.memUsedKiB / memTotal) * 100 : null;
  const agent = d.agent === "connected" && running;
  const disks = d.disks.filter((k) => k.device === "disk");
  const fs = agent && g?.available ? g.filesystems : [];
  return (
    <section className="card" id="utilization" aria-label="Resource utilization">
      <h3>Resource utilization</h3>
      <div className="util">
        <div className="st-col">
          <h4>CPU</h4>
          <div className="st-val">{running ? pct(cur?.cpu) : dash}</div>
          <div className="st-note muted">
            {d.vcpus} vCPU{d.vcpus > 1 ? "s" : ""}
          </div>
          <Spark t={t} values={pts.map((p) => p.cpu)} max={100} />
        </div>
        <div className="st-col">
          <h4>Memory</h4>
          <div className="st-val">{running ? pct(memPct) : dash}</div>
          <div className="st-note muted">
            {running && cur?.memUsedKiB != null ? `${kib(cur.memUsedKiB)} of ${kib(memTotal)}${cur.memGuest ? "" : ", allocated"}` : `${kib(d.currentMemoryKiB || d.memoryKiB)} configured`}
          </div>
          <Spark t={t} values={pts.map((p) => p.memUsedKiB)} max={Math.max(memTotal, 1)} />
        </div>
      </div>
      <dl className="st-rates">
        <dt>Disk read</dt>
        <dd>{running ? rate(cur?.diskRead) : dash}</dd>
        <dt>Disk write</dt>
        <dd>{running ? rate(cur?.diskWrite) : dash}</dd>
        <dt>Network in</dt>
        <dd>{running ? rate(cur?.netRx) : dash}</dd>
        <dt>Network out</dt>
        <dd>{running ? rate(cur?.netTx) : dash}</dd>
      </dl>
      <div className="st-col">
        <h4>Storage on the host</h4>
        {disks.map((k) => (
          <div className="fsbar" key={k.target}>
            <div className="row">
              <span className="mono">{k.target}</span>
              <span className="muted">{k.capacity ? `${bytes(k.allocation ?? 0)} of ${bytes(k.capacity)}` : "size unknown"}</span>
            </div>
            <UsageBar used={k.allocation ?? 0} total={k.capacity ?? 0} />
          </div>
        ))}
        {disks.length === 0 && <div className="st-row muted">No disks</div>}
        <h4>Storage in the guest</h4>
        {fs.map((f) => (
          <div className="fsbar" key={f.mountpoint}>
            <div className="row">
              <span className="mono">{f.mountpoint}</span>
              <span className="muted">
                {bytes(f.usedBytes)} of {bytes(f.totalBytes)} ({f.totalBytes ? Math.round((f.usedBytes / f.totalBytes) * 100) : 0}%)
              </span>
            </div>
            <UsageBar used={f.usedBytes} total={f.totalBytes} />
          </div>
        ))}
        {fs.length === 0 && (
          <div className="st-row muted">
            {!running ? "The VM is not running" : d.agent !== "connected" ? "Not reported: needs the guest agent" : !g ? "Asking the guest agent" : g.available ? "The guest reports no filesystems" : "The guest agent did not answer"}
          </div>
        )}
      </div>
    </section>
  );
}

export default function Overview({ d, base }: { d: VMDetail; base: string }) {
  const act = useAction();
  const viewer = useRole() === "viewer";
  // One request each for the guest agent's report and the stats series, shared by the cards that show them.
  const g = usePoll<GuestInfo>(d.agent === "connected" && d.state === "running" ? `${base}/guest` : null, 20000);
  const s = usePoll<{ points: VMPoint[] }>(d.state === "running" ? `${base}/stats` : null);
  return (
    <div className="ov">
      <div className="ov-band">
        <ConsoleCard d={d} />
        <SummaryCard d={d} g={g} />
        <UtilCard d={d} s={s} g={g} />
      </div>
      <div className="ov-grid">
        <section className="card">
          <h3>Hardware</h3>
          <dl>
            <dt>Node</dt>
            <dd>{d.nodeName}</dd>
            <dt>vCPUs</dt>
            <dd>{d.vcpus}</dd>
            <dt>Memory</dt>
            <dd>{bytes(d.memoryKiB * 1024)}</dd>
            <dt>Firmware</dt>
            <dd>
              {d.firmware.toUpperCase()}
              {d.tpm ? " + TPM" : ""}
            </dd>
            <dt>Machine</dt>
            <dd>
              {d.machine} ({d.arch})
            </dd>
            <dt>Boot order</dt>
            <dd>{d.boot.length ? d.boot.join(", ") : "per device"}</dd>
            <dt>UUID</dt>
            <dd className="mono">{d.uuid}</dd>
            <dt>Start on host boot</dt>
            <dd>
              <label className="check">
                <input type="checkbox" checked={d.autostart} disabled={act.busy || viewer} onChange={(e) => act.run(() => put(`${base}/autostart`, { autostart: e.target.checked }))} />
                {d.autostart ? "Yes" : "No"}
              </label>
            </dd>
          </dl>
          {d.passthroughDevices > 0 && (
            <p className="note">
              {d.passthroughDevices} host device{d.passthroughDevices > 1 ? "s are" : " is"} passed through. This VM cannot be live migrated.
            </p>
          )}
          <ErrorBox error={act.error} />
        </section>


        <section className="card">
          <h3>Disks</h3>
          <table className="compact">
            <tbody>
              {d.disks.map((k) => (
                <tr key={k.target}>
                  <td className="mono">{k.target}</td>
                  <td>
                    {k.device}
                    {k.readOnly ? " (read-only)" : ""}
                  </td>
                  <td>{[k.bus, k.format].filter(Boolean).join(" · ")}</td>
                  <td className="mono wrap">{k.source || "(empty)"}</td>
                </tr>
              ))}
              {d.disks.length === 0 && (
                <tr>
                  <td className="muted">No disks.</td>
                </tr>
              )}
            </tbody>
          </table>
        </section>

        <section className="card">
          <h3>Network interfaces</h3>
          <table className="compact">
            <tbody>
              {d.nics.map((n) => (
                <tr key={n.mac}>
                  <td className="mono">{n.mac}</td>
                  <td>
                    {n.source} ({n.type})
                  </td>
                  <td>{n.model}</td>
                </tr>
              ))}
              {d.nics.length === 0 && (
                <tr>
                  <td className="muted">No network interfaces.</td>
                </tr>
              )}
            </tbody>
          </table>
        </section>

        {d.agent === "connected" ? <GuestCard d={d} g={g} /> : <AgentCard d={d} base={base} />}
      </div>
    </div>
  );
}

const agentInstall: [string, string][] = [
  ["Debian / Ubuntu", "sudo apt install -y qemu-guest-agent && sudo systemctl enable --now qemu-guest-agent"],
  ["RHEL / Rocky / Fedora", "sudo dnf install -y qemu-guest-agent && sudo systemctl enable --now qemu-guest-agent"],
  ["openSUSE", "sudo zypper install -y qemu-guest-agent && sudo systemctl enable --now qemu-guest-agent"],
  ["Alpine", "apk add qemu-guest-agent && rc-update add qemu-guest-agent && rc-service qemu-guest-agent start"],
  ["Windows", "Install virtio-win-guest-tools.exe from the virtio-win ISO (it includes the guest agent)."],
];

/** Guest agent status, with the steps to get it working when it is not. */
function AgentCard({ d, base }: { d: VMDetail; base: string }) {
  const act = useAction();
  const [restart, setRestart] = useState(false);
  const [copied, setCopied] = useState("");
  const help = useAgentHelp(base, d.agent !== "connected");
  const copy = (cmd: string) => navigator.clipboard?.writeText(cmd).then(() => setCopied(cmd));
  const addChannel = () =>
    act.run(async () => {
      const r = await post<{ needsRestart: boolean }>(`${base}/agent-channel`);
      setRestart(r.needsRestart);
    });

  return (
    <section className="card ov-wide">
      <h3>
        Guest agent <span className={"badge s-agent-" + d.agent}>{d.agent === "connected" ? "connected" : d.agent === "absent" ? "not set up" : "not responding"}</span>
      </h3>
      {d.agent === "connected" && <p className="muted">The guest reports its addresses and supports clean shutdown, filesystem freeze for snapshots and more.</p>}
      {d.agent === "absent" && (
        <>
          <p>
            This VM has no guest agent channel. The agent gives accurate IP addresses, consistent snapshots and reliable shutdown. Setting it up takes two steps: add the
            channel device here, then install the agent inside the guest.
          </p>
          <button className="primary" disabled={act.busy} onClick={addChannel}>
            Add agent channel
          </button>
        </>
      )}
      {d.agent !== "connected" && help?.os === "windows" && <VirtioWin d={d} base={base} help={help} />}
      {restart && <p className="note">The channel was saved but could not be hot-plugged. It will appear after the VM is shut down and started again.</p>}
      {d.agent === "disconnected" && (
        <>
          <p>
            The channel is in place but nothing answers inside the guest{d.state !== "running" ? " (the VM is not running)" : ""}. Install and start the agent in the guest:
          </p>
          <div className="install">
            {agentInstall.filter(([os]) => !help?.os || (help.os === "windows") === (os === "Windows")).map(([os, cmd]) => (
              <div key={os}>
                <strong>{os}</strong>
                <code>{cmd}</code>
                {os !== "Windows" && (
                  <button className="link" onClick={() => copy(cmd)}>
                    {copied === cmd ? "Copied" : "Copy"}
                  </button>
                )}
              </div>
            ))}
          </div>
          <p className="muted">This card updates by itself once the agent connects.</p>
        </>
      )}
      <ErrorBox error={act.error} />
    </section>
  );
}

