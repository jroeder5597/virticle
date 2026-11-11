// Hardware tab: CPU, memory, disks, CD-ROM, network interfaces and boot order.
// Every change is sent to the server, which applies it to the running VM and
// the saved definition where libvirt allows and says which one happened.
import { useEffect, useState } from "react";
import { type LucideIcon, ArrowDown, ArrowLeftRight, ArrowUp, ArrowUpFromLine, Disc3, Link2, Link2Off, Maximize2, Network as NetIcon, Trash2, X } from "lucide-react";
import { api, bytes, get, Network, Pool, Volume, VMDetail } from "../api";
import { useRole } from "../auth";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import FirmwareCard from "./Firmware";
import AdvancedCard from "./Advanced";
import PassthroughCard from "./Passthrough";
import DiskMoveDialog from "./DiskMove";
import PoolSelect from "../PoolSelect";
import BridgeSelect from "../BridgeSelect";
import "./hw.css";

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
  if (r.needsRestart) s = "Saved. Takes effect after a restart.";
  else if (r.applied === "config" && !running) s = "Saved.";
  else s = "Applied and saved.";
  return r.note ? `${s} ${r.note}` : s;
}

/** Send/run/outcome for one set of hardware cards: the Hardware tab has one, each Edit Settings dialog has its own. */
function useHw(d: VMDetail, base: string, onChanged: () => void) {
  const act = useAction();
  const [outcome, setOutcome] = useState<{ text: string; restart: boolean }>();
  const running = d.state !== "shutoff";
  const send: Send = async (method, path, body) => {
    const r = await api<HwResult>(method, base + path, body);
    setOutcome({ text: summary(r, running), restart: r.needsRestart });
    onChanged();
    return r;
  };
  const run = (fn: () => Promise<unknown>) => act.run(fn);
  return { send, run, busy: act.busy, error: act.error, outcome };
}

export type HwArea = "cpu" | "disks" | "nics" | "cdrom" | "boot" | "firmware" | "advanced" | "passthrough";

function Outcome({ hw }: { hw: ReturnType<typeof useHw> }) {
  return (
    <>
      {hw.outcome && <div className={hw.outcome.restart ? "note" : "muted"}>{hw.outcome.text}</div>}
      <ErrorBox error={hw.error} />
    </>
  );
}

/** One hardware section; `bare` renders it for a dialog (no card frame, no element ids). */
function Section({ area, d, base, onChanged, hw, bare }: { area: HwArea; d: VMDetail; base: string; onChanged: () => void; hw: ReturnType<typeof useHw>; bare?: boolean }) {
  const p = { d, send: hw.send, run: hw.run, busy: hw.busy, bare };
  switch (area) {
    case "cpu":
      return <CpuMemory {...p} />;
    case "boot":
      return <Boot {...p} />;
    case "disks":
      return <Disks {...p} disks={d.disks.filter((k) => k.device === "disk")} onChanged={onChanged} />;
    case "nics":
      return <NICs {...p} />;
    case "cdrom":
      return <CDROMs {...p} cdroms={d.disks.filter((k) => k.device === "cdrom")} />;
    case "firmware":
      return <FirmwareCard d={d} base={base} onChanged={onChanged} bare={bare} />;
    case "advanced":
      return <AdvancedCard d={d} base={base} onChanged={onChanged} bare={bare} />;
    case "passthrough":
      return <PassthroughCard d={d} base={base} onChanged={onChanged} bare={bare} />;
  }
}

/** Dialog sizes: tables and forms with many columns get the wide one. */
const wideAreas: HwArea[] = ["disks", "nics", "cdrom", "boot", "advanced", "passthrough"];

/**
 * The same section as on the Hardware tab, in a themed dialog (Actions, Edit Settings). It reads the same VM detail
 * and reports changes through the same `onChanged`, so the tab and the dialog always agree.
 */
export function HardwareDialog({ area, title, d, base, onChanged, onClose }: { area: HwArea; title: string; d: VMDetail; base: string; onChanged: () => void; onClose: () => void }) {
  const hw = useHw(d, base, onChanged);
  const viewer = useRole() === "viewer";
  return (
    <Modal title={title} onClose={onClose} wide={wideAreas.includes(area)}>
      <div className="hwdlg">
        <fieldset className="plain" disabled={viewer}>
          <Outcome hw={hw} />
          <Section area={area} d={d} base={base} onChanged={onChanged} hw={hw} bare />
        </fieldset>
      </div>
    </Modal>
  );
}

export default function Hardware({ d, base, onChanged }: { d: VMDetail; base: string; onChanged: () => void }) {
  const viewer = useRole() === "viewer";
  const hw = useHw(d, base, onChanged);
  const S = (area: HwArea) => <Section area={area} d={d} base={base} onChanged={onChanged} hw={hw} />;
  return (
    // A viewer sees the hardware but every control is disabled, not just its buttons hidden.
    <fieldset className="plain hwwrap" disabled={viewer}>
      <Outcome hw={hw} />
      <div className="hw">
        <div className="hwcol">
          {S("cpu")}
          {S("firmware")}
        </div>
        <div className="hwcol">
          {S("boot")}
          {S("cdrom")}
        </div>
        {S("disks")}
        {S("nics")}
        {S("advanced")}
        {S("passthrough")}
      </div>
    </fieldset>
  );
}

interface CardProps {
  d: VMDetail;
  send: Send;
  run: (fn: () => Promise<unknown>) => Promise<boolean>;
  busy: boolean;
  /** Shown in a dialog: no card frame, no element ids (the tab may be showing the same card). */
  bare?: boolean;
}

/** Card frame with a title row: title left, actions right. In a dialog the dialog's own title replaces the heading. */
function Card({ id, title, bare, actions, className, children }: { id: string; title: string; bare?: boolean; actions?: React.ReactNode; className?: string; children: React.ReactNode }) {
  return (
    <section className={(bare ? "hwbare" : "card") + (className ? " " + className : "")} id={bare ? undefined : id} aria-label={bare ? title : undefined}>
      <div className="chead">
        {!bare && <h3>{title}</h3>}
        {actions && <div className="chead-actions">{actions}</div>}
      </div>
      {children}
    </section>
  );
}

/** A labelled icon button for a table row. */
function RowBtn({ label, icon: Icon, ...rest }: { label: string; icon: LucideIcon } & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button className="iconbtn" aria-label={label} title={label} {...rest}>
      <Icon size={15} aria-hidden="true" />
    </button>
  );
}

function CpuMemory({ d, send, run, busy, bare }: CardProps) {
  const [editing, setEditing] = useState(!!bare);
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
    if (ok && !bare) setEditing(false);
  };
  const num = (set: (n: number) => void) => (e: React.ChangeEvent<HTMLInputElement>) => set(Number(e.target.value));
  return (
    <Card id="hw-cpu" title="CPU and memory" bare={bare} actions={!editing && <button onClick={start}>Edit</button>}>
      {!editing ? (
        <>
          <dl className="hwdl">
            <dt>vCPUs</dt>
            <dd>
              {d.vcpus} <span className="muted">(up to {d.maxVcpus})</span>
            </dd>
            <dt>Memory</dt>
            <dd>
              {bytes(d.currentMemoryKiB * 1024)} <span className="muted">(up to {bytes(d.memoryKiB * 1024)})</span>
            </dd>
          </dl>
        </>
      ) : (
        <>
          <div className="grid2">
            <Field label="vCPUs now">
              <input type="number" min={1} value={vcpus} onChange={num(setVcpus)} />
            </Field>
            <Field label="Maximum vCPUs" hint={d.state !== "shutoff" ? "Applies after a restart." : undefined}>
              <input type="number" min={1} value={maxV} onChange={num(setMaxV)} />
            </Field>
            <Field label="Memory now (MiB)" hint={d.state !== "shutoff" ? "Live, up to the running maximum." : undefined}>
              <input type="number" min={64} value={mem} onChange={num(setMem)} />
            </Field>
            <Field label="Maximum memory (MiB)" hint={d.state !== "shutoff" ? "Applies after a restart." : undefined}>
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
    </Card>
  );
}

// --- disks ---

function Disks({ d, disks, send, run, busy, onChanged, bare }: CardProps & { disks: DiskT[]; onChanged: () => void }) {
  const [adding, setAdding] = useState(false);
  const [resizing, setResizing] = useState<DiskT>();
  const [detaching, setDetaching] = useState<DiskT>();
  const [moving, setMoving] = useState<DiskT>();
  return (
    <Card id="hw-disks" title="Disks" bare={bare} className="full" actions={<button onClick={() => setAdding(true)}>Add disk</button>}>
      <div className="table-scroll">
      <table className="compact hwt hwt-stack">
        <thead>
          <tr>
            <th className="fit">Target</th>
            <th className="fit">Bus</th>
            <th>Source</th>
            <th className="num fit">Size</th>
            <th className="fit" />
          </tr>
        </thead>
        <tbody>
          {disks.map((k) => (
            <tr key={k.target}>
              <td className="mono fit">{k.target}</td>
              <td className="fit">{[k.bus, k.format, k.readOnly ? "read-only" : ""].filter(Boolean).join(" · ")}</td>
              <td className="mono wrap">{k.source}</td>
              <td className="num fit">{k.capacity ? bytes(k.capacity) : ""}</td>
              <td className="actions fit">
                <RowBtn label="Resize" icon={Maximize2} disabled={busy} onClick={() => setResizing(k)} />
                <RowBtn label="Move" icon={ArrowLeftRight} disabled={busy || !k.source || k.source.startsWith("/dev/")} onClick={() => setMoving(k)} />
                <RowBtn label="Detach" icon={Trash2} disabled={busy} onClick={() => setDetaching(k)} />
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
      </div>
      {adding && <AddDisk d={d} send={send} onClose={() => setAdding(false)} />}
      {moving && <DiskMoveDialog d={d} disk={moving} onClose={() => setMoving(undefined)} onDone={onChanged} />}
      {resizing && <ResizeDisk disk={resizing} send={send} onClose={() => setResizing(undefined)} />}
      {detaching && (
        <Modal title={`Detach ${detaching.target}?`} onClose={() => setDetaching(undefined)}>
          <DetachBody disk={detaching} d={d} send={send} run={run} onClose={() => setDetaching(undefined)} />
        </Modal>
      )}
    </Card>
  );
}

function DetachBody({ disk, d, send, onClose }: { disk: DiskT; d: VMDetail; send: Send; run: CardProps["run"]; onClose: () => void }) {
  const [del, setDel] = useState(false);
  const act = useAction();
  return (
    <>
      <p>
        <span className="mono">{disk.source}</span> is removed from the VM{d.state !== "shutoff" ? " while it runs; unmount it in the guest first" : ""}.
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
      <Field label="New size (GiB)" hint={`Now ${disk.capacity ? bytes(disk.capacity) : "unknown"}. Disks only grow.`}>
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

/** The node's storage pools; inactive ones only when `all` (a picker that lists them as unavailable). */
function usePools(node: number, all = false) {
  const [pools, setPools] = useState<Pool[]>([]);
  useEffect(() => {
    get<Pool[]>(`/nodes/${node}/pools`).then((p) => setPools(all ? p : p.filter((x) => x.state === "running")), () => {});
  }, [node, all]);
  return pools;
}

function AddDisk({ d, send, onClose }: { d: VMDetail; send: Send; onClose: () => void }) {
  const pools = usePools(d.node, true);
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
    const live = pools.filter((p) => p.state === "running");
    if (!pool && live.length) setPool(live.find((p) => p.name === "default")?.name ?? live[0].name);
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
      <Field label="Storage pool" group>
        <PoolSelect pools={pools} value={pool} onChange={setPool} label="Storage pool" needBytes={mode === "new" ? size * 2 ** 30 : undefined} />
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
            <Select
              label="Format"
              value={format}
              onChange={setFormat}
              options={[
                { value: "qcow2", label: "qcow2" },
                { value: "raw", label: "raw" },
              ]}
            />
          </Field>
        </div>
      ) : (
        <Field label="Volume">
          <Select label="Volume" value={volume} onChange={setVolume} options={vols.map((v) => ({ value: v.name, label: `${v.name} (${bytes(v.capacity)})` }))} />
        </Field>
      )}
      <div className="grid2">
        <Field label="Bus" hint={d.state !== "shutoff" && bus !== "virtio" ? "Appears after a restart." : undefined}>
          <Select
            label="Bus"
            value={bus}
            onChange={setBus}
            options={[
              { value: "virtio", label: "virtio" },
              { value: "scsi", label: "scsi" },
              { value: "sata", label: "sata" },
            ]}
          />
        </Field>
        <Field label="Cache">
          <Select label="Cache" value={cache} onChange={setCache} options={["", "none", "writeback", "writethrough", "directsync", "unsafe"].map((c) => ({ value: c, label: c || "default" }))} />
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

function CDROMs({ d, cdroms, send, run, busy, bare }: CardProps & { cdroms: DiskT[] }) {
  const [picking, setPicking] = useState<DiskT>();
  return (
    <Card id="hw-cdrom" title="CD-ROM" bare={bare}>
      <div className="table-scroll">
      <table className="compact hwt hwt-stack">
        <tbody>
          {cdroms.map((k) => (
            <tr key={k.target}>
              <td className="mono fit">{k.target}</td>
              <td className="fit">{k.bus}</td>
              <td className="mono wrap">{k.source || <span className="muted">(no media)</span>}</td>
              <td className="actions fit">
                <RowBtn label={k.source ? "Change media" : "Insert media"} icon={Disc3} disabled={busy} onClick={() => setPicking(k)} />
                {k.source && <RowBtn label="Eject" icon={ArrowUpFromLine} disabled={busy} onClick={() => run(() => send("PUT", `/cdrom/${k.target}`, null))} />}
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
      </div>
      {cdroms.length < 4 && (
        <div className="hwadd">
          <button disabled={busy} onClick={() => run(() => send("POST", "/cdrom"))}>
            Add CD-ROM drive
          </button>
          {d.state !== "shutoff" && <span className="muted">Appears after a restart.</span>}
        </div>
      )}
      {picking && <PickISO d={d} drive={picking} send={send} onClose={() => setPicking(undefined)} />}
    </Card>
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
        <p className="muted">No ISO volumes in this node's pools. Upload one on the Storage page.</p>
      ) : (
        <Field label="ISO image" hint={drive.source ? `Replaces ${drive.source}.` : undefined}>
          <Select label="ISO image" value={choice} onChange={setChoice} options={(isos ?? []).map(({ pool, vol }) => ({ value: pool + "/" + vol.name, label: `${pool} / ${vol.name} (${bytes(vol.capacity)})` }))} />
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

function NICs({ d, send, run, busy, bare }: CardProps) {
  const [form, setForm] = useState<{ nic?: NicT } | null>(null);
  const [removing, setRemoving] = useState<NicT>();
  return (
    <Card id="hw-nics" title="Network interfaces" bare={bare} className="full" actions={<button onClick={() => setForm({})}>Add interface</button>}>
      <div className="table-scroll">
      <table className="compact hwt hwt-stack">
        <thead>
          <tr>
            <th className="fit">MAC</th>
            <th>Network</th>
            <th className="fit">Model</th>
            <th className="fit">Link</th>
            <th className="fit" />
          </tr>
        </thead>
        <tbody>
          {d.nics.map((n) => (
            <tr key={n.mac}>
              <td className="mono fit">{n.mac}</td>
              <td className="wrap">
                {n.source} <span className="muted">({n.type})</span>
              </td>
              <td className="fit">{n.model}</td>
              <td className="fit">{n.linkState}</td>
              <td className="actions fit">
                <RowBtn label="Change network" icon={NetIcon} disabled={busy} onClick={() => setForm({ nic: n })} />
                <RowBtn
                  label={`Link ${n.linkState === "down" ? "up" : "down"}`}
                  icon={n.linkState === "down" ? Link2 : Link2Off}
                  disabled={busy}
                  onClick={() => run(() => send("PUT", `/nics/${n.mac}`, { linkState: n.linkState === "down" ? "up" : "down" }))}
                />
                <RowBtn label="Remove" icon={Trash2} disabled={busy} onClick={() => setRemoving(n)} />
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
      </div>
      {form && <NICForm d={d} nic={form.nic} send={send} onClose={() => setForm(null)} />}
      {removing && (
        <Modal title={`Remove ${removing.mac}?`} onClose={() => setRemoving(undefined)}>
          <p>The interface is unplugged from the VM{d.state !== "shutoff" ? " while it runs" : ""}.</p>
          <RemoveNIC nic={removing} send={send} onClose={() => setRemoving(undefined)} />
        </Modal>
      )}
    </Card>
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
          <Select label="Network" value={network} onChange={setNetwork} options={nets.map((n) => ({ value: n.name, label: n.name + (n.active ? "" : " (inactive)"), disabled: !n.active }))} />
        </Field>
      ) : (
        <Field label="Bridge device" group>
          <BridgeSelect node={d.node} value={bridge} onChange={setBridge} networks={nets} />
        </Field>
      )}
      {!nic && (
        <div className="grid2">
          <Field label="Model">
            <Select label="Model" value={model} onChange={setModel} options={["virtio", "e1000e", "e1000", "rtl8139"].map((m) => ({ value: m, label: m }))} />
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

function Boot({ d, send, run, busy, bare }: CardProps) {
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
    <Card id="hw-boot" title="Boot order" bare={bare}>
      {legacy && <p className="muted">Class-based list ({d.boot.join(", ")}); saving makes it per device.</p>}
      <div className="table-scroll">
      <table className="compact hwt">
        <tbody>
          {order.map((id, i) => (
            <tr key={id}>
              <td className="num fit">{i + 1}.</td>
              <td className="wrap">{label(id)}</td>
              <td className="actions fit">
                <RowBtn label="Move up" icon={ArrowUp} disabled={i === 0} onClick={() => move(i, -1)} />
                <RowBtn label="Move down" icon={ArrowDown} disabled={i === order.length - 1} onClick={() => move(i, 1)} />
                <RowBtn
                  label="Remove from boot order"
                  icon={X}
                  onClick={() => {
                    setOrder(order.filter((x) => x !== id));
                    setDirty(true);
                  }}
                />
              </td>
            </tr>
          ))}
          {order.length === 0 && (
            <tr>
              <td className="muted">None set; the firmware decides.</td>
            </tr>
          )}
          {rest.map((x) => (
            <tr key={x.id}>
              <td className="fit" />
              <td className="muted wrap">{x.label}</td>
              <td className="actions fit">
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
      </div>
      <footer className="hwfoot">
        {d.state !== "shutoff" && <span className="muted">Applies at the next start.</span>}
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
    </Card>
  );
}
