import { useState } from "react";
import { Link } from "react-router-dom";
import { bytes, get, post } from "../api";
import { useEvents, useLive } from "../live";
import { ErrorBox, Modal, useAction, useConfirm } from "../ui";
import "./imagecat.css";

export interface CatalogueImage {
  id: string;
  name: string;
  version: string;
  url: string;
  algorithm: string;
  defaultUser?: string;
  volumeName: string;
  cloudInit: boolean;
  sizeBytes?: number;
  present: boolean;
  downloading: boolean;
}
interface PoolImages {
  images: CatalogueImage[];
  downloadable: boolean;
  reason?: string;
  running: string[];
}

/**
 * Well-known cloud images the node downloads itself and verifies by checksum.
 * The checksum comes from the same project as the image: it catches a corrupt
 * download, not a compromised mirror.
 */
export default function ImageCatalogue(props: { node: number; nodeName?: string; pool: string; onClose: () => void; onDone: () => void }) {
  const base = `/nodes/${props.node}/pools/${encodeURIComponent(props.pool)}/images`;
  const list = useLive(() => get<PoolImages>(base), base);
  const act = useAction();
  const confirm = useConfirm();
  const [progress, setProgress] = useState<Record<string, number>>({});
  const [failure, setFailure] = useState<string | undefined>();

  // Task events already flow over the event stream: use them for progress and for the outcome.
  useEvents((e) => {
    const t = e.task;
    if (e.type !== "task" || !t || t.kind !== "image" || !t.target.startsWith(props.pool + "/")) return;
    if (props.nodeName && t.node && t.node !== props.nodeName) return; // another node's pool of the same name
    const vol = t.target.slice(props.pool.length + 1);
    if (t.status === "running") {
      setProgress((p) => ({ ...p, [vol]: t.progress }));
      return;
    }
    setProgress((p) => {
      const { [vol]: _gone, ...rest } = p;
      return rest;
    });
    setFailure(t.status === "failed" ? `${vol}: ${t.error ?? "failed"}` : undefined);
    props.onDone();
  });

  const start = async (img: CatalogueImage) => {
    setFailure(undefined);
    if (await act.run(() => post(base, { id: img.id }))) {
      setProgress((p) => ({ ...p, [img.volumeName]: 0 }));
      list.reload();
    }
  };
  const cancel = async (img: CatalogueImage) => {
    if (!(await confirm.ask({ title: "Stop download", message: `The download of ${img.name} into pool ${props.pool} is stopped and the partial file is removed.`, confirm: "Stop download" }))) return;
    return act.run(() => post(base + "/cancel", { name: img.volumeName }));
  };
  const running = new Set([...(list.data?.running ?? []), ...Object.keys(progress)]);

  return (
    <Modal title="Image catalogue" onClose={props.onClose} wide>
      <p className="muted catnote">
        The node downloads the image into <b>{props.pool}</b> and checks its checksum before the file appears. The checksum comes from the same project as the image: it detects a damaged download, not a compromised mirror.
      </p>
      {confirm.dialog}
      <ErrorBox error={list.error ?? act.error ?? failure} />
      {list.data && !list.data.downloadable && <div className="empty">{list.data.reason ?? "This pool cannot be downloaded into."}</div>}
      {list.data?.downloadable && (
        <ul className="catlist">
          {list.data.images.map((img) => {
            const busy = running.has(img.volumeName);
            const pct = progress[img.volumeName];
            return (
              <li key={img.id} className="catitem">
                <div className="catmain">
                  <div className="cattitle">
                    <b>{img.name}</b>
                    <span className="badge">{img.cloudInit ? "cloud-init" : "no cloud-init"}</span>
                    {img.present && <span className="badge catok">already in this pool</span>}
                  </div>
                  <small className="muted">
                    ~{bytes(img.sizeBytes ?? 0)}
                    {img.defaultUser && (
                      <>
                        {" · login "}
                        <span className="mono">{img.defaultUser}</span>
                      </>
                    )}
                    {" · "}
                    <span className="mono catvol">{img.volumeName}</span>
                  </small>
                </div>
                <div className="catact">
                  {busy ? (
                    <>
                      <span className="muted">{pct ? `${Math.round(pct * 100)}%` : "starting"}</span>
                      <button disabled={act.busy} onClick={() => cancel(img)}>
                        Cancel
                      </button>
                    </>
                  ) : img.present ? (
                    <Link className="button" to={`/?new=${props.node}&pool=${encodeURIComponent(props.pool)}&image=${encodeURIComponent(img.volumeName)}`} onClick={props.onClose}>
                      Create VM from this image
                    </Link>
                  ) : (
                    <button className="primary" disabled={act.busy} onClick={() => start(img)}>
                      Download
                    </button>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
      <footer>
        <button onClick={props.onClose}>Close</button>
      </footer>
    </Modal>
  );
}
