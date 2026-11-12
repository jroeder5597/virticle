import { FormEvent, useEffect, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import Play from "lucide-react/icons/play";
import Square from "lucide-react/icons/square";
import Trash2 from "lucide-react/icons/trash-2";
import { bytes, del, get, NodeStatus, Pool, post, uploadVolume, Volume } from "../api";
import { orEvents, useLive } from "../live";
import { useLiveDebounced } from "../liveDebounced";
import DownloadVolume from "./DownloadVolume";
import ImageCatalogue from "./ImageCatalogue";
import { AlertMarks, useAlerts } from "./Alerts";
import ImportDialog from "./Import";
import { NodeErr, NodeErrors, NodeFilter, nodeEvents, nodeLabel, NodePool, poolEvents } from "./dcShared";
import Select from "../Select";
import { ErrorBox, Field, Modal, NodePicker, PageTitle, StateBadge, UsageBar, useAction, useConfirm, usePrompt } from "../ui";

/**
 * `fixedNode` pins the page to one node (node page tab): no node picker, no node auto-selection.
 * `allNodes` lists the pools of every node in one table (Datacenter tab), one request for all of them.
 */
export default function Storage({ fixedNode, allNodes }: { fixedNode?: number; allNodes?: boolean } = {}) {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes", nodeEvents);
  const [pickedNode, setNode] = useState(0);
  const node = fixedNode ?? pickedNode;
  // ?pool=NAME&catalogue=1 (a link from the New VM dialog) selects a pool and opens the image catalogue.
  const [qs, setQs] = useSearchParams();
  const [selected, setSelected] = useState<{ node: number; name: string } | undefined>(qs.get("pool") ? { node: 0, name: qs.get("pool")! } : undefined);
  const [openCatalogue] = useState(qs.get("catalogue") === "1");
  useEffect(() => {
    if (qs.has("catalogue") || qs.has("pool")) setQs({}, { replace: true });
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  const [creating, setCreating] = useState(false);
  const [filterNode, setFilterNode] = useState(0); // all-nodes mode: 0 is every node
  const [text, setText] = useState("");
  const all = nodes.data?.nodes ?? [];
  useEffect(() => {
    if (!allNodes && fixedNode === undefined && !all.some((n) => n.id === node && n.state === "online")) setNode(all.find((n) => n.state === "online")?.id ?? 0);
  }, [all, node]); // eslint-disable-line react-hooks/exhaustive-deps
  const single = useLive(() => (!allNodes && node ? get<Pool[]>(`/nodes/${node}/pools`) : Promise.resolve([])), `pools/${allNodes ? "-" : node}`, orEvents("pool"));
  const every = useLiveDebounced(
    () => (allNodes ? get<{ pools: NodePool[]; errors: NodeErr[] }>("/pools") : Promise.resolve({ pools: [] as NodePool[], errors: [] as NodeErr[] })),
    `pools/all/${!!allNodes}`,
    poolEvents,
  );
  const loaded = allNodes ? every.data : single.data;
  const reload = allNodes ? every.reload : single.reload;
  const alerts = useAlerts();
  const act = useAction();
  const confirm = useConfirm();
  const nodeName = (id: number) => nodeLabel(all.find((n) => n.id === id) ?? { addr: String(id) });
  const rows: NodePool[] = allNodes ? every.data?.pools ?? [] : (single.data ?? []).map((p) => ({ ...p, node, nodeName: nodeName(node) }));
  const needle = text.trim().toLowerCase();
  const shown = rows.filter((p) => (!filterNode || p.node === filterNode) && (!needle || [p.name, p.nodeName, p.type, p.state, p.target ?? ""].some((s) => s.toLowerCase().includes(needle))));
  const pool = shown.find((p) => p.name === selected?.name && (!selected.node || selected.node === p.node)) ?? shown[0];
  const poolAction = async (p: NodePool, action: string) => {
    if (action === "stop" || action === "remove") {
      const where = allNodes ? ` on ${p.nodeName}` : "";
      const ok = await confirm.ask(
        action === "stop"
          ? { title: `Stop pool ${p.name}`, message: `The storage pool ${p.name}${where} is stopped. VMs whose disks are in it cannot start, and running ones may fail on their next disk access, until it is started again.`, confirm: "Stop pool" }
          : { title: `Remove pool ${p.name}`, message: `The storage pool ${p.name} is removed from ${allNodes ? p.nodeName : "this node"}. The data in it is not deleted.`, confirm: "Remove pool" },
      );
      if (!ok) return;
    }
    if (await act.run(() => post(`/nodes/${p.node}/pools/${encodeURIComponent(p.name)}/action`, { action }))) reload();
  };

  if (fixedNode === undefined && nodes.data && all.length === 0) {
    return (
      <>
        <PageTitle>Storage</PageTitle>
        <div className="empty">
          No nodes yet. <Link to={allNodes ? "/datacenter/nodes" : "/nodes"}>Add a node</Link> to manage its storage.
        </div>
      </>
    );
  }
  const online = all.filter((n) => n.state === "online");
  const createNode = allNodes ? (online.some((n) => n.id === filterNode) ? filterNode : online[0]?.id ?? 0) : node;
  return (
    <>
      <div className="head">
        {fixedNode === undefined ? <PageTitle>Storage</PageTitle> : <h2>Storage pools</h2>}
        <div className="actions">
          {fixedNode === undefined && !allNodes && <NodePicker auto nodes={all} value={node} onChange={setNode} />}
          <button className="primary" disabled={!createNode} onClick={() => setCreating(true)}>
            New pool
          </button>
        </div>
      </div>
      <ErrorBox error={(allNodes ? every.error : single.error) ?? act.error} />
      <NodeErrors errors={every.data?.errors} />
      {allNodes && <NodeFilter nodes={all} value={filterNode} onChange={setFilterNode} text={text} onText={setText} placeholder="Search pools by name, type or state" shown={shown.length} total={rows.length} />}
      {allNodes && loaded && shown.length === 0 ? (
        <div className="empty">{rows.length ? "No pool matches the filter." : "No storage pools on the connected nodes."}</div>
      ) : (
        <table className={allNodes ? "dctable" : ""}>
          <thead>
            <tr>
              <th>Pool</th>
              {allNodes && <th>Node</th>}
              <th>Type</th>
              <th>State</th>
              {!allNodes && <th>Location</th>}
              <th>Usage</th>
              {allNodes && <th>Autostart</th>}
              <th />
            </tr>
          </thead>
          <tbody>
            {shown.map((p) => (
              <tr key={p.node + "/" + p.name} className={pool?.node === p.node && pool?.name === p.name ? "selected" : ""} onClick={() => setSelected({ node: allNodes ? p.node : 0, name: p.name })}>
                <td title={p.target}>
                  {p.name}
                  <AlertMarks alerts={alerts} match={(a) => a.id === `pool:${p.node}:${p.name}`} />
                </td>
                {allNodes && <td>{p.nodeName}</td>}
                <td>{p.type}</td>
                <td>
                  <StateBadge state={p.state} />
                </td>
                {!allNodes && <td className="mono">{p.target}</td>}
                <td>
                  {p.state === "running" && (
                    <>
                      <UsageBar used={p.allocation} total={p.capacity} />
                      <small className="muted">
                        {bytes(p.available)} free of {bytes(p.capacity)}
                      </small>
                    </>
                  )}
                </td>
                {allNodes && <td>{p.autostart ? "Yes" : "No"}</td>}
                <td className="actions" onClick={(e) => e.stopPropagation()}>
                  {allNodes ? (
                    <>
                      {p.state === "running" ? (
                        <button className="iconbtn" disabled={act.busy} onClick={() => poolAction(p, "stop")} aria-label="Stop" title="Stop">
                          <Square size={15} aria-hidden="true" />
                        </button>
                      ) : (
                        <button className="iconbtn" disabled={act.busy} onClick={() => poolAction(p, "start")} aria-label="Start" title="Start">
                          <Play size={15} aria-hidden="true" />
                        </button>
                      )}
                      <button className="iconbtn danger" disabled={act.busy} onClick={() => poolAction(p, "remove")} aria-label="Remove" title="Remove">
                        <Trash2 size={15} aria-hidden="true" />
                      </button>
                    </>
                  ) : (
                    <>
                      {p.state === "running" ? (
                        <button disabled={act.busy} onClick={() => poolAction(p, "stop")}>
                          Stop
                        </button>
                      ) : (
                        <button disabled={act.busy} onClick={() => poolAction(p, "start")}>
                          Start
                        </button>
                      )}
                      <button className="danger" disabled={act.busy} onClick={() => poolAction(p, "remove")}>
                        Remove
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {pool && pool.state === "running" && <Volumes key={pool.node + "/" + pool.name} node={pool.node} nodeName={pool.nodeName} showNode={allNodes} pool={pool} openCatalogue={openCatalogue} />}
      {confirm.dialog}
      {creating && <CreatePool node={createNode} nodes={allNodes ? online : undefined} onClose={() => setCreating(false)} onDone={reload} />}
    </>
  );
}

function Volumes({ node, nodeName, showNode, pool, openCatalogue }: { node: number; nodeName?: string; showNode?: boolean; pool: Pool; openCatalogue?: boolean }) {
  const base = `/nodes/${node}/pools/${encodeURIComponent(pool.name)}/volumes`;
  const vols = useLive(() => get<Volume[]>(base), base, poolEvents);
  const act = useAction();
  const confirm = useConfirm();
  const prompt = usePrompt();
  const [creating, setCreating] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [catalogue, setCatalogue] = useState(!!openCatalogue);
  const [importing, setImporting] = useState<string | undefined>();
  const [upload, setUpload] = useState<{ name: string; progress: number } | undefined>();
  const file = useRef<HTMLInputElement>(null);

  const onFile = async (f?: File) => {
    if (!f) return;
    setUpload({ name: f.name, progress: 0 });
    await act.run(() => uploadVolume(node, pool.name, f, (p) => setUpload({ name: f.name, progress: p })));
    setUpload(undefined);
    if (file.current) file.current.value = "";
    vols.reload();
  };
  const volAction = (v: Volume, body: object) => act.run(() => post(`${base}/${encodeURIComponent(v.name)}/action`, body));
  const resize = async (v: Volume) => {
    const answer = await prompt.ask({
      title: `Resize ${v.name}`,
      label: "New size (GiB)",
      hint: `Currently ${(v.capacity / 2 ** 30).toFixed(1)} GiB. Volumes can only grow.`,
      type: "number",
      confirm: "Resize",
    });
    const gib = Number(answer);
    if (answer && gib > 0) volAction(v, { action: "resize", capacity: Math.round(gib * 2 ** 30) });
  };
  const clone = async (v: Volume) => {
    const name = await prompt.ask({ title: `Clone ${v.name}`, label: "Name of the copy", initial: v.name.replace(/(\.[^.]+)?$/, "-copy$1"), confirm: "Clone" });
    if (name) volAction(v, { action: "clone", name });
  };
  const remove = async (v: Volume) => {
    const ok = await confirm.ask({ title: `Delete volume ${v.name}`, message: `The volume ${v.name} in pool ${pool.name} is deleted. Its data is destroyed and cannot be recovered.`, confirm: "Delete volume" });
    if (ok) act.run(() => del(`${base}/${encodeURIComponent(v.name)}`));
  };

  return (
    <>
      <div className="head sub">
        <h2>Volumes in {pool.name}{showNode && nodeName ? ` on ${nodeName}` : ""}</h2>
        <div className="actions">
          <input ref={file} type="file" hidden onChange={(e) => onFile(e.target.files?.[0])} />
          <button disabled={!!upload} onClick={() => file.current?.click()}>
            Upload image or ISO
          </button>
          <button onClick={() => setDownloading(true)}>Download from URL</button>
          <button onClick={() => setCatalogue(true)}>Image catalogue</button>
          <button onClick={() => setImporting("")}>Import disk or OVA</button>
          <button onClick={() => setCreating(true)}>New volume</button>
        </div>
      </div>
      {upload && (
        <div className="progress">
          Uploading {upload.name} {(upload.progress * 100).toFixed(0)}%
          <div className="bar">
            <div style={{ width: upload.progress * 100 + "%" }} />
          </div>
        </div>
      )}
      <ErrorBox error={vols.error ?? act.error} />
      {vols.data && vols.data.length === 0 ? (
        <div className="empty">This pool is empty.</div>
      ) : (
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Format</th>
              <th className="num">Size</th>
              <th className="num">Used on disk</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {(vols.data ?? []).map((v) => (
              <tr key={v.name}>
                <td title={v.path}>{v.name}</td>
                <td>{v.format}</td>
                <td className="num">{bytes(v.capacity)}</td>
                <td className="num">{bytes(v.allocation)}</td>
                <td className="actions">
                  <button disabled={act.busy} onClick={() => setImporting(v.name)} title="Convert this image to a qcow2 volume">
                    Import
                  </button>
                  <button disabled={act.busy} onClick={() => clone(v)}>
                    Clone
                  </button>
                  <button disabled={act.busy} onClick={() => resize(v)}>
                    Resize
                  </button>
                  <button
                    className="danger"
                    disabled={act.busy}
                    onClick={() => remove(v)}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {confirm.dialog}
      {prompt.dialog}
      {downloading && <DownloadVolume node={node} pool={pool.name} onClose={() => setDownloading(false)} onDone={vols.reload} />}
      {catalogue && <ImageCatalogue node={node} nodeName={nodeName} pool={pool.name} onClose={() => setCatalogue(false)} onDone={vols.reload} />}
      {creating && <CreateVolume base={base} pool={pool} onClose={() => setCreating(false)} />}
      {importing !== undefined && <ImportDialog node={node} pool={pool.name} volumes={vols.data ?? []} initial={importing} onClose={() => setImporting(undefined)} onDone={vols.reload} />}
    </>
  );
}

function CreateVolume({ base, pool, onClose }: { base: string; pool: Pool; onClose: () => void }) {
  const [name, setName] = useState("");
  const [gib, setGib] = useState(20);
  const [format, setFormat] = useState("qcow2");
  const act = useAction();
  const fileBased = pool.type === "dir" || pool.type === "netfs" || pool.type === "fs";
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const full = fileBased && !/\.[a-z0-9]+$/i.test(name) ? `${name}.${format === "raw" ? "img" : format}` : name;
    if (await act.run(() => post(base, { name: full, capacity: gib * 2 ** 30, format: fileBased ? format : "" }))) onClose();
  };
  return (
    <Modal title={`New volume in ${pool.name}`} onClose={onClose}>
      <form onSubmit={submit}>
        <Field label="Name">
          <input value={name} onChange={(e) => setName(e.target.value)} required autoFocus />
        </Field>
        <div className="grid2">
          <Field label="Size (GiB)">
            <input type="number" min={1} value={gib} onChange={(e) => setGib(Number(e.target.value))} />
          </Field>
          {fileBased && (
            <Field label="Format" hint="qcow2 supports snapshots and grows on demand.">
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
          )}
        </div>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Create
          </button>
        </footer>
      </form>
    </Modal>
  );
}

const poolTypes: Record<string, string> = {
  dir: "Directory",
  netfs: "NFS share (untested)",
  logical: "LVM volume group (untested)",
  iscsi: "iSCSI target (untested)",
  xml: "Custom (XML) (untested)",
};
const dirDefault = (name: string) => (name ? `/var/lib/libvirt/images/${name}` : "");
const mountDefault = (name: string) => (name ? `/mnt/${name}` : "");

function CreatePool({ node: initialNode, nodes, onClose, onDone }: { node: number; nodes?: NodeStatus[]; onClose: () => void; onDone: () => void }) {
  const [node, setNode] = useState(initialNode);
  const [type, setType] = useState("dir");
  const [f, setF] = useState({ name: "", target: "", sourceHost: "", sourcePath: "", sourceName: "", xml: "" });
  // The directory and the NFS mount point follow the name until the user types their own.
  const [typed, setTyped] = useState<{ dir?: string; mount?: string }>({});
  const dir = typed.dir ?? dirDefault(f.name);
  const mount = typed.mount ?? mountDefault(f.name);
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const target = type === "dir" ? dir : type === "netfs" ? mount : "";
    const body = type === "xml" ? { xml: f.xml } : { ...f, target, xml: "", type };
    if (await act.run(() => post(`/nodes/${node}/pools`, body))) {
      onDone();
      onClose();
    }
  };
  return (
    <Modal title="New storage pool" onClose={onClose}>
      <form onSubmit={submit}>
        {nodes && (
          <Field label="Node">
            <NodePicker nodes={nodes} value={node} onChange={setNode} />
          </Field>
        )}
        {type !== "xml" && (
          <Field label="Name">
            <input value={f.name} onChange={set("name")} required autoFocus />
          </Field>
        )}
        {type === "dir" && (
          <Field label="Directory on the node" hint="Created if it does not exist.">
            <input
              value={dir}
              onChange={(e) => setTyped({ ...typed, dir: e.target.value })}
              required
              placeholder="/var/lib/libvirt/images/name"
            />
          </Field>
        )}
        <details className="cloudinit">
          <summary>Advanced</summary>
          <Field label="Other storage types">
            <Select label="Other storage types" value={type} onChange={setType} options={Object.entries(poolTypes).map(([k, v]) => ({ value: k, label: v }))} />
          </Field>
          {type === "netfs" && (
            <>
              <div className="grid2">
                <Field label="NFS server">
                  <input value={f.sourceHost} onChange={set("sourceHost")} required placeholder="nas.example.lan" />
                </Field>
                <Field label="Export path">
                  <input value={f.sourcePath} onChange={set("sourcePath")} required placeholder="/export/vms" />
                </Field>
              </div>
              <Field label="Mount point on the node">
                <input
                  value={mount}
                  onChange={(e) => setTyped({ ...typed, mount: e.target.value })}
                  required
                  placeholder="/mnt/name"
                />
              </Field>
            </>
          )}
          {type === "logical" && (
            <Field label="Volume group" hint="An existing LVM volume group on the node.">
              <input value={f.sourceName} onChange={set("sourceName")} required placeholder="vg0" />
            </Field>
          )}
          {type === "iscsi" && (
            <div className="grid2">
              <Field label="iSCSI server">
                <input value={f.sourceHost} onChange={set("sourceHost")} required placeholder="192.168.1.50" />
              </Field>
              <Field label="Target name">
                <input value={f.sourcePath} onChange={set("sourcePath")} required placeholder="iqn.2026-01.lan.example:vms" />
              </Field>
            </div>
          )}
          {type === "xml" && (
            <Field label="Pool definition" hint="Any pool type libvirt supports: Ceph RBD, ZFS, Gluster and others.">
              <textarea spellCheck={false} rows={10} value={f.xml} onChange={set("xml")} required placeholder="Pool definition (XML)" />
            </Field>
          )}
        </details>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Create
          </button>
        </footer>
      </form>
    </Modal>
  );
}
