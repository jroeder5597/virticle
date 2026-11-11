import { FormEvent, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { bytes, get, Network, post, Task, Volume } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";

interface Inspect {
  kind: "disk" | "ova";
  file: string;
  format: string;
  virtualSize: number;
  actualSize: number;
  warning?: string;
  members?: { name: string; size: number }[];
}

interface Proposal {
  name: string;
  vcpus: number;
  memoryMiB: number;
  firmware: string;
  secureBoot: boolean;
  osType: string;
  pool: string;
  existingVolume: string;
}

interface Imported {
  kind: "disk" | "ova";
  volume?: string;
  volumes?: string[];
  proposal?: Proposal;
  warnings?: string[];
}

/** Convert a disk image (qcow2, raw, VMDK, VDI, VHDX) or an OVA that is already on the node into a qcow2 volume. */
export default function ImportDialog({ node, pool, volumes, initial, onClose, onDone }: { node: number; pool: string; volumes: Volume[]; initial?: string; onClose: () => void; onDone: () => void }) {
  const [mode, setMode] = useState<"volume" | "path">("volume");
  const [volume, setVolume] = useState(initial ?? "");
  const [path, setPath] = useState("");
  const [name, setName] = useState("");
  const [info, setInfo] = useState<Inspect | undefined>();
  const [done, setDone] = useState<Imported | undefined>();
  const [progress, setProgress] = useState<number | undefined>();
  const act = useAction();
  const inspectAct = useAction();
  const timer = useRef<number | undefined>();
  useEffect(() => () => window.clearInterval(timer.current), []);

  const source = mode === "volume" ? { pool, volume } : { path };
  const ready = mode === "volume" ? !!volume : path.startsWith("/");

  const inspect = () =>
    inspectAct.run(async () => {
      const i = await post<Inspect>(`/nodes/${node}/import/inspect`, { source });
      setInfo(i);
      if (!name) setName((mode === "volume" ? volume : path.split("/").pop() ?? "").replace(/\.(qcow2|raw|img|vmdk|vdi|vhdx|ova)$/i, "") + (i.kind === "disk" ? "-imported" : ""));
    });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const target = `${pool}/${info?.kind === "disk" ? name + (name.endsWith(".qcow2") ? "" : ".qcow2") : name}`;
    setProgress(0);
    timer.current = window.setInterval(async () => {
      try {
        const tasks = await get<Task[]>("/tasks");
        const t = tasks.find((x) => x.status === "running" && x.kind.startsWith("import") && x.target === target);
        if (t) setProgress(t.progress);
      } catch {
        /* the next tick retries */
      }
    }, 1500);
    const ok = await act.run(async () => setDone(await post<Imported>(`/nodes/${node}/import`, { source, pool, name })));
    window.clearInterval(timer.current);
    setProgress(undefined);
    if (ok) onDone();
  };

  if (done) return <Finished node={node} done={done} onClose={onClose} />;
  return (
    <Modal title="Import a disk image or OVA" onClose={onClose}>
      <form onSubmit={submit}>
        <p className="muted">
          The image must already be on the node: upload it or download it into a pool first, or point to a file inside a pool directory. The format is detected from the content, and the result is a new qcow2 volume in <strong>{pool}</strong>.
        </p>
        <div className="segmented">
          <button type="button" className={mode === "volume" ? "on" : ""} onClick={() => (setMode("volume"), setInfo(undefined))}>
            Volume in {pool}
          </button>
          <button type="button" className={mode === "path" ? "on" : ""} onClick={() => (setMode("path"), setInfo(undefined))}>
            Path on the node
          </button>
        </div>
        {mode === "volume" ? (
          <Field label="Source volume">
            <select
              value={volume}
              onChange={(e) => {
                setVolume(e.target.value);
                setInfo(undefined);
              }}
              required
            >
              <option value="">Choose a volume</option>
              {volumes.map((v) => (
                <option key={v.name} value={v.name}>
                  {v.name} ({bytes(v.capacity)})
                </option>
              ))}
            </select>
          </Field>
        ) : (
          <Field label="File path" hint="Must be inside the directory of a running pool of this node.">
            <input
              value={path}
              onChange={(e) => {
                setPath(e.target.value);
                setInfo(undefined);
              }}
              placeholder="/var/lib/libvirt/images/disk.vmdk"
              required
            />
          </Field>
        )}
        <div className="actions" style={{ marginBottom: 10 }}>
          <button type="button" disabled={!ready || inspectAct.busy} onClick={inspect}>
            {inspectAct.busy ? "Inspecting" : "Inspect"}
          </button>
        </div>
        <ErrorBox error={inspectAct.error} />
        {info && (
          <div className="muted" style={{ marginBottom: 10 }}>
            {info.kind === "ova" ? (
              <>OVA archive with {info.members?.length} files ({info.members?.map((m) => m.name).join(", ")}).</>
            ) : (
              <>
                Detected format <strong>{info.format}</strong>, virtual size {bytes(info.virtualSize)}, {bytes(info.actualSize)} on disk.
              </>
            )}
            {info.warning && <div className="note">{info.warning}</div>}
          </div>
        )}
        {info && (
          <Field label={info.kind === "ova" ? "VM name" : "New volume name"} hint={info.kind === "ova" ? 'Disks become "<name>-disk1.qcow2", ...' : '".qcow2" is added when missing.'}>
            <input value={name} onChange={(e) => setName(e.target.value)} required />
          </Field>
        )}
        {progress !== undefined && (
          <div className="progress">
            Converting {(progress * 100).toFixed(0)}%
            <div className="bar">
              <div style={{ width: progress * 100 + "%" }} />
            </div>
          </div>
        )}
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={!info || !name || act.busy}>
            {act.busy ? "Importing" : "Import"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}

function Finished({ node, done, onClose }: { node: number; done: Imported; onClose: () => void }) {
  const nav = useNavigate();
  const p = done.proposal;
  const [name, setName] = useState(p?.name ?? "");
  const [vcpus, setVcpus] = useState(p?.vcpus ?? 1);
  const [mem, setMem] = useState(p?.memoryMiB ?? 1024);
  const [network, setNetwork] = useState("");
  const [networks, setNetworks] = useState<Network[]>([]);
  const act = useAction();
  useEffect(() => {
    get<Network[]>(`/nodes/${node}/networks`).then((n) => setNetworks(n.filter((x) => x.active))).catch(() => {});
  }, [node]);
  if (!p) {
    return (
      <Modal title="Import finished" onClose={onClose}>
        <p>
          Created volume <strong className="mono">{done.volume}</strong>. Create a VM with "Existing volume" as its disk source to use it.
        </p>
        <footer>
          <button className="primary" onClick={onClose}>
            Close
          </button>
        </footer>
      </Modal>
    );
  }
  const create = () =>
    act.run(async () => {
      const r = await post<{ uuid: string }>(`/nodes/${node}/vms`, { ...p, name, vcpus, memoryMiB: mem, network, start: false });
      onClose();
      nav(`/vm/${node}/${r.uuid}`);
    });
  return (
    <Modal title="OVA imported" onClose={onClose}>
      <p>
        Disk{(done.volumes?.length ?? 0) > 1 ? "s" : ""} converted: <span className="mono">{done.volumes?.join(", ")}</span>. The descriptor suggests the VM below ({p.firmware.toUpperCase()}
        {p.secureBoot ? " + Secure Boot" : ""}, {p.osType}). Adjust it and create it; the imported volume is used as the disk, not copied.
      </p>
      {done.warnings?.map((w) => (
        <div key={w} className="note" style={{ marginBottom: 8 }}>
          {w}
        </div>
      ))}
      <div className="grid2">
        <Field label="VM name">
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Network">
          <select value={network} onChange={(e) => setNetwork(e.target.value)}>
            <option value="">None</option>
            {networks.map((n) => (
              <option key={n.name} value={n.name}>
                {n.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="vCPUs">
          <input type="number" min={1} value={vcpus} onChange={(e) => setVcpus(Number(e.target.value))} />
        </Field>
        <Field label="Memory (MiB)">
          <input type="number" min={64} step={64} value={mem} onChange={(e) => setMem(Number(e.target.value))} />
        </Field>
      </div>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Not now</button>
        <button className="primary" disabled={act.busy || !name} onClick={create}>
          Create VM
        </button>
      </footer>
    </Modal>
  );
}
