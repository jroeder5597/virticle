import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { bytes, get, Network, NodeStatus, Pool, post, VM, Volume } from "../api";
import { useLive } from "../live";
import { ErrorBox, Field, Modal, NodePicker, StateBadge, useAction } from "../ui";

export default function VMs() {
  const vms = useLive(() => get<VM[]>("/vms"), "vms");
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const [creating, setCreating] = useState(false);
  const act = useAction();
  const online = nodes.data?.nodes.filter((n) => n.state === "online") ?? [];
  const list = [...(vms.data ?? [])].sort((a, b) => a.name.localeCompare(b.name) || a.node - b.node);
  const action = (vm: VM, a: string) => act.run(() => post(`/nodes/${vm.node}/vms/${vm.uuid}/action`, { action: a }));

  return (
    <>
      <div className="head">
        <h1>Virtual machines</h1>
        <button className="primary" disabled={online.length === 0} onClick={() => setCreating(true)}>
          New VM
        </button>
      </div>
      <ErrorBox error={vms.error ?? act.error} />
      {nodes.data && nodes.data.nodes.length === 0 ? (
        <div className="empty">
          No nodes yet. <Link to="/nodes">Add a node</Link> to see its virtual machines.
        </div>
      ) : vms.data && list.length === 0 ? (
        <div className="empty">No virtual machines on the connected nodes.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>State</th>
              <th>Node</th>
              <th className="num">vCPUs</th>
              <th className="num">Memory</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.map((vm) => (
              <tr key={vm.node + vm.uuid}>
                <td>
                  <Link to={`/vm/${vm.node}/${vm.uuid}`}>{vm.name}</Link>
                </td>
                <td>
                  <StateBadge state={vm.state} />
                </td>
                <td>{vm.nodeName}</td>
                <td className="num">{vm.vcpus}</td>
                <td className="num">{bytes(vm.memoryKiB * 1024)}</td>
                <td className="actions">
                  {vm.state === "shutoff" ? (
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

function CreateVM({ nodes, onClose }: { nodes: NodeStatus[]; onClose: () => void }) {
  const nav = useNavigate();
  const act = useAction();
  const [node, setNode] = useState(nodes[0].id);
  const [name, setName] = useState("");
  const [vcpus, setVcpus] = useState(2);
  const [memoryMiB, setMemory] = useState(2048);
  const [firmware, setFirmware] = useState("bios");
  const [source, setSource] = useState<"iso" | "image" | "empty">("iso");
  const [pool, setPool] = useState("");
  const [diskGiB, setDisk] = useState(20);
  const [media, setMedia] = useState("");
  const [network, setNetwork] = useState("");
  const [start, setStart] = useState(true);
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
        firmware,
        pool,
        diskGiB,
        network,
        start,
        iso: source === "iso" ? media : "",
        baseVolume: source === "image" ? media : "",
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
        <Field label="Install from">
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
          <Field label="Disk size (GiB)">
            <input type="number" min={1} value={diskGiB} onChange={(e) => setDisk(Number(e.target.value))} />
          </Field>
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
          <Field label="Firmware">
            <select value={firmware} onChange={(e) => setFirmware(e.target.value)}>
              <option value="bios">BIOS</option>
              <option value="uefi">UEFI</option>
            </select>
          </Field>
        </div>
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
