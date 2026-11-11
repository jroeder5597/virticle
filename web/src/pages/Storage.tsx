import { FormEvent, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { bytes, del, get, NodeStatus, Pool, post, uploadVolume, Volume } from "../api";
import { orEvents, useLive } from "../live";
import DownloadVolume from "./DownloadVolume";
import { AlertMarks, useAlerts } from "./Alerts";
import ImportDialog from "./Import";
import { ErrorBox, Field, Modal, NodePicker, StateBadge, UsageBar, useAction } from "../ui";

/** `fixedNode` pins the page to one node (node page tab): no node picker, no node auto-selection. */
export default function Storage({ fixedNode }: { fixedNode?: number } = {}) {
  const nodes = useLive(() => get<{ nodes: NodeStatus[] }>("/nodes"), "nodes");
  const [pickedNode, setNode] = useState(0);
  const node = fixedNode ?? pickedNode;
  const [selected, setSelected] = useState("");
  const [creating, setCreating] = useState(false);
  const all = nodes.data?.nodes ?? [];
  useEffect(() => {
    if (fixedNode === undefined && !all.some((n) => n.id === node && n.state === "online")) setNode(all.find((n) => n.state === "online")?.id ?? 0);
  }, [all, node]);
  const pools = useLive(() => (node ? get<Pool[]>(`/nodes/${node}/pools`) : Promise.resolve([])), `pools/${node}`, orEvents("pool"));
  const alerts = useAlerts();
  const act = useAction();
  const pool = pools.data?.find((p) => p.name === selected) ?? pools.data?.[0];
  const poolAction = (p: Pool, action: string) => act.run(() => post(`/nodes/${node}/pools/${encodeURIComponent(p.name)}/action`, { action }));

  if (fixedNode === undefined && nodes.data && all.length === 0) {
    return (
      <>
        <h1>Storage</h1>
        <div className="empty">
          No nodes yet. <Link to="/nodes">Add a node</Link> to manage its storage.
        </div>
      </>
    );
  }
  return (
    <>
      <div className="head">
        {fixedNode === undefined ? <h1>Storage</h1> : <h2>Storage pools</h2>}
        <div className="actions">
          {fixedNode === undefined && <NodePicker nodes={all} value={node} onChange={setNode} />}
          <button className="primary" disabled={!node} onClick={() => setCreating(true)}>
            New pool
          </button>
        </div>
      </div>
      <ErrorBox error={pools.error ?? act.error} />
      <table>
        <thead>
          <tr>
            <th>Pool</th>
            <th>Type</th>
            <th>State</th>
            <th>Location</th>
            <th>Usage</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {(pools.data ?? []).map((p) => (
            <tr key={p.name} className={pool?.name === p.name ? "selected" : ""} onClick={() => setSelected(p.name)}>
              <td>
                {p.name}
                <AlertMarks alerts={alerts} match={(a) => a.id === `pool:${node}:${p.name}`} />
              </td>
              <td>{p.type}</td>
              <td>
                <StateBadge state={p.state} />
              </td>
              <td className="mono">{p.target}</td>
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
              <td className="actions" onClick={(e) => e.stopPropagation()}>
                {p.state === "running" ? (
                  <button disabled={act.busy} onClick={() => poolAction(p, "stop")}>
                    Stop
                  </button>
                ) : (
                  <button disabled={act.busy} onClick={() => poolAction(p, "start")}>
                    Start
                  </button>
                )}
                <button
                  className="danger"
                  disabled={act.busy}
                  onClick={() => window.confirm(`Remove pool ${p.name} from this node? The data in it is not deleted.`) && poolAction(p, "remove")}
                >
                  Remove
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {pool && pool.state === "running" && <Volumes node={node} pool={pool} />}
      {creating && <CreatePool node={node} onClose={() => setCreating(false)} />}
    </>
  );
}

function Volumes({ node, pool }: { node: number; pool: Pool }) {
  const base = `/nodes/${node}/pools/${encodeURIComponent(pool.name)}/volumes`;
  const vols = useLive(() => get<Volume[]>(base), base);
  const act = useAction();
  const [creating, setCreating] = useState(false);
  const [downloading, setDownloading] = useState(false);
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
  const resize = (v: Volume) => {
    const answer = window.prompt(`New size for ${v.name} in GiB (currently ${(v.capacity / 2 ** 30).toFixed(1)}). Volumes can only grow.`);
    const gib = Number(answer);
    if (answer && gib > 0) volAction(v, { action: "resize", capacity: Math.round(gib * 2 ** 30) });
  };
  const clone = (v: Volume) => {
    const name = window.prompt(`Name for the copy of ${v.name}:`, v.name.replace(/(\.[^.]+)?$/, "-copy$1"));
    if (name) volAction(v, { action: "clone", name });
  };

  return (
    <>
      <div className="head sub">
        <h2>Volumes in {pool.name}</h2>
        <div className="actions">
          <input ref={file} type="file" hidden onChange={(e) => onFile(e.target.files?.[0])} />
          <button disabled={!!upload} onClick={() => file.current?.click()}>
            Upload image or ISO
          </button>
          <button onClick={() => setDownloading(true)}>Download from URL</button>
          <button onClick={() => setImporting("")}>Import disk or OVA</button>
          <button onClick={() => setCreating(true)}>New volume</button>
        </div>
      </div>
      {upload && (
        <div className="progress">
          Uploading {upload.name}… {(upload.progress * 100).toFixed(0)}%
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
                    onClick={() => window.confirm(`Delete ${v.name}? Its data is destroyed and cannot be recovered.`) && act.run(() => del(`${base}/${encodeURIComponent(v.name)}`))}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {downloading && <DownloadVolume node={node} pool={pool.name} onClose={() => setDownloading(false)} onDone={vols.reload} />}
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
              <select value={format} onChange={(e) => setFormat(e.target.value)}>
                <option value="qcow2">qcow2</option>
                <option value="raw">raw</option>
              </select>
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

const poolTypes: Record<string, string> = { dir: "Directory", netfs: "NFS share", logical: "LVM volume group", iscsi: "iSCSI target", xml: "Other (libvirt XML)" };

function CreatePool({ node, onClose }: { node: number; onClose: () => void }) {
  const [type, setType] = useState("dir");
  const [f, setF] = useState({ name: "", target: "", sourceHost: "", sourcePath: "", sourceName: "", xml: "" });
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const act = useAction();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body = type === "xml" ? { xml: f.xml } : { ...f, xml: "", type };
    if (await act.run(() => post(`/nodes/${node}/pools`, body))) onClose();
  };
  return (
    <Modal title="New storage pool" onClose={onClose}>
      <form onSubmit={submit}>
        <Field label="Type">
          <select value={type} onChange={(e) => setType(e.target.value)}>
            {Object.entries(poolTypes).map(([k, v]) => (
              <option key={k} value={k}>
                {v}
              </option>
            ))}
          </select>
        </Field>
        {type !== "xml" && (
          <Field label="Name">
            <input value={f.name} onChange={set("name")} required autoFocus />
          </Field>
        )}
        {type === "dir" && (
          <Field label="Directory on the node" hint="Created if it does not exist.">
            <input value={f.target} onChange={set("target")} required placeholder="/var/lib/libvirt/images" />
          </Field>
        )}
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
              <input value={f.target} onChange={set("target")} required placeholder="/mnt/vms" />
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
            <Field label="Portal address">
              <input value={f.sourceHost} onChange={set("sourceHost")} required placeholder="192.168.1.50" />
            </Field>
            <Field label="Target IQN">
              <input value={f.sourcePath} onChange={set("sourcePath")} required placeholder="iqn.2026-01.lan.example:vms" />
            </Field>
          </div>
        )}
        {type === "xml" && (
          <Field label="Pool definition" hint="Any pool type libvirt supports: Ceph RBD, ZFS, Gluster and others.">
            <textarea spellCheck={false} rows={10} value={f.xml} onChange={set("xml")} required placeholder="<pool type='rbd'>…</pool>" />
          </Field>
        )}
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
