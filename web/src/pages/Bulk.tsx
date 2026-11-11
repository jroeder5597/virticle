// Bulk actions on the selected VMs. The browser fans out the ordinary per-VM
// calls (three at a time) and shows each VM's own result, so every action is a
// task of its own in the task log and failures are reported per VM.
import { useEffect, useState } from "react";
import { Check, Ellipsis, Minus, X } from "lucide-react";
import { del, get, post, put, VM, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction } from "../ui";
import { MAX_TAGS, TAG_RE } from "./Tags";

const CONCURRENCY = 3;

interface Result {
  vm: VM;
  status: "pending" | "ok" | "skipped" | "failed";
  text?: string;
}

async function inPool<T>(items: T[], limit: number, fn: (x: T, i: number) => Promise<void>) {
  let next = 0;
  await Promise.all(
    Array.from({ length: Math.min(limit, items.length) }, async () => {
      while (next < items.length) {
        const i = next++;
        await fn(items[i], i);
      }
    }),
  );
}

const key = (v: VM) => v.node + "/" + v.uuid;
const base = (v: VM) => `/nodes/${v.node}/vms/${v.uuid}`;

/** What to do for one VM: a skip reason, a no-op reason, or the call. */
type Plan = (vm: VM) => { skip: string } | { run: () => Promise<unknown> };

export function BulkBar({ selected, onClear }: { selected: VM[]; onClear: () => void }) {
  const [title, setTitle] = useState("");
  const [results, setResults] = useState<Result[] | undefined>();
  const [running, setRunning] = useState(false);
  const [tagging, setTagging] = useState(false);
  const [deleting, setDeleting] = useState(false);

  const execute = async (label: string, plan: Plan) => {
    setTitle(label);
    setRunning(true);
    const res: Result[] = selected.map((vm) => ({ vm, status: "pending" }));
    setResults([...res]);
    await inPool(selected, CONCURRENCY, async (vm, i) => {
      const p = plan(vm);
      if ("skip" in p) res[i] = { vm, status: "skipped", text: p.skip };
      else {
        try {
          await p.run();
          res[i] = { vm, status: "ok" };
        } catch (e: any) {
          res[i] = { vm, status: "failed", text: String(e.message ?? e) };
        }
      }
      setResults([...res]);
    });
    setRunning(false);
  };

  const action = (label: string, name: string, applies: (vm: VM) => string | undefined) =>
    execute(label, (vm) => {
      const why = applies(vm);
      return why ? { skip: why } : { run: () => post(`${base(vm)}/action`, { action: name }) };
    });
  const notRunning = (vm: VM) => (vm.state === "shutoff" ? "not running" : undefined);

  const n = selected.length;
  return (
    <div className="bulkbar">
      <strong>{n} selected</strong>
      <button disabled={running} onClick={() => action("Start", "start", (vm) => (vm.template ? "template" : vm.state !== "shutoff" ? "already " + vm.state : undefined))}>
        Start
      </button>
      <button disabled={running} onClick={() => action("Shut down", "shutdown", notRunning)}>
        Shut down
      </button>
      <button disabled={running} onClick={() => action("Reboot", "reboot", notRunning)}>
        Reboot
      </button>
      <button className="danger" disabled={running} onClick={() => window.confirm(`Force ${n} VM(s) off? Unsaved data in the guests is lost.`) && action("Force off", "poweroff", notRunning)}>
        Force off
      </button>
      <button disabled={running} onClick={() => setTagging(true)}>
        Tags
      </button>
      <button className="danger" disabled={running} onClick={() => setDeleting(true)}>
        Delete
      </button>
      <button className="ghost" onClick={onClear}>
        Clear selection
      </button>
      {results && <BulkResults title={title} results={results} running={running} onClose={() => { setResults(undefined); if (selected.length === 0) onClear(); }} />}
      {tagging && (
        <TagDialog
          onClose={() => setTagging(false)}
          onApply={(mode, tag) => {
            setTagging(false);
            execute(`${mode === "add" ? "Add" : "Remove"} tag "${tag}"`, (vm) => {
              const have = vm.tags ?? [];
              if (mode === "add" && have.includes(tag)) return { skip: "already tagged" };
              if (mode === "remove" && !have.includes(tag)) return { skip: "not tagged" };
              const next = mode === "add" ? [...have, tag] : have.filter((t) => t !== tag);
              return { run: () => put(`${base(vm)}/tags`, { tags: next }) };
            });
          }}
        />
      )}
      {deleting && (
        <DeleteDialog
          vms={selected}
          onClose={() => setDeleting(false)}
          onConfirm={(disks) => {
            setDeleting(false);
            execute("Delete", (vm) => (vm.state !== "shutoff" ? { skip: "running: shut it down first" } : { run: () => del(`${base(vm)}${disks ? "?disks=1" : ""}`) }));
          }}
        />
      )}
    </div>
  );
}

function BulkResults({ title, results, running, onClose }: { title: string; results: Result[]; running: boolean; onClose: () => void }) {
  const done = results.filter((r) => r.status !== "pending").length;
  const failed = results.filter((r) => r.status === "failed").length;
  return (
    <Modal title={`${title}: ${done} of ${results.length} done`} onClose={running ? () => {} : onClose}>
      <ul className="checks" aria-label="Bulk results">
        {results.map((r) => (
          <li key={key(r.vm)} className={r.status === "ok" ? "ok" : ""}>
            <span className="mark">{r.status === "ok" ? <Check size={14} aria-label="ok" /> : r.status === "failed" ? <X size={14} aria-label="failed" /> : r.status === "skipped" ? <Minus size={14} aria-label="skipped" /> : <Ellipsis size={14} aria-label="running" />}</span>
            <span>
              <strong>{r.vm.name}</strong> <span className="muted">on {r.vm.nodeName}</span>
              {r.status === "skipped" && <span className="muted"> skipped: {r.text}</span>}
              {r.status === "failed" && <div className="rowerror">{r.text}</div>}
            </span>
          </li>
        ))}
      </ul>
      {!running && failed > 0 && <div className="error">{failed} failed. The task log has the details.</div>}
      <footer>
        <button className="primary" disabled={running} onClick={onClose}>
          Close
        </button>
      </footer>
    </Modal>
  );
}

function TagDialog({ onClose, onApply }: { onClose: () => void; onApply: (mode: "add" | "remove", tag: string) => void }) {
  const [mode, setMode] = useState<"add" | "remove">("add");
  const [tag, setTag] = useState("");
  const ok = TAG_RE.test(tag.trim());
  return (
    <Modal title="Tags on the selected VMs" onClose={onClose}>
      <div className="segmented">
        <button type="button" className={mode === "add" ? "on" : ""} onClick={() => setMode("add")}>
          Add tag
        </button>
        <button type="button" className={mode === "remove" ? "on" : ""} onClick={() => setMode("remove")}>
          Remove tag
        </button>
      </div>
      <Field label="Tag" hint={`Up to ${MAX_TAGS} tags per VM; letters, digits, space and . _ : -`}>
        <input value={tag} onChange={(e) => setTag(e.target.value)} autoFocus maxLength={31} />
      </Field>
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={!ok} onClick={() => onApply(mode, tag.trim())}>
          Apply
        </button>
      </footer>
    </Modal>
  );
}

function DeleteDialog({ vms, onClose, onConfirm }: { vms: VM[]; onClose: () => void; onConfirm: (disks: boolean) => void }) {
  const [details, setDetails] = useState<Record<string, VMDetail>>({});
  const [disks, setDisks] = useState(false);
  const [typed, setTyped] = useState("");
  const act = useAction();
  useEffect(() => {
    act.run(async () => {
      const got = await Promise.all(vms.map((v) => get<VMDetail>(base(v)).then((d) => [key(v), d] as const)));
      setDetails(Object.fromEntries(got));
    });
  }, []);
  const deletable = vms.filter((v) => v.state === "shutoff");
  const word = "delete";
  const owned = (v: VM) => (details[key(v)]?.disks ?? []).filter((k) => k.device === "disk" && !k.readOnly && k.source);
  return (
    <Modal title={`Delete ${deletable.length} VM(s)?`} onClose={onClose}>
      <p>This removes the VM definitions together with their NVRAM files, TPM state and snapshot records. It cannot be undone.</p>
      <ul className="checks" aria-label="To be removed">
        {vms.map((v) => (
          <li key={key(v)}>
            <span>
              <strong>{v.name}</strong> <span className="muted">on {v.nodeName}</span>
              {v.state !== "shutoff" ? (
                <div className="muted">will be skipped: {v.state}, shut it down first</div>
              ) : (
                owned(v).map((k) => (
                  <div key={k.target} className="mono muted">
                    disk {k.target}: {k.source} ({disks ? "deleted" : "kept"})
                  </div>
                ))
              )}
            </span>
          </li>
        ))}
      </ul>
      <label className="check">
        <input type="checkbox" checked={disks} onChange={(e) => setDisks(e.target.checked)} /> Also delete their disk volumes
      </label>
      <Field label={`Type "${word}" to confirm`}>
        <input value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
      </Field>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="danger" disabled={typed.trim() !== word || deletable.length === 0 || act.busy} onClick={() => onConfirm(disks)}>
          Delete {deletable.length} VM(s)
        </button>
      </footer>
    </Modal>
  );
}
