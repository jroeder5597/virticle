// Bulk actions on the selected VMs. The browser fans out the ordinary per-VM
// calls (three at a time) and shows each VM's own result, so every action is a
// task of its own in the task log and failures are reported per VM.
import { useEffect, useState } from "react";
import { CheckList, CheckItem, StatusLine } from "../Checks";
import { del, get, post, put, VM, VMDetail } from "../api";
import { ErrorBox, Field, Modal, useAction, useConfirm } from "../ui";
import { powerConfirm, powerLabel } from "../power";
import { MAX_TAGS, TAG_RE } from "./Tags";
import { TagChip } from "../TagChip";
import { fetchTags, TagInfo } from "../tagStore";

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
  const confirm = useConfirm();

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

  const action = async (name: keyof typeof powerLabel, applies: (vm: VM) => string | undefined) => {
    // One confirmation for the whole selection, listing the VMs the action will reach.
    const spec = powerConfirm(name, selected.filter((vm) => !applies(vm)).map((vm) => vm.name));
    if (spec && !(await confirm.ask(spec))) return;
    execute(powerLabel[name], (vm) => {
      const why = applies(vm);
      return why ? { skip: why } : { run: () => post(`${base(vm)}/action`, { action: name }) };
    });
  };
  const notRunning = (vm: VM) => (vm.state === "shutoff" ? "not running" : undefined);

  const n = selected.length;
  return (
    <div className="bulkbar">
      <strong>{n} selected</strong>
      <button disabled={running} onClick={() => action("start", (vm) => (vm.template ? "template" : vm.state !== "shutoff" ? "already " + vm.state : undefined))}>
        Power On
      </button>
      <button disabled={running} onClick={() => action("shutdown", notRunning)}>
        Shutdown (graceful)
      </button>
      <button disabled={running} onClick={() => action("reboot", notRunning)}>
        Reboot (Graceful)
      </button>
      <button className="danger" disabled={running} onClick={() => action("poweroff", notRunning)}>
        Halt (Force Shutdown)
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
      {confirm.dialog}
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
          onConfirm={(disks, replicas) => {
            setDeleting(false);
            execute("Delete", (vm) => {
              if (vm.state !== "shutoff") return { skip: "running: shut it down first" };
              const q = [...(disks ? ["disks=1"] : []), ...(replicas && vm.replication ? ["replica=1"] : [])].join("&");
              return { run: () => del(`${base(vm)}${q ? "?" + q : ""}`) };
            });
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
      <CheckList
        label="Bulk results"
        sort={false}
        items={results.map(
          (r): CheckItem => ({
            key: key(r.vm),
            status: r.status === "ok" ? "ok" : r.status === "failed" ? "error" : r.status === "skipped" ? "info" : "pending",
            title: r.vm.name,
            detail: `on ${r.vm.nodeName}${r.status === "skipped" ? `, skipped: ${r.text}` : r.status === "failed" ? `, failed: ${r.text}` : r.status === "pending" ? ", waiting" : ""}`,
          }),
        )}
      />
      {!running && failed > 0 && <StatusLine status="error">The task log has the details.</StatusLine>}
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
  const [known, setKnown] = useState<TagInfo[]>([]);
  useEffect(() => {
    fetchTags().then(setKnown, () => {});
  }, []);
  const near = known.filter((k) => !tag.trim() || k.name.toLowerCase().includes(tag.trim().toLowerCase())).slice(0, 12);
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
      {near.length > 0 && (
        <div className="tags bulk-known" role="group" aria-label="Existing tags">
          {near.map((k) => (
            <button key={k.name} type="button" className="tagpick" onClick={() => setTag(k.name)}>
              <TagChip name={k.name} color={k.color} />
            </button>
          ))}
        </div>
      )}
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="primary" disabled={!ok} onClick={() => onApply(mode, tag.trim())}>
          Apply
        </button>
      </footer>
    </Modal>
  );
}

function DeleteDialog({ vms, onClose, onConfirm }: { vms: VM[]; onClose: () => void; onConfirm: (disks: boolean, replicas: boolean) => void }) {
  const [details, setDetails] = useState<Record<string, VMDetail>>({});
  const [disks, setDisks] = useState(false);
  const [replicas, setReplicas] = useState(true);
  const [typed, setTyped] = useState("");
  const act = useAction();
  useEffect(() => {
    act.run(async () => {
      const got = await Promise.all(vms.map((v) => get<VMDetail>(base(v)).then((d) => [key(v), d] as const)));
      setDetails(Object.fromEntries(got));
    });
  }, []);
  const deletable = vms.filter((v) => v.state === "shutoff");
  const replicated = deletable.filter((v) => v.replication).length;
  const word = "delete";
  const owned = (v: VM) => (details[key(v)]?.disks ?? []).filter((k) => k.device === "disk" && !k.readOnly && k.source);
  return (
    <Modal title={`Delete ${deletable.length} VM(s)?`} onClose={onClose}>
      <p>This removes the VM definitions together with their NVRAM files, TPM state and snapshot records. It cannot be undone.</p>
      <CheckList
        label="To be removed"
        sort={false}
        summary={false}
        items={vms.map(
          (v): CheckItem => ({
            key: key(v),
            status: v.state !== "shutoff" ? "warning" : "info",
            title: v.name,
            detail:
              `on ${v.nodeName}` +
              (v.state !== "shutoff"
                ? `, will be skipped: ${v.state}, shut it down first`
                : owned(v)
                    .map((k) => `; disk ${k.target}: ${k.source} (${disks ? "deleted" : "kept"})`)
                    .join("")),
          }),
        )}
      />
      <label className="check">
        <input type="checkbox" checked={disks} onChange={(e) => setDisks(e.target.checked)} /> Also delete their disk volumes
      </label>
      {replicated > 0 && (
        <label className="check">
          <input type="checkbox" checked={replicas} onChange={(e) => setReplicas(e.target.checked)} /> Also delete the replica of {replicated === 1 ? "the VM that has one" : `the ${replicated} VMs that have one`}
        </label>
      )}
      <Field label={`Type "${word}" to confirm`}>
        <input value={typed} onChange={(e) => setTyped(e.target.value)} autoFocus />
      </Field>
      <ErrorBox error={act.error} />
      <footer>
        <button onClick={onClose}>Cancel</button>
        <button className="danger" disabled={typed.trim() !== word || deletable.length === 0 || act.busy} onClick={() => onConfirm(disks, replicas)}>
          Delete {deletable.length} VM(s)
        </button>
      </footer>
    </Modal>
  );
}
