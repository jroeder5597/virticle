import { Play, Power, SquareTerminal } from "lucide-react";
import { FormEvent, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { bytes, get, Network, NodeStatus, Pool, post, VM, Volume } from "../api";
import { useLive } from "../live";
import CloudInitFields, { CloudInitValues, cloudInitPayload, cloudInitUsed, emptyCloudInit } from "./CloudInit";
import PoolSelect from "../PoolSelect";
import Select from "../Select";
import { ErrorBox, Field, Modal, NodePicker, PageTitle, StateBadge, useAction, useConfirm } from "../ui";
import { powerConfirm, powerLabel } from "../power";
import { BulkBar } from "./Bulk";
import { emptyFw, FirmwareFields, fwPayload, FwValues, limitToCaps, OsTypeField, useFwCaps } from "./Firmware";
import DialogAdvanced from "./DialogAdvanced";
import { TagChips } from "./Tags";
import { Meter, pct, useAllStats, VMPoint } from "../charts";
import { baselineLabel, useCpuBaseline } from "../cpu";

const DEFAULT_DISK_GIB = 20;

// suggestedDiskGiB is the disk size offered for a new VM: the default, or the
// whole GiB that are free in the pool when that is less (at least 1).
export function suggestedDiskGiB(availableBytes: number): number {
  const free = Math.floor(availableBytes / 2 ** 30);
  return Math.max(1, Math.min(DEFAULT_DISK_GIB, free));
}

/** `fixedNode` limits the list to one node (node page tab): the node column goes and New VM preselects it. */
export default function VMs({ fixedNode }: { fixedNode?: number } = {}) {
  const vms = useLive(() => get<VM[]>("/vms"), "vms");
  const live = useAllStats();
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const [creating, setCreating] = useState(false);
  const [params, setParams] = useSearchParams();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState("");
  const act = useAction();
  const preferred = Number(params.get("new")) || 0; // set by the sidebar tree's "Add VM"
  // &pool=&image= (the image catalogue's "Create VM from this image") preselect a base image.
  const [preset, setPreset] = useState<{ pool: string; image: string } | undefined>();
  const online = (nodes.data?.nodes.filter((n) => n.state === "online" && (fixedNode === undefined || n.id === fixedNode)) ?? []).sort((a, b) => Number(b.id === preferred) - Number(a.id === preferred));
  useEffect(() => {
    if (preferred && nodes.data) {
      setPreset(params.get("image") ? { pool: params.get("pool") ?? "", image: params.get("image") ?? "" } : undefined);
      setCreating(online.length > 0);
      setParams({}, { replace: true });
    }
  }, [preferred, nodes.data]); // eslint-disable-line react-hooks/exhaustive-deps
  const all = [...(vms.data ?? [])].filter((v) => fixedNode === undefined || v.node === fixedNode).sort((a, b) => a.name.localeCompare(b.name) || a.node - b.node);
  const allTags = [...new Set(all.flatMap((v) => v.tags ?? []))].sort();
  const needle = search.trim().toLowerCase();
  const list = all.filter(
    (v) =>
      (!tagFilter || (v.tags ?? []).includes(tagFilter)) &&
      (!needle || [v.name, v.nodeName, v.state, ...(v.tags ?? [])].some((s) => s.toLowerCase().includes(needle))),
  );
  const keyOf = (v: VM) => v.node + "/" + v.uuid;
  const picked = all.filter((v) => selected.has(keyOf(v)));
  const toggle = (v: VM) =>
    setSelected((s) => {
      const n = new Set(s);
      n.has(keyOf(v)) ? n.delete(keyOf(v)) : n.add(keyOf(v));
      return n;
    });
  const allShown = list.length > 0 && list.every((v) => selected.has(keyOf(v)));
  const confirm = useConfirm();
  const action = async (vm: VM, a: string) => {
    const spec = powerConfirm(a, [vm.name]);
    if (spec && !(await confirm.ask(spec))) return;
    return act.run(() => post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: a }));
  };

  return (
    <>
      <div className="head">
        {fixedNode === undefined ? <PageTitle>Virtual machines</PageTitle> : <h2>Virtual machines</h2>}
        <button className="primary" disabled={online.length === 0} onClick={() => setCreating(true)}>
          New VM
        </button>
      </div>
      <ErrorBox error={vms.error ?? act.error} />
      {all.length > 0 && (
        <div className="filters">
          <input type="search" placeholder="Search name, node, state or tag" aria-label="Search VMs" value={search} onChange={(e) => setSearch(e.target.value)} />
          {allTags.length > 0 && (
            <Select auto label="Filter by tag" value={tagFilter} onChange={setTagFilter} options={[{ value: "", label: "All tags" }, ...allTags.map((t) => ({ value: t, label: t }))]} />
          )}
          {(search || tagFilter) && <span className="muted">{list.length} of {all.length} shown</span>}
        </div>
      )}
      {selected.size > 0 && <BulkBar selected={picked} onClear={() => setSelected(new Set())} />}
      {fixedNode === undefined && nodes.data && nodes.data.nodes.length === 0 ? (
        <div className="empty">
          No nodes yet. <Link to="/nodes">Add a node</Link> to see its virtual machines.
        </div>
      ) : vms.data && list.length === 0 ? (
        <div className="empty">{all.length ? "No VM matches the search or tag filter." : "No virtual machines on the connected nodes."}</div>
      ) : (
        <table className="vmlist">
          <thead>
            <tr>
              <th className="sel">
                <input
                  type="checkbox"
                  aria-label="Select all shown VMs"
                  checked={allShown}
                  onChange={() => setSelected((s) => { const n = new Set(s); list.forEach((v) => (allShown ? n.delete(keyOf(v)) : n.add(keyOf(v)))); return n; })}
                />
              </th>
              <th>Name</th>
              <th>State</th>
              {fixedNode === undefined && <th>Node</th>}
              <th className="num">vCPUs</th>
              <th className="num">Memory</th>
              <th>CPU</th>
              <th>Memory used</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((vm) => (
              <tr key={vm.node + vm.uuid} className={selected.has(keyOf(vm)) ? "selected" : ""}>
                <td className="sel">
                  <input type="checkbox" aria-label={`Select ${vm.name}`} checked={selected.has(keyOf(vm))} onChange={() => toggle(vm)} />
                </td>
                <td className="vmname">
                  <Link to={`/vm/${vm.node}/${vm.uuid}`}>{vm.name}</Link>
                  {vm.template && <span className="badge s-template">Template</span>}
                  <TagChips tags={vm.tags} onPick={setTagFilter} />
                </td>
                <td>
                  <StateBadge state={vm.state} />
                </td>
                {fixedNode === undefined && <td className="vmnode">{vm.nodeName}</td>}
                <td className="num">{vm.vcpus}</td>
                <td className="num">{bytes(vm.memoryKiB * 1024)}</td>
                <UsageCells p={vm.state === "running" ? live?.vms[`${vm.node}/${vm.uuid}`] : undefined} />
                <td className="actions">
                  {vm.state === "shutoff" && vm.template ? null : vm.state === "shutoff" ? (
                    <button className="iconbtn" disabled={act.busy} onClick={() => action(vm, "start")} aria-label={powerLabel.start} title={powerLabel.start}>
                      <Play size={15} aria-hidden="true" />
                    </button>
                  ) : (
                    <>
                      {vm.state === "paused" && (
                        <button className="iconbtn" disabled={act.busy} onClick={() => action(vm, "resume")} aria-label="Resume" title="Resume">
                          <Play size={15} aria-hidden="true" />
                        </button>
                      )}
                      <button className="iconbtn" disabled={act.busy} onClick={() => action(vm, "shutdown")} aria-label={powerLabel.shutdown} title={powerLabel.shutdown}>
                        <Power size={15} aria-hidden="true" />
                      </button>
                      <Link className="button iconbtn" to={`/vm/${vm.node}/${vm.uuid}?tab=console`} aria-label="Console" title="Console">
                        <SquareTerminal size={15} aria-hidden="true" />
                      </Link>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {confirm.dialog}
      {creating && <CreateVM nodes={online} preset={preset} onClose={() => setCreating(false)} />}
    </>
  );
}

/** Compact CPU and memory columns from the newest stats sample. */
function UsageCells({ p }: { p?: VMPoint }) {
  if (!p) {
    return (
      <>
        <td className="muted">–</td>
        <td className="muted">–</td>
      </>
    );
  }
  const used = p.memUsedKiB ?? 0;
  const total = p.memTotalKiB ?? 0;
  return (
    <>
      <td>
        <Meter value={p.cpu} max={100} label={pct(p.cpu)} />
      </td>
      <td>
        <Meter value={used} max={total} label={total ? `${bytes(used * 1024)}${p.memGuest ? "" : " alloc."}` : "–"} />
      </td>
    </>
  );
}

const CPU_HINT: Record<string, string> = {
  baseline: "The best CPU every online node can run. Live migration works between them.",
  "host-model": "The closest named model of this node. Migration needs a target that can run it.",
  "host-passthrough": "The exact host CPU. Migration needs an identical CPU on the target.",
};

function CreateVM({ nodes, preset, onClose }: { nodes: NodeStatus[]; preset?: { pool: string; image: string }; onClose: () => void }) {
  const nav = useNavigate();
  const act = useAction();
  const [node, setNode] = useState(nodes[0].id);
  const [name, setName] = useState("");
  const [vcpus, setVcpus] = useState(2);
  const [memoryMiB, setMemory] = useState(2048);
  const [fw, setFw] = useState<FwValues>(emptyFw);
  const { caps, error: capsError } = useFwCaps(node);
  const [cpu, setCpu] = useState("baseline");
  const baseline = useCpuBaseline();
  const [source, setSource] = useState<"iso" | "image" | "empty" | "existing">(preset ? "image" : "iso");
  const [pool, setPool] = useState(preset?.pool ?? "");
  const sourceTouched = useRef(!!preset); // true once the user picked where to install from
  const presetImage = useRef(preset?.image); // used once, for the first load of the node's volumes
  const [diskGiB, setDisk] = useState(DEFAULT_DISK_GIB);
  const diskEdited = useRef(false); // once the user types a size, it is left alone
  const [media, setMedia] = useState("");
  const [network, setNetwork] = useState("");
  const [start, setStart] = useState(true);
  const [ci, setCi] = useState<CloudInitValues>(emptyCloudInit);
  const [pools, setPools] = useState<Pool[]>([]);
  const [networks, setNetworks] = useState<Network[]>([]);
  const [volumes, setVolumes] = useState<(Volume & { pool: string })[]>([]);

  // A default the node cannot honour (UEFI, say) is dropped as soon as the node's abilities are known.
  useEffect(() => {
    if (caps) setFw((v) => limitToCaps(v, caps));
  }, [caps]);

  // The suggested disk size fits the chosen pool: 20 GiB, or what is free
  // when the pool has less.
  useEffect(() => {
    if (diskEdited.current) return;
    const p = pools.find((x) => x.name === pool);
    if (p) setDisk(suggestedDiskGiB(p.available));
  }, [pool, pools]);

  // Pools, networks and images belong to the chosen node.
  useEffect(() => {
    let live = true;
    setMedia(presetImage.current ?? "");
    presetImage.current = undefined;
    (async () => {
      const [p, n] = await Promise.all([get<Pool[]>(`/nodes/${node}/pools`), get<Network[]>(`/nodes/${node}/networks`)]);
      if (!live) return;
      const running = p.filter((x) => x.state === "running");
      setPools(running);
      setPool((cur) => (running.some((x) => x.name === cur) ? cur : running[0]?.name ?? ""));
      setNetworks(n.filter((x) => x.active));
      setNetwork((cur) => (n.some((x) => x.name === cur) ? cur : n.find((x) => x.active)?.name ?? ""));
      const all = await Promise.all(
        running.map((x) => get<Volume[]>(`/nodes/${node}/pools/${encodeURIComponent(x.name)}/volumes`).then((v) => v.map((y) => ({ ...y, pool: x.name })))),
      );
      if (live) setVolumes(all.flat());
    })().catch((e) => live && act.run(() => Promise.reject(e)));
    return () => {
      live = false;
    };
  }, [node]);

  const isos = volumes.filter((v) => v.format === "iso" || v.name.toLowerCase().endsWith(".iso"));
  const images = volumes.filter((v) => v.pool === pool && (v.format === "qcow2" || v.format === "raw") && !v.name.toLowerCase().endsWith(".iso"));

  // Nothing to install from but a downloaded image (the first run after the
  // image catalogue): start on "Disk image" with that image chosen, so that
  // the dialog needs only a name. Once, and never after the user chose.
  useEffect(() => {
    if (sourceTouched.current || volumes.length === 0) return;
    if (isos.length === 0 && images.length > 0) {
      sourceTouched.current = true;
      setSource("image");
      if (images.length === 1) setMedia(images[0].name);
    }
  }, [volumes, pool]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const need = source === "iso" ? "an ISO" : source === "image" ? "a base image" : source === "existing" ? "a volume" : "";
    if (need && !media) return void act.run(() => Promise.reject(new Error(`Choose ${need}.`)));
    if (!pool) return void act.run(() => Promise.reject(new Error("Choose a storage pool.")));
    let created: { uuid: string; startError?: string } | undefined;
    const ok = await act.run(async () => {
      created = await post(`/nodes/${node}/vms`, {
        name,
        vcpus,
        memoryMiB,
        ...fwPayload(fw),
        cpu,
        pool,
        diskGiB,
        network,
        start,
        iso: source === "iso" ? media : "",
        baseVolume: source === "image" ? media : "",
        existingVolume: source === "existing" ? media : "",
        cloudInit: source === "image" && cloudInitUsed(ci) ? cloudInitPayload(ci, name) : undefined,
      });
    });
    if (ok && created) {
      onClose();
      nav(`/vm/${node}/${created.uuid}${start && !created.startError ? "?tab=console" : ""}`);
    }
  };

  return (
    <Modal title="New virtual machine" onClose={onClose}>
      <form onSubmit={submit}>
        <div className="grid2">
          <Field label="Name">
            <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus pattern="[A-Za-z0-9][A-Za-z0-9._\-]*" />
          </Field>
          {nodes.length > 1 && (
            <Field label="Node">
              <NodePicker nodes={nodes} value={node} onChange={setNode} />
            </Field>
          )}
          <Field label="vCPUs">
            <input type="number" min={1} value={vcpus} onChange={(e) => setVcpus(Number(e.target.value))} />
          </Field>
          <Field label="Memory (MiB)">
            <input type="number" min={64} step={64} value={memoryMiB} onChange={(e) => setMemory(Number(e.target.value))} />
          </Field>
        </div>
        <ErrorBox error={capsError} />
        <OsTypeField value={fw} caps={caps} onChange={setFw} />
        <Field label="Install from" group>
          <div className="segmented">
            {(
              [
                ["iso", "Installer ISO"],
                ["image", "Disk image"],
                ["empty", "Empty disk"],
              ] as const
            ).map(([v, label]) => (
              <button
                type="button"
                key={v}
                className={source === v ? "on" : ""}
                onClick={() => {
                  sourceTouched.current = true;
                  setSource(v);
                  setMedia("");
                }}
              >
                {label}
              </button>
            ))}
          </div>
        </Field>
        {source === "iso" && (
          <Field label="ISO" hint={isos.length === 0 ? "No ISO images on this node. Upload one under Storage." : undefined}>
            <Select label="ISO" value={media} onChange={setMedia} placeholder="Choose an ISO" options={isos.map((v) => ({ value: v.path, label: `${v.pool}/${v.name}` }))} />
          </Field>
        )}
        {source === "image" && (
          <Field label="Base image" hint="The new disk starts as a copy of this image, in the same pool.">
            <Select label="Base image" value={media} onChange={setMedia} placeholder="Choose an image" options={images.map((v) => ({ value: v.name, label: `${v.name} (${bytes(v.capacity)})` }))} />
          </Field>
        )}
        {source === "image" && images.length === 0 && pool && (
          <p className="muted">
            No images in this pool. <Link to={`/node/${node}/storage?pool=${encodeURIComponent(pool)}&catalogue=1`}>Get a cloud image from the catalogue</Link>.
          </p>
        )}
        <div className="grid2">
          {pools.length !== 1 && (
            <Field label="Storage pool">
              <PoolSelect label="Storage pool" pools={pools} value={pool} onChange={setPool} needBytes={source === "existing" ? undefined : diskGiB * 2 ** 30} />
            </Field>
          )}
          {source !== "existing" && (
            <Field label="Disk size (GiB)">
              <input
                type="number"
                min={1}
                value={diskGiB}
                onChange={(e) => {
                  diskEdited.current = true;
                  setDisk(Number(e.target.value));
                }}
              />
            </Field>
          )}
          <Field label="Network">
            <Select label="Network" value={network} onChange={setNetwork} options={[{ value: "", label: "None" }, ...networks.map((n) => ({ value: n.name, label: `${n.name} (${n.forward})` }))]} />
          </Field>
        </div>
        {source === "image" && <CloudInitFields value={ci} onChange={setCi} preselect />}
        <label className="check">
          <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Start after creating
        </label>
        <DialogAdvanced>
          <Field label="CPU type" hint={CPU_HINT[cpu]}>
            <Select
              label="CPU type"
              value={cpu}
              onChange={setCpu}
              options={[
                { value: "baseline", label: baselineLabel(baseline) },
                { value: "host-model", label: "Host model (this node)" },
                { value: "host-passthrough", label: "Host passthrough (no live migration to different CPUs)" },
              ]}
            />
          </Field>
          <FirmwareFields value={fw} caps={caps} onChange={setFw} />
          <Field label="Existing volume" hint="Use a volume of the chosen pool as the disk as it is, not copied.">
            <Select
              label="Existing volume"
              value={source === "existing" ? media : ""}
              onChange={(v) => {
                setSource(v ? "existing" : "iso");
                setMedia(v);
              }}
              options={[{ value: "", label: "None" }, ...images.map((v) => ({ value: v.name, label: `${v.name} (${bytes(v.capacity)})` }))]}
            />
          </Field>
        </DialogAdvanced>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            {act.busy ? "Creating" : "Create VM"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}
