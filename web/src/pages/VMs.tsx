import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { bytes, get, Network, NodeStatus, Pool, post, VM, Volume } from "../api";
import { useLive } from "../live";
import CloudInitFields, { CloudInitValues, cloudInitPayload, cloudInitUsed, emptyCloudInit } from "./CloudInit";
import { ErrorBox, Field, Modal, NodePicker, StateBadge, useAction } from "../ui";
import { BulkBar } from "./Bulk";
import { emptyFw, FirmwareFields, fwPayload, FwValues } from "./Firmware";
import { TagChips } from "./Tags";
import { Meter, pct, useAllStats, VMPoint } from "../charts";

/** `fixedNode` limits the list to one node (node page tab): the node column goes and New VM preselects it. */
export default function VMs({ fixedNode }: { fixedNode?: number } = {}) {
  const vms = useLive(() => get<VM[]>("/vms"), "vms");
  const live = useAllStats();
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [search, setSearch] = useState("");
  const [tagFilter, setTagFilter] = useState("");
  const act = useAction();
  const online = nodes.data?.nodes.filter((n) => n.state === "online" && (fixedNode === undefined || n.id === fixedNode)) ?? [];
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
  const action = (vm: VM, a: string) => act.run(() => post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: a }));

  return (
    <>
      <div className="head">
        {fixedNode === undefined ? <h1>Virtual machines</h1> : <h2>Virtual machines</h2>}
        <button className="primary" disabled={online.length === 0} onClick={() => setCreating(true)}>
          New VM
        </button>
      </div>
      <ErrorBox error={vms.error ?? act.error} />
      {all.length > 0 && (
        <div className="filters">
          <input type="search" placeholder="Search name, node, state or tag…" aria-label="Search VMs" value={search} onChange={(e) => setSearch(e.target.value)} />
          {allTags.length > 0 && (
            <select aria-label="Filter by tag" value={tagFilter} onChange={(e) => setTagFilter(e.target.value)}>
              <option value="">All tags</option>
              {allTags.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </select>
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
        <table>
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
                <td>
                  <Link to={`/vm/${vm.node}/${vm.uuid}`}>{vm.name}</Link>
                  {vm.template && <span className="badge s-template">Template</span>}
                  <TagChips tags={vm.tags} onPick={setTagFilter} />
                </td>
                <td>
                  <StateBadge state={vm.state} />
                </td>
                {fixedNode === undefined && <td>{vm.nodeName}</td>}
                <td className="num">{vm.vcpus}</td>
                <td className="num">{bytes(vm.memoryKiB * 1024)}</td>
                <UsageCells p={vm.state === "running" ? live?.vms[`${vm.node}/${vm.uuid}`] : undefined} />
                <td className="actions">
                  {vm.state === "shutoff" && vm.template ? null : vm.state === "shutoff" ? (
                    <button disabled={act.busy} onClick={() => action(vm, "start")}>
                      Start
                    </button>
                  ) : (
                    <>
                      {vm.state === "paused" && (
                        <button disabled={act.busy} onClick={() => action(vm, "resume")}>
                          Resume
                        </button>
                      )}
                      <button disabled={act.busy} onClick={() => action(vm, "shutdown")}>
                        Shut down
                      </button>
                      <Link className="button" to={`/vm/${vm.node}/${vm.uuid}?tab=console`}>
                        Console
                      </Link>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {creating && <CreateVM nodes={online} onClose={() => setCreating(false)} />}
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

function CreateVM({ nodes, onClose }: { nodes: NodeStatus[]; onClose: () => void }) {
  const nav = useNavigate();
  const act = useAction();
  const [node, setNode] = useState(nodes[0].id);
  const [name, setName] = useState("");
  const [vcpus, setVcpus] = useState(2);
  const [memoryMiB, setMemory] = useState(2048);
  const [fw, setFw] = useState<FwValues>(emptyFw);
  const [source, setSource] = useState<"iso" | "image" | "empty" | "existing">("iso");
  const [pool, setPool] = useState("");
  const [diskGiB, setDisk] = useState(20);
  const [media, setMedia] = useState("");
  const [network, setNetwork] = useState("");
  const [start, setStart] = useState(true);
  const [ci, setCi] = useState<CloudInitValues>(emptyCloudInit);
  const [pools, setPools] = useState<Pool[]>([]);
  const [networks, setNetworks] = useState<Network[]>([]);
  const [volumes, setVolumes] = useState<(Volume & { pool: string })[]>([]);

  // Pools, networks and images belong to the chosen node.
  useEffect(() => {
    let live = true;
    setMedia("");
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

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let created: { uuid: string; startError?: string } | undefined;
    const ok = await act.run(async () => {
      created = await post(`/nodes/${node}/vms`, {
        name,
        vcpus,
        memoryMiB,
        ...fwPayload(fw),
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
          <Field label="Node">
            <NodePicker nodes={nodes} value={node} onChange={setNode} />
          </Field>
          <Field label="vCPUs">
            <input type="number" min={1} value={vcpus} onChange={(e) => setVcpus(Number(e.target.value))} />
          </Field>
          <Field label="Memory (MiB)">
            <input type="number" min={64} step={64} value={memoryMiB} onChange={(e) => setMemory(Number(e.target.value))} />
          </Field>
        </div>
        <Field label="Install from" group>
          <div className="segmented">
            {(
              [
                ["iso", "Installer ISO"],
                ["image", "Disk image"],
                ["empty", "Empty disk"],
                ["existing", "Existing volume"],
              ] as const
            ).map(([v, label]) => (
              <button
                type="button"
                key={v}
                className={source === v ? "on" : ""}
                onClick={() => {
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
            <select value={media} onChange={(e) => setMedia(e.target.value)} required>
              <option value="">Choose an ISO…</option>
              {isos.map((v) => (
                <option key={v.path} value={v.path}>
                  {v.pool}/{v.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        {source === "image" && (
          <Field label="Base image" hint="The new disk starts as a copy of this image, in the same pool.">
            <select value={media} onChange={(e) => setMedia(e.target.value)} required>
              <option value="">Choose an image…</option>
              {images.map((v) => (
                <option key={v.path} value={v.name}>
                  {v.name} ({bytes(v.capacity)})
                </option>
              ))}
            </select>
          </Field>
        )}
        {source === "existing" && (
          <Field label="Volume" hint="Used as the VM's disk as it is, not copied (an imported disk, say). Import other formats under Storage first.">
            <select value={media} onChange={(e) => setMedia(e.target.value)} required>
              <option value="">Choose a volume…</option>
              {images.map((v) => (
                <option key={v.path} value={v.name}>
                  {v.name} ({bytes(v.capacity)})
                </option>
              ))}
            </select>
          </Field>
        )}
        <div className="grid2">
          <Field label="Storage pool">
            <select value={pool} onChange={(e) => setPool(e.target.value)} required>
              {pools.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.name} ({bytes(p.available)} free)
                </option>
              ))}
            </select>
          </Field>
          {source !== "existing" && (
            <Field label="Disk size (GiB)">
              <input type="number" min={1} value={diskGiB} onChange={(e) => setDisk(Number(e.target.value))} />
            </Field>
          )}
          <Field label="Network">
            <select value={network} onChange={(e) => setNetwork(e.target.value)}>
              <option value="">None</option>
              {networks.map((n) => (
                <option key={n.name} value={n.name}>
                  {n.name} ({n.forward})
                </option>
              ))}
            </select>
          </Field>
        </div>
        <FirmwareFields node={node} value={fw} onChange={setFw} />
        {source === "image" && <CloudInitFields vmName={name} value={ci} onChange={setCi} />}
        <label className="check">
          <input type="checkbox" checked={start} onChange={(e) => setStart(e.target.checked)} /> Start after creating
        </label>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            {act.busy ? "Creating…" : "Create"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}
