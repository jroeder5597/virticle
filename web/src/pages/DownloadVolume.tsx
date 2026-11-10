import { FormEvent, useState } from "react";
import { post } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";

/** Asks the node to fetch a URL into a pool. Progress shows in the task log. */
export default function DownloadVolume(props: { node: number; pool: string; onClose: () => void; onDone: () => void }) {
  const act = useAction();
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");

  // Suggest the file name from the URL until the user types their own.
  const onUrl = (u: string) => {
    setUrl(u);
    if (!name || name === suggest(url)) setName(suggest(u));
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (await act.run(() => post(`/nodes/${props.node}/pools/${encodeURIComponent(props.pool)}/volumes/download`, { name, url }))) {
      props.onDone();
      props.onClose();
    }
  };
  return (
    <Modal title={`Download into ${props.pool}`} onClose={props.onClose}>
      <form onSubmit={submit}>
        <Field label="URL" hint="http or https. The node downloads it directly; this can take a while for large images.">
          <input type="url" value={url} onChange={(e) => onUrl(e.target.value)} required autoFocus placeholder="https://…/image.qcow2" pattern="https?://.+" />
        </Field>
        <Field label="Volume name">
          <input value={name} onChange={(e) => setName(e.target.value)} required pattern="[A-Za-z0-9][A-Za-z0-9._+\-]*" />
        </Field>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={props.onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            {act.busy ? "Downloading…" : "Download"}
          </button>
        </footer>
      </form>
    </Modal>
  );
}

function suggest(u: string): string {
  try {
    return decodeURIComponent(new URL(u).pathname.split("/").pop() ?? "");
  } catch {
    return "";
  }
}
