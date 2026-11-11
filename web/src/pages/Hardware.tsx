// Hardware tab: CPU, memory, disks, CD-ROM, network interfaces and boot order.
// Every change is sent to the server, which applies it to the running VM and
// the saved definition where libvirt allows and says which one happened.
import { useEffect, useState } from "react";
import { api, bytes, get, Network, Pool, Volume, VMDetail } from "../api";
import { useRole } from "../auth";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import FirmwareCard from "./Firmware";
import AdvancedCard from "./Advanced";
import PassthroughCard from "./Passthrough";
import DiskMoveDialog from "./DiskMove";

interface HwResult {
  applied: "live+config" | "live" | "config";
  needsRestart: boolean;
  note?: string;
  target?: string;
  mac?: string;
}

type Send = (method: string, path: string, body?: unknown) => Promise<HwResult>;
type DiskT = VMDetail["disks"][number];
type NicT = VMDetail["nics"][number];

function summary(r: HwResult, running: boolean): string {
  let s: string;
  if (r.needsRestart) s = "Saved to the VM's definition. It takes effect after the VM is shut down and started again.";
  else if (r.applied === "config" && !running) s = "Saved to the VM's definition.";
  else s = "Applied to the running VM and saved to its definition.";
  return r.note ? `${s} ${r.note}` : s;
}

export default function Hardware({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const act = useAction();
  const viewer = useRole() === "viewer";
  const [outcome, setOutcome] = useState<{ text: string; restart: boolean }>();
  const running = d.state !== "shutoff";
  const send: Send = async (method, path, body) => {
    const r = await api<HwResult>(method, base + path, body);
    setOutcome({ text: summary(r, running), restart: r.needsRestart });
    onChanged();
    return r;
  };
  const run = (fn: () => Promise<unknown>) => act.run(fn);
  const disks = d.disks.filter((k) => k.device === "disk");
  const cdroms = d.disks.filter((k) => k.device === "cdrom");

  return (
    // A viewer sees the hardware but every control is disabled, not just its buttons hidden.
    <fieldset className="plain" disabled={viewer}>
      {outcome && <div className={outcome.restart ? "note" : "muted"}>{outcome.text}</div>}
      <ErrorBox error={act.error} />
      <div className="cards" style={{ marginTop: 12, alignItems: "start" }}>
        <CpuMemory d={d} send={send} run={run} busy={act.busy} />
        <Boot d={d} send={send} run={run} busy={act.busy} />
        <Disks d={d} disks={disks} send={send} run={run} busy={act.busy} onChanged={onChanged} />
        <NICs d={d} send={send} run={run} busy={act.busy} />
        <CDROMs d={d} cdroms={cdroms} send={send} run={run} busy={act.busy} />
        <FirmwareCard d={d} base={base} onChanged={onChanged} />
        <AdvancedCard d={d} base={base} onChanged={onChanged} />
        <PassthroughCard d={d} base={base} onChanged={onChanged} />
      </div>
    </fieldset>
  );
}

interface CardProps {
  d: VMDetail;
  send: Send;
  run: (fn: () => Promise<unknown>) => Promise<boolean>;
  busy: boolean;
}

function CpuMemory({ d, send, run, busy }: CardProps) {
  const [editing, setEditing] = useState(false);
  const [vcpus, setVcpus] = useState(d.vcpus);
  const [maxV, setMaxV] = useState(d.maxVcpus);
  const [mem, setMem] = useState(Math.round(d.currentMemoryKiB / 1024));
  const [maxMem, setMaxMem] = useState(Math.round(d.memoryKiB / 1024));
  const start = () => {
    setVcpus(d.vcpus);
    setMaxV(d.maxVcpus);
    setMem(Math.round(d.currentMemoryKiB / 1024));
    setMaxMem(Math.round(d.memoryKiB / 1024));
    setEditing(true);
  };
  const save = async () => {
    const ok = await run(async () => {
      if (vcpus !== d.vcpus || maxV !== d.maxVcpus) await send("PUT", "/cpu", { vcpus, maxVcpus: maxV });
      if (mem !== Math.round(d.currentMemoryKiB / 1024) || maxMem !== Math.round(d.memoryKiB / 1024)) await send("PUT", "/memory", { memoryMiB: mem, maxMemoryMiB: maxMem });
    });
    if (ok) setEditing(false);
  };
  const num = (set: (n: number) => void) => (e: React.ChangeEvent<HTMLInputElement>) => set(Number(e.target.value));
  return (
    <section className="card" id="hw-cpu">
      <h3>CPU and memory</h3>
      {!editing ? (
        <>
          <dl>
            <dt>vCPUs</dt>
            <dd>
              {d.vcpus} <span className="muted">(up to {d.maxVcpus})</span>
            </dd>
            <dt>Memory</dt>
            <dd>
              {bytes(d.currentMemoryKiB * 1024)} <span className="muted">(up to {bytes(d.memoryKiB * 1024)})</span>
            </dd>
          </dl>
          <footer style={{ justifyContent: "flex-start" }}>
            <button onClick={start}>Edit</button>
          </footer>
        </>
      ) : (
        <>
          <div className="grid2">
            <Field label="vCPUs now">
              <input type="number" min={1} value={vcpus} onChange={num(setVcpus)} />
            </Field>
            <Field label="Maximum vCPUs" hint={d.state !== "shutoff" ? "A new maximum applies after a restart." : undefined}>
              <input type="number" min={1} value={maxV} onChange={num(setMaxV)} />
            </Field>
            <Field label="Memory now (MiB)" hint={d.state !== "shutoff" ? "Changed live by ballooning, within the running maximum." : undefined}>
              <input type="number" min={64} value={mem} onChange={num(setMem)} />
            </Field>
            <Field label="Maximum memory (MiB)" hint={d.state !== "shutoff" ? "A new maximum applies after a restart." : undefined}>
              <input type="number" min={64} value={maxMem} onChange={num(setMaxMem)} />
            </Field>
          </div>
          <footer>
            <button onClick={() => setEditing(false)}>Cancel</button>
            <button className="primary" disabled={busy || vcpus < 1 || mem < 64 || vcpus > maxV || mem > maxMem} onClick={save}>
              Apply
            </button>
          </footer>
        </>
      )}
    </section>
  );
}

// --- disks ---

function Disks({ d, disks, send, run, busy, onChanged }: CardProps & { disks: DiskT[]; onChanged: () => void }) {
  const [adding, setAdding] = useState(false);
  const [resizing, setResizing] = useState<DiskT>();
  const [detaching, setDetaching] = useState<DiskT>();
  const [moving, setMoving] = useState<DiskT>();
  return (
    <section className="card span" id="hw-disks">
      <h3>Disks</h3>
      <table className="compact">
        <thead>
          <tr>
            <th>Target</th>
            <th>Bus</th>
            <th>Source</th>
            <th className="num">Size</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {disks.map((k) => (
            <tr key={k.target}>
              <td className="mono">{k.target}</td>
              <td>{[k.bus, k.format, k.readOnly ? "read-only" : ""].filter(Boolean).join(" · ")}</td>
              <td className="mono wrap">{k.source}</td>
              <td className="num">{k.capacity ? bytes(k.capacity) : ""}</td>
              <td className="actions">
                <button className="link" disabled={busy} onClick={() => setResizing(k)}>
                  Resize
                </button>
                <button className="link" disabled={busy || !k.source || k.source.startsWith("/dev/")} onClick={() => setMoving(k)}>
                  Move
                </button>
                <button className="link" disabled={busy} onClick={() => setDetaching(k)}>
                  Detach
                </button>
              </td>
            </tr>
          ))}
          {disks.length === 0 && (
            <tr>
              <td className="muted">No disks.</td>
            </tr>
          )}
        </tbody>
      </table>
      <footer style={{ justifyContent: "flex-start" }}>
        <button onClick={() => setAdding(true)}>Add disk</button>
      </footer>
      {adding && <AddDisk d={d} send={send} onClose={() => setAdding(false)} />}
      {moving && <DiskMoveDialog d={d} disk={moving} onClose={() => setMoving(undefined)} onDone={onChanged} />}
      {resizing && <ResizeDisk disk={resizing} send={send} onClose={() => setResizing(undefined)} />}
      {detaching && (
        <Modal title={`Detach ${detaching.target}?`} onClose={() => setDetaching(undefined)}>
          <DetachBody disk={detaching} d={d} send={send} run={run} onClose={() => setDetaching(undefined)} />
        </Modal>
      )}
    </section>
  );
}

function DetachBody({ disk, d, send, onClose }: { disk: DiskT; d: VMDetail; send: Send; run: CardProps["run"]; onClose: () => void }) {
  const [del, setDel] = useState(false);
  const act = useAction();
  return (
    <>
      <p>
        <span className="mono">{disk.source}</span> is removed from the VM{d.state !== "shutoff" ? " while it runs. The guest should unmount it first" : ""}.
      </p>
      {!disk.readOnly && (
        <label className="check">
          <input type="checkbox" checked={del} onChange={(e) => setDel(e.target.checked)} />
          Also delete the volume if it is in a storage pool (cannot be undone)
        </label>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="danger"
          disabled={act.busy}
          onClick={async () => {
            if (await act.run(() => send("DELETE", `/disks/${disk.target}${del ? "?delete=1" : ""}`))) onClose();
          }}
        >
          Detach
        </button>
      </footer>
    </>
  );
}

function ResizeDisk({ disk, send, onClose }: { disk: DiskT; send: Send; onClose: () => void }) {
  const cur = Math.ceil((disk.capacity ?? 0) / 2 ** 30);
  const [size, setSize] = useState(cur + 1);
  const act = useAction();
  return (
    <Modal title={`Resize ${disk.target}`} onClose={onClose}>
      <Field label="New size (GiB)" hint={`Currently ${disk.capacity ? bytes(disk.capacity) : "unknown"}. Disks can only grow; the guest must then extend its partition and filesystem.`}>
        <input type="number" min={cur + 1} value={size} onChange={(e) => setSize(Number(e.target.value))} />
      </Field>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="primary"
          disabled={act.busy || size <= (disk.capacity ?? 0) / 2 ** 30}
          onClick={async () => {
            if (await act.run(() => send("POST", `/disks/${disk.target}/resize`, { sizeGiB: size }))) onClose();
          }}
        >
          Resize
        </button>
      </footer>
    </Modal>
  );
}

function usePools(node: number) {
  const [pools, setPools] = useState<Pool[]>([]);
  useEffect(() => {
    get<Pool[]>(`/nodes/${node}/pools`).then((p) => setPools(p.filter((x) => x.state === "running")), () => {});
  }, [node]);
  return pools;
}

function AddDisk({ d, send, onClose }: { d: VMDetail; send: Send; onClose: () => void }) {
  const pools = usePools(d.node);
  const [mode, setMode] = useState<"new" | "existing">("new");
  const [pool, setPool] = useState("");
  const [vols, setVols] = useState<Volume[]>([]);
  const [volume, setVolume] = useState("");
  const [name, setName] = useState(`${d.name}-disk${d.disks.length + 1}.qcow2`);
  const [size, setSize] = useState(10);
  const [format, setFormat] = useState("qcow2");
  const [bus, setBus] = useState("virtio");
  const [cache, setCache] = useState("");
  const [readonly, setReadonly] = useState(false);
  const act = useAction();
  useEffect(() => {
    if (!pool && pools.length) setPool(pools.find((p) => p.name === "default")?.name ?? pools[0].name);
  }, [pools, pool]);
  useEffect(() => {
    if (!pool) return;
    get<Volume[]>(`/nodes/${d.node}/pools/${encodeURIComponent(pool)}/volumes`).then(
      (v) => {
        const free = v.filter((x) => !d.disks.some((k) => k.source === x.path));
        setVols(free);
        setVolume(free[0]?.name ?? "");
      },
      () => setVols([]),
    );
  }, [pool, d.node, d.disks]);
  const submit = () =>
    act.run(async () => {
      await send("POST", "/disks", {
        pool,
        bus,
        cache: cache || undefined,
        readonly,
        ...(mode === "new" ? { newVolume: { name, sizeGiB: size, format } } : { volume }),
      });
      onClose();
    });
  return (
    <Modal title="Add disk" onClose={onClose}>
      <div className="segmented">
        <button className={mode === "new" ? "on" : ""} onClick={() => setMode("new")}>
          New volume
        </button>
        <button className={mode === "existing" ? "on" : ""} onClick={() => setMode("existing")}>
          Existing volume
        </button>
      </div>
      <Field label="Storage pool">
        <select value={pool} onChange={(e) => setPool(e.target.value)}>
          {pools.map((p) => (
            <option key={p.name}>{p.name}</option>
          ))}
        </select>
      </Field>
      {mode === "new" ? (
        <div className="grid2">
          <Field label="Volume name">
            <input value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Size (GiB)">
            <input type="number" min={1} value={size} onChange={(e) => setSize(Number(e.target.value))} />
          </Field>
          <Field label="Format">
            <select value={format} onChange={(e) => setFormat(e.target.value)}>
              <option>qcow2</option>
              <option>raw</option>
            </select>
          </Field>
        </div>
      ) : (
        <Field label="Volume">
          <select value={volume} onChange={(e) => setVolume(e.target.value)}>
            {vols.map((v) => (
              <option key={v.name} value={v.name}>
                {v.name} ({bytes(v.capacity)})
              </option>
            ))}
          </select>
        </Field>
      )}
      <div className="grid2">
        <Field label="Bus" hint={d.state !== "shutoff" && bus !== "virtio" ? "This bus cannot be hot-plugged; the disk appears after a restart." : undefined}>
          <select value={bus} onChange={(e) => setBus(e.target.value)}>
            <option value="virtio">virtio</option>
            <option value="scsi">scsi</option>
            <option value="sata">sata</option>
          </select>
        </Field>
        <Field label="Cache">
          <select value={cache} onChange={(e) => setCache(e.target.value)}>
            <option value="">default</option>
            <option>none</option>
            <option>writeback</option>
            <option>writethrough</option>
            <option>directsync</option>
            <option>unsafe</option>
          </select>
        </Field>
      </div>
      <label className="check">
        <input type="checkbox" checked={readonly} onChange={(e) => setReadonly(e.target.checked)} />
        Read-only
      </label>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={act.busy || !pool || (mode === "new" ? !name : !volume)} onClick={submit}>
          Add disk
        </button>
      </footer>
    </Modal>
  );
}

// --- CD-ROM ---

function CDROMs({ d, cdroms, send, run, busy }: CardProps & { cdroms: DiskT[] }) {
  const [picking, setPicking] = useState<DiskT>();
  return (
    <section className="card" id="hw-cdrom">
      <h3>CD-ROM</h3>
      <table className="compact">
        <tbody>
          {cdroms.map((k) => (
            <tr key={k.target}>
              <td className="mono">{k.target}</td>
              <td>{k.bus}</td>
              <td className="mono wrap">{k.source || <span className="muted">(no media)</span>}</td>
              <td className="actions">
                <button className="link" disabled={busy} onClick={() => setPicking(k)}>
                  {k.source ? "Change media" : "Insert media"}
                </button>
                {k.source && (
                  <button className="link" disabled={busy} onClick={() => run(() => send("PUT", `/cdrom/${k.target}`, null))}>
                    Eject
                  </button>
                )}
              </td>
            </tr>
          ))}
          {cdroms.length === 0 && (
            <tr>
              <td className="muted">No CD-ROM drive.</td>
            </tr>
          )}
        </tbody>
      </table>
      {cdroms.length === 0 && (
        <footer style={{ justifyContent: "flex-start" }}>
          <button disabled={busy} onClick={() => run(() => send("POST", "/cdrom"))}>
            Add CD-ROM drive
          </button>
          {d.state !== "shutoff" && <span className="muted">A SATA drive cannot be hot-plugged; it appears after a restart.</span>}
        </footer>
      )}
      {picking && <PickISO d={d} drive={picking} send={send} onClose={() => setPicking(undefined)} />}
    </section>
  );
}

function PickISO({ d, drive, send, onClose }: { d: VMDetail; drive: DiskT; send: Send; onClose: () => void }) {
  const pools = usePools(d.node);
  const [isos, setIsos] = useState<{ pool: string; vol: Volume }[]>();
  const [choice, setChoice] = useState("");
  const act = useAction();
  useEffect(() => {
    Promise.all(
      pools.map((p) =>
        get<Volume[]>(`/nodes/${d.node}/pools/${encodeURIComponent(p.name)}/volumes`)
          .then((vs) => vs.filter((v) => /\.iso$/i.test(v.name) || v.format === "iso").map((vol) => ({ pool: p.name, vol })))
          .catch(() => []),
      ),
    ).then((all) => {
      const list = all.flat();
      setIsos(list);
      setChoice((c) => c || (list[0] ? `${list[0].pool}/${list[0].vol.name}` : ""));
    });
  }, [pools, d.node]);
  return (
    <Modal title={`Media for ${drive.target}`} onClose={onClose}>
      {isos && isos.length === 0 ? (
        <p className="muted">No .iso volumes found in the node's storage pools. Upload one on the Storage page first.</p>
      ) : (
        <Field label="ISO image" hint={drive.source ? `Replaces ${drive.source}.` : undefined}>
          <select value={choice} onChange={(e) => setChoice(e.target.value)}>
            {(isos ?? []).map(({ pool, vol }) => (
              <option key={pool + "/" + vol.name} value={pool + "/" + vol.name}>
                {pool} / {vol.name} ({bytes(vol.capacity)})
              </option>
            ))}
          </select>
        </Field>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="primary"
          disabled={act.busy || !choice}
          onClick={async () => {
            const i = choice.indexOf("/");
            if (await act.run(() => send("PUT", `/cdrom/${drive.target}`, { pool: choice.slice(0, i), volume: choice.slice(i + 1) }))) onClose();
          }}
        >
          Insert
        </button>
      </footer>
    </Modal>
  );
}

// --- network interfaces ---

function NICs({ d, send, run, busy }: CardProps) {
  const [form, setForm] = useState<{ nic?: NicT } | null>(null);
  const [removing, setRemoving] = useState<NicT>();
  return (
    <section className="card span" id="hw-nics">
      <h3>Network interfaces</h3>
      <table className="compact">
        <thead>
          <tr>
            <th>MAC</th>
            <th>Network</th>
            <th>Model</th>
            <th>Link</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {d.nics.map((n) => (
            <tr key={n.mac}>
              <td className="mono">{n.mac}</td>
              <td>
                {n.source} <span className="muted">({n.type})</span>
              </td>
              <td>{n.model}</td>
              <td>{n.linkState}</td>
              <td className="actions">
                <button className="link" disabled={busy} onClick={() => setForm({ nic: n })}>
                  Change network
                </button>
                <button className="link" disabled={busy} onClick={() => run(() => send("PUT", `/nics/${n.mac}`, { linkState: n.linkState === "down" ? "up" : "down" }))}>
                  Link {n.linkState === "down" ? "up" : "down"}
                </button>
                <button className="link" disabled={busy} onClick={() => setRemoving(n)}>
                  Remove
                </button>
              </td>
            </tr>
          ))}
          {d.nics.length === 0 && (
            <tr>
              <td className="muted">No network interfaces.</td>
            </tr>
          )}
        </tbody>
      </table>
      <footer style={{ justifyContent: "flex-start" }}>
        <button onClick={() => setForm({})}>Add interface</button>
      </footer>
      {form && <NICForm d={d} nic={form.nic} send={send} onClose={() => setForm(null)} />}
      {removing && (
        <Modal title={`Remove ${removing.mac}?`} onClose={() => setRemoving(undefined)}>
          <p>The interface is unplugged from the VM{d.state !== "shutoff" ? " while it runs" : ""}.</p>
          <RemoveNIC nic={removing} send={send} onClose={() => setRemoving(undefined)} />
        </Modal>
      )}
    </section>
  );
}

function RemoveNIC({ nic, send, onClose }: { nic: NicT; send: Send; onClose: () => void }) {
  const act = useAction();
  return (
    <>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="danger"
          disabled={act.busy}
          onClick={async () => {
            if (await act.run(() => send("DELETE", `/nics/${nic.mac}`))) onClose();
          }}
        >
          Remove
        </button>
      </footer>
    </>
  );
}

function NICForm({ d, nic, send, onClose }: { d: VMDetail; nic?: NicT; send: Send; onClose: () => void }) {
  const [nets, setNets] = useState<Network[]>([]);
  const [kind, setKind] = useState<"network" | "bridge">(nic && nic.type === "bridge" ? "bridge" : "network");
  const [network, setNetwork] = useState(nic && nic.type === "network" ? nic.source : "");
  const [bridge, setBridge] = useState(nic && nic.type === "bridge" ? nic.source : "");
  const [model, setModel] = useState("virtio");
  const [mac, setMac] = useState("");
  const act = useAction();
  useEffect(() => {
    get<Network[]>(`/nodes/${d.node}/networks`).then(
      (n) => {
        setNets(n);
        setNetwork((cur) => cur || (n.find((x) => x.name === "default") ?? n[0])?.name || "");
      },
      () => {},
    );
  }, [d.node]);
  const target = kind === "network" ? { network } : { bridge };
  return (
    <Modal title={nic ? `Change network of ${nic.mac}` : "Add network interface"} onClose={onClose}>
      <div className="segmented">
        <button className={kind === "network" ? "on" : ""} onClick={() => setKind("network")}>
          Virtual network
        </button>
        <button className={kind === "bridge" ? "on" : ""} onClick={() => setKind("bridge")}>
          Host bridge
        </button>
      </div>
      {kind === "network" ? (
        <Field label="Network">
          <select value={network} onChange={(e) => setNetwork(e.target.value)}>
            {nets.map((n) => (
              <option key={n.name} value={n.name} disabled={!n.active}>
                {n.name}
                {n.active ? "" : " (inactive)"}
              </option>
            ))}
          </select>
        </Field>
      ) : (
        <Field label="Bridge device" hint="For example br0 or virbr0.">
          <input value={bridge} onChange={(e) => setBridge(e.target.value)} />
        </Field>
      )}
      {!nic && (
        <div className="grid2">
          <Field label="Model">
            <select value={model} onChange={(e) => setModel(e.target.value)}>
              <option>virtio</option>
              <option>e1000e</option>
              <option>e1000</option>
              <option>rtl8139</option>
            </select>
          </Field>
          <Field label="MAC address" hint="Leave empty to generate one.">
            <input className="mono" placeholder="52:54:00:12:34:56" value={mac} onChange={(e) => setMac(e.target.value)} />
          </Field>
        </div>
      )}
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button
          className="primary"
          disabled={act.busy || !(kind === "network" ? network : bridge)}
          onClick={async () => {
            const ok = await act.run(() =>
              nic ? send("PUT", `/nics/${nic.mac}`, target) : send("POST", "/nics", { ...target, model, mac: mac || undefined }),
            );
            if (ok) onClose();
          }}
        >
          {nic ? "Apply" : "Add"}
        </button>
      </footer>
    </Modal>
  );
}

// --- boot order ---

interface BootDev {
  id: string; // disk target or MAC, as the API expects
  label: string;
}

function bootDevices(d: VMDetail): { all: BootDev[]; order: string[]; legacy: boolean } {
  const all: BootDev[] = [
    ...d.disks.map((k) => ({ id: k.target, label: `${k.device === "cdrom" ? "CD-ROM" : "Disk"} ${k.target}${k.source ? " (" + k.source.split("/").pop() + ")" : ""}` })),
    ...d.nics.map((n) => ({ id: n.mac, label: `Network ${n.mac} (${n.source})` })),
  ];
  const ranked = [
    ...d.disks.map((k) => [k.bootOrder ?? 0, k.target] as const),
    ...d.nics.map((n) => [n.bootOrder ?? 0, n.mac] as const),
  ]
    .filter(([o]) => o > 0)
    .sort((a, b) => a[0] - b[0])
    .map(([, id]) => id);
  if (ranked.length > 0 || d.boot.length === 0) return { all, order: ranked, legacy: false };
  // Legacy <os><boot dev=.../> list: expand each class of device in order.
  const order: string[] = [];
  for (const dev of d.boot) {
    if (dev === "hd") order.push(...d.disks.filter((k) => k.device === "disk").map((k) => k.target));
    if (dev === "cdrom") order.push(...d.disks.filter((k) => k.device === "cdrom").map((k) => k.target));
    if (dev === "network") order.push(...d.nics.map((n) => n.mac));
  }
  return { all, order: order.filter((x, i) => order.indexOf(x) === i), legacy: true };
}

function Boot({ d, send, run, busy }: CardProps) {
  const { all, order: current, legacy } = bootDevices(d);
  const key = current.join(",");
  const [order, setOrder] = useState(current);
  const [dirty, setDirty] = useState(false);
  useEffect(() => {
    if (!dirty) setOrder(key ? key.split(",") : []);
  }, [key, dirty]);
  const label = (id: string) => all.find((x) => x.id === id)?.label ?? id;
  const move = (i: number, by: number) => {
    const o = [...order];
    [o[i], o[i + by]] = [o[i + by], o[i]];
    setOrder(o);
    setDirty(true);
  };
  const rest = all.filter((x) => !order.includes(x.id));
  return (
    <section className="card" id="hw-boot">
      <h3>Boot order</h3>
      {legacy && <p className="muted">This VM uses the older class-based boot list ({d.boot.join(", ")}). Saving switches it to a per-device order.</p>}
      <table className="compact">
        <tbody>
          {order.map((id, i) => (
            <tr key={id}>
              <td className="num">{i + 1}.</td>
              <td>{label(id)}</td>
              <td className="actions">
                <button className="ghost" disabled={i === 0} onClick={() => move(i, -1)} aria-label="Move up">
                  ↑
                </button>
                <button className="ghost" disabled={i === order.length - 1} onClick={() => move(i, 1)} aria-label="Move down">
                  ↓
                </button>
                <button
                  className="ghost"
                  onClick={() => {
                    setOrder(order.filter((x) => x !== id));
                    setDirty(true);
                  }}
                  aria-label="Remove from boot order"
                >
                  ✕
                </button>
              </td>
            </tr>
          ))}
          {order.length === 0 && (
            <tr>
              <td className="muted">No boot order set; the firmware decides.</td>
            </tr>
          )}
          {rest.map((x) => (
            <tr key={x.id}>
              <td />
              <td className="muted">{x.label}</td>
              <td className="actions">
                <button
                  className="link"
                  onClick={() => {
                    setOrder([...order, x.id]);
                    setDirty(true);
                  }}
                >
                  Add
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <footer>
        {d.state !== "shutoff" && <span className="muted">Takes effect at the next start.</span>}
        <button
          disabled={!dirty}
          onClick={() => {
            setOrder(current);
            setDirty(false);
          }}
        >
          Revert
        </button>
        <button
          className="primary"
          disabled={!dirty || busy || order.length === 0}
          onClick={async () => {
            if (await run(() => send("PUT", "/boot", { order }))) setDirty(false);
          }}
        >
          Save boot order
        </button>
      </footer>
    </section>
  );
}
