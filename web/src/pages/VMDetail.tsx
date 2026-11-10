import { useEffect, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { bytes, del, get, post, put, VMDetail } from "../api";
import { useLive } from "../live";
import { ErrorBox, Modal, StateBadge, useAction } from "../ui";
import { SerialConsole, VNCConsole } from "./Console";
import MigrateDialog from "./Migrate";

export default function VMDetailPage() {
  const { node: nodeParam, uuid = "" } = useParams();
  const node = Number(nodeParam);
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") ?? "overview";
  const nav = useNavigate();
  const vm = useLive(() => get<VMDetail>(`/nodes/${node}/vms/${uuid}`), `${node}/${uuid}`, (e) =>
    e.type === "node" ? true : e.type === "domain" ? e.uuid === uuid : e.task?.status !== "running",
  );
  const act = useAction();
  const [deleting, setDeleting] = useState(false);
  const [migrating, setMigrating] = useState(false);
  const base = `/nodes/${node}/vms/${uuid}`;
  const d = vm.data;

  if (!d) {
    return (
      <>
        <Link to="/">← Virtual machines</Link>
        <ErrorBox error={vm.error} />
      </>
    );
  }
  const run = (action: string) => act.run(() => post(`${base}/action`, { action }));
  const off = d.state === "shutoff";
  const tabs = [
    ["overview", "Overview"],
    ["console", "Console"],
    ["xml", "XML"],
  ];

  return (
    <>
      <Link to="/">← Virtual machines</Link>
      <div className="head">
        <h1>
          {d.name} <StateBadge state={d.state} />
        </h1>
        <div className="actions">
          {off && (
            <button className="primary" disabled={act.busy} onClick={() => run("start")}>
              Start
            </button>
          )}
          {d.state === "paused" && (
            <button disabled={act.busy} onClick={() => run("resume")}>
              Resume
            </button>
          )}
          {d.state === "running" && (
            <>
              <button disabled={act.busy} onClick={() => run("shutdown")}>
                Shut down
              </button>
              <button disabled={act.busy} onClick={() => run("reboot")}>
                Reboot
              </button>
              <button disabled={act.busy} onClick={() => run("pause")}>
                Pause
              </button>
            </>
          )}
          {!off && (
            <>
              <button disabled={act.busy} onClick={() => window.confirm(`Reset ${d.name}? This is like pressing the reset button.`) && run("reset")}>
                Reset
              </button>
              <button className="danger" disabled={act.busy} onClick={() => window.confirm(`Force ${d.name} off? Unsaved data in the guest is lost.`) && run("poweroff")}>
                Force off
              </button>
            </>
          )}
          <button onClick={() => setMigrating(true)}>Migrate</button>
          {off && (
            <button className="danger" onClick={() => setDeleting(true)}>
              Delete
            </button>
          )}
        </div>
      </div>
      <ErrorBox error={act.error ?? vm.error} />
      <div className="tabs">
        {tabs.map(([id, label]) => (
          <button key={id} className={tab === id ? "on" : ""} onClick={() => setParams(id === "overview" ? {} : { tab: id })}>
            {label}
          </button>
        ))}
      </div>
      {tab === "overview" && <Overview d={d} base={base} />}
      {tab === "console" && <ConsoleTab d={d} />}
      {tab === "xml" && <XMLEditor d={d} base={base} onSaved={vm.reload} />}
      {migrating && <MigrateDialog d={d} onClose={() => setMigrating(false)} />}
      {deleting && <DeleteVM d={d} base={base} onClose={() => setDeleting(false)} onDeleted={() => nav("/")} />}
    </>
  );
}

function Overview({ d, base }: { d: VMDetail; base: string }) {
  const act = useAction();
  return (
    <div className="cards">
      <section className="card">
        <h3>Summary</h3>
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
              <input type="checkbox" checked={d.autostart} disabled={act.busy} onChange={(e) => act.run(() => put(`${base}/autostart`, { autostart: e.target.checked }))} />
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

      <AgentCard d={d} base={base} />

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
                <td className="mono">{n.ips.length ? n.ips.join(", ") : d.state === "running" ? "no address yet" : ""}</td>
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
  const copy = (cmd: string) => navigator.clipboard?.writeText(cmd).then(() => setCopied(cmd));
  const addChannel = () =>
    act.run(async () => {
      const r = await post<{ needsRestart: boolean }>(`${base}/agent-channel`);
      setRestart(r.needsRestart);
    });

  return (
    <section className="card">
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
      {restart && <p className="note">The channel was saved but could not be hot-plugged. It will appear after the VM is shut down and started again.</p>}
      {d.agent === "disconnected" && (
        <>
          <p>
            The channel is in place but nothing answers inside the guest{d.state !== "running" ? " (the VM is not running)" : ""}. Install and start the agent in the guest:
          </p>
          <div className="install">
            {agentInstall.map(([os, cmd]) => (
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

function ConsoleTab({ d }: { d: VMDetail }) {
  const kinds = [...(d.hasVNC ? ["vnc"] : []), ...(d.hasSerial ? ["serial"] : [])];
  const [kind, setKind] = useState(kinds[0]);
  if (d.state === "shutoff") return <div className="empty">Start the VM to open its console.</div>;
  if (kinds.length === 0) return <div className="empty">This VM has neither a VNC display nor a serial console. Add one on the XML tab.</div>;
  return (
    <>
      {kinds.length > 1 && (
        <div className="segmented">
          <button className={kind === "vnc" ? "on" : ""} onClick={() => setKind("vnc")}>
            Display (VNC)
          </button>
          <button className={kind === "serial" ? "on" : ""} onClick={() => setKind("serial")}>
            Serial console
          </button>
        </div>
      )}
      {kind === "vnc" ? <VNCConsole node={d.node} uuid={d.uuid} /> : <SerialConsole node={d.node} uuid={d.uuid} />}
    </>
  );
}

function XMLEditor({ d, base, onSaved }: { d: VMDetail; base: string; onSaved: () => void }) {
  const [text, setText] = useState(d.xml);
  const [dirty, setDirty] = useState(false);
  const [saved, setSaved] = useState(false);
  const act = useAction();
  // Follow outside changes until the user starts editing.
  useEffect(() => {
    if (!dirty) setText(d.xml);
  }, [d.xml, dirty]);
  const save = async () => {
    if (await act.run(() => put(`${base}/xml`, { xml: text }))) {
      setDirty(false);
      setSaved(true);
      onSaved();
    }
  };
  return (
    <div className="xml">
      <p className="muted">
        The full libvirt definition, the same document <code>virsh edit</code> shows. libvirt validates it on save.
        {d.state !== "shutoff" && " The VM is running: saved changes apply the next time it is started from off."}
      </p>
      <textarea
        spellCheck={false}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setDirty(true);
          setSaved(false);
        }}
      />
      <ErrorBox error={act.error} />
      <footer>
        {saved && <span className="muted">Saved.</span>}
        <button
          disabled={!dirty}
          onClick={() => {
            setText(d.xml);
            setDirty(false);
            act.clear();
          }}
        >
          Revert
        </button>
        <button className="primary" disabled={!dirty || act.busy} onClick={save}>
          Save
        </button>
      </footer>
    </div>
  );
}

function DeleteVM(props: { d: VMDetail; base: string; onClose: () => void; onDeleted: () => void }) {
  const { d } = props;
  const [disks, setDisks] = useState(false);
  const act = useAction();
  const owned = d.disks.filter((k) => k.device === "disk" && !k.readOnly && k.source);
  return (
    <Modal title={`Delete ${d.name}?`} onClose={props.onClose}>
      <p>The VM's definition{d.snapshots.length ? `, its ${d.snapshots.length} snapshot record(s)` : ""} and firmware settings are removed. This cannot be undone.</p>
      {owned.length > 0 && (
        <label className="check">
          <input type="checkbox" checked={disks} onChange={(e) => setDisks(e.target.checked)} />
          Also delete its disk{owned.length > 1 ? "s" : ""}: <span className="mono">{owned.map((k) => k.source).join(", ")}</span>
        </label>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={props.onClose}>Cancel</button>
        <button className="danger" disabled={act.busy} onClick={async () => {
            let left: string[] = [];
            const ok = await act.run(async () => {
              left = (await del<{ leftDisks: string[] }>(`${props.base}${disks ? "?disks=1" : ""}`)).leftDisks;
            });
            if (!ok) return;
            if (left.length) window.alert(`The VM was deleted, but these disks are not in any storage pool and were left in place:\n\n${left.join("\n")}`);
            props.onDeleted();
          }}>
          Delete
        </button>
      </footer>
    </Modal>
  );
}
