import { useEffect, useState } from "react";
import { bytes, get, put, VMDetail } from "../api";
import { ErrorBox, Field, useAction, useConfirm } from "../ui";
import { OneLine, SectionFrame } from "./Disclosure";
import { CheckList } from "../Checks";

interface Tuning {
  state: string;
  maxVcpus: number;
  memoryKiB: number;
  saved: {
    vcpus: number;
    vcpuCpuset?: string;
    pins: { vcpu: number; cpuset: string }[];
    emulatorPin?: string;
    cpuMode: string;
    cpuModel?: string;
    topology?: { sockets: number; dies?: number; clusters?: number; cores: number; threads: number };
    hugepages: boolean;
    hugepageKiB?: number;
    numaMode?: string;
    numaNodeset?: string;
  };
  pendingRestart: boolean;
  host: {
    cpus: number[];
    cells: { id: number; memoryKiB: number; cpus: number[]; pages: { sizeKiB: number; total: number; free: number }[] }[];
    cpuModels: string[];
    hugepageSizesKiB: number[];
  };
  warning: string;
  warnings: string[];
}
interface Result {
  note?: string;
  needsRestart: boolean;
}

function cpuList(cpus: number[]): string {
  const out: string[] = [];
  for (let i = 0; i < cpus.length; ) {
    let j = i;
    while (j + 1 < cpus.length && cpus[j + 1] === cpus[j] + 1) j++;
    out.push(j === i ? `${cpus[i]}` : `${cpus[i]}-${cpus[j]}`);
    i = j + 1;
  }
  return out.join(",");
}

/** CPU pinning, CPU model and topology, huge pages and NUMA policy of one VM. */
/** One line for the collapsed header: what is set now. */
function tuningSummary(t: Tuning): string {
  const s = t.saved;
  const pinned = s.pins.length + (s.emulatorPin ? 1 : 0);
  return [
    pinned ? `${pinned} CPU pin${pinned > 1 ? "s" : ""}` : "No pinning",
    s.hugepages ? (s.hugepageKiB ? `${bytes(s.hugepageKiB * 1024)} huge pages` : "huge pages") : "ordinary pages",
    s.numaMode ? `NUMA ${s.numaMode} on node ${s.numaNodeset ?? "?"}` : "default NUMA policy",
  ].join(", ");
}

export default function AdvancedCard({ d, base, onChanged, bare }: { d: VMDetail; base: string; onChanged: () => void; bare?: boolean }) {
  const [t, setT] = useState<Tuning | undefined>();
  const [loadErr, setLoadErr] = useState<string | undefined>();
  const [msg, setMsg] = useState<string | undefined>();
  const act = useAction();
  const load = () => get<Tuning>(`${base}/tuning`).then((x) => { setT(x); setLoadErr(undefined); }, (e) => setLoadErr(String(e.message ?? e)));
  useEffect(() => {
    load();
  }, [base, d.state]);
  const save = (path: string, body: unknown) =>
    act.run(async () => {
      const r = await put<Result>(`${base}/tuning/${path}`, body);
      setMsg(r.note);
      await load();
      onChanged();
    });

  const title = "Advanced: CPU placement, huge pages, NUMA";
  if (loadErr)
    return (
      <SectionFrame id="hw-advanced" title={title} summary="Could not be read" bare={bare}>
        <ErrorBox error={loadErr} />
      </SectionFrame>
    );
  if (!t)
    return (
      <SectionFrame id="hw-advanced" title={title} summary="Loading" bare={bare}>
        <div className="muted">Loading</div>
      </SectionFrame>
    );
  const s = t.saved;
  return (
    <SectionFrame id="hw-advanced" title={title} summary={tuningSummary(t)} bare={bare}>
      <div className="advanced">
      <OneLine
        tone="note"
        text={`Applies at the next start.${t.pendingRestart ? " Not active in the running VM yet." : ""}`}
        details={t.warning}
        summary="Why this matters"
      />
      {t.warnings.length > 0 && <CheckList summary={false} label="Tuning warnings" items={t.warnings.map((w) => ({ status: "warning", detail: w }))} />}
      <ErrorBox error={act.error} />
      {msg && <div className="muted">{msg}</div>}

      <div className="tgrid">
        <HostTopology t={t} />
        <CpuShape key={JSON.stringify([s.cpuMode, s.cpuModel, s.topology])} t={t} busy={act.busy} save={save} />
        <Pinning key={JSON.stringify([s.pins, s.emulatorPin])} t={t} busy={act.busy} save={save} />
        <Hugepages key={String(s.hugepageKiB) + s.hugepages} t={t} busy={act.busy} save={save} />
        <Numa key={`${s.numaMode}/${s.numaNodeset}`} t={t} busy={act.busy} save={save} />
      </div>
      </div>
    </SectionFrame>
  );
}

type SaveFn = (path: string, body: unknown) => Promise<boolean>;
interface P {
  t: Tuning;
  busy: boolean;
  save: SaveFn;
}

function HostTopology({ t }: { t: Tuning }) {
  return (
    <div>
      <h4>This node</h4>
      <table className="compact">
        <thead>
          <tr>
            <th>NUMA node</th>
            <th>CPUs</th>
            <th>Memory</th>
            <th>Free huge pages</th>
          </tr>
        </thead>
        <tbody>
          {t.host.cells.map((c) => (
            <tr key={c.id}>
              <td>{c.id}</td>
              <td>{cpuList(c.cpus)}</td>
              <td>{bytes(c.memoryKiB * 1024)}</td>
              <td>
                {c.pages.filter((p) => p.sizeKiB > 4).map((p) => (
                  <div key={p.sizeKiB}>
                    {bytes(p.sizeKiB * 1024)}: {p.free} of {p.total}
                  </div>
                ))}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {t.host.cells.length === 0 && <div className="muted">The topology of the node could not be read.</div>}
    </div>
  );
}

function CpuShape({ t, busy, save }: P) {
  const s = t.saved;
  const [mode, setMode] = useState(s.cpuMode);
  const [model, setModel] = useState(s.cpuModel || t.host.cpuModels.find((m) => m === "qemu64") || t.host.cpuModels[0] || "");
  const [tp, setTp] = useState(s.topology ?? { sockets: 1, cores: t.maxVcpus, threads: 1 });
  const [none, setNone] = useState(!s.topology);
  return (
    <div>
      <h4>CPU model and topology</h4>
      <Field label="CPU mode" hint={mode === "host-passthrough" ? "Needs an identical CPU on a migration target." : mode === "host-model" ? "Closest named model." : undefined}>
        <select value={mode} onChange={(e) => setMode(e.target.value)}>
          <option value="host-model">host-model</option>
          <option value="host-passthrough">host-passthrough</option>
          <option value="custom">custom model</option>
        </select>
      </Field>
      {mode === "custom" && (
        <Field label="Model">
          <select value={model} onChange={(e) => setModel(e.target.value)}>
            {t.host.cpuModels.map((m) => (
              <option key={m}>{m}</option>
            ))}
          </select>
        </Field>
      )}
      <label className="check">
        <input type="checkbox" checked={none} onChange={(e) => setNone(e.target.checked)} /> No explicit topology (libvirt decides)
      </label>
      {!none && (
        <>
          <div className="grid3">
            <Field label="Sockets">
              <input type="number" min={1} value={tp.sockets} onChange={(e) => setTp({ ...tp, sockets: Number(e.target.value) })} />
            </Field>
            <Field label="Cores">
              <input type="number" min={1} value={tp.cores} onChange={(e) => setTp({ ...tp, cores: Number(e.target.value) })} />
            </Field>
            <Field label="Threads">
              <input type="number" min={1} value={tp.threads} onChange={(e) => setTp({ ...tp, threads: Number(e.target.value) })} />
            </Field>
          </div>
          <small>
            Sockets × cores × threads must equal the VM's maximum vCPU count ({t.maxVcpus}); now {tp.sockets * tp.cores * tp.threads * (s.topology?.dies ?? 1) * (s.topology?.clusters ?? 1)}.
          </small>
        </>
      )}
      <footer style={{ justifyContent: "flex-start" }}>
        <button disabled={busy} onClick={() => save("cpu", { mode, model: mode === "custom" ? model : "", noTopology: none, sockets: none ? 0 : tp.sockets, cores: none ? 0 : tp.cores, threads: none ? 0 : tp.threads })}>
          Save CPU
        </button>
      </footer>
    </div>
  );
}

function Pinning({ t, busy, save }: P) {
  const s = t.saved;
  const [pins, setPins] = useState<string[]>(() => Array.from({ length: t.maxVcpus || s.vcpus }, (_, i) => s.pins.find((p) => p.vcpu === i)?.cpuset ?? ""));
  const [emu, setEmu] = useState(s.emulatorPin ?? "");
  const confirm = useConfirm();
  const hostList = cpuList(t.host.cpus);
  const apply = () =>
    save("pinning", {
      vcpus: pins.map((cpuset, vcpu) => ({ vcpu, cpuset: cpuset.trim() })).filter((p) => p.cpuset !== ""),
      emulator: emu.trim(),
    });
  return (
    <div>
      <h4>vCPU pinning</h4>
      <small>Host CPUs {hostList}. Empty: no pin. Syntax 0-3,6 or ^1.</small>
      <table className="compact">
        <tbody>
          {pins.map((v, i) => (
            <tr key={i}>
              <td>vCPU {i}</td>
              <td>
                <input aria-label={`vCPU ${i} host CPUs`} value={v} placeholder="any" onChange={(e) => setPins(pins.map((x, j) => (j === i ? e.target.value : x)))} />
              </td>
            </tr>
          ))}
          <tr>
            <td>Emulator threads</td>
            <td>
              <input aria-label="Emulator host CPUs" value={emu} placeholder="any" onChange={(e) => setEmu(e.target.value)} />
            </td>
          </tr>
        </tbody>
      </table>
      <footer style={{ justifyContent: "flex-start" }}>
        <button disabled={busy} onClick={apply}>
          Save pinning
        </button>
        <button disabled={busy || (s.pins.length === 0 && !s.emulatorPin)} onClick={async () => (await confirm.ask({ title: "Remove CPU pinning", message: "All CPU pinning of this VM is removed. Its vCPUs and emulator threads may then run on any host CPU.", confirm: "Remove pinning" })) && save("pinning", { clear: true })}>
          Remove pinning
        </button>
      </footer>
      {confirm.dialog}
    </div>
  );
}

function Hugepages({ t, busy, save }: P) {
  const s = t.saved;
  const [size, setSize] = useState(s.hugepages ? s.hugepageKiB ?? 0 : 0);
  const free = (k: number) => t.host.cells.reduce((n, c) => n + (c.pages.find((p) => p.sizeKiB === k)?.free ?? 0), 0);
  const need = size ? Math.ceil(t.memoryKiB / size) : 0;
  return (
    <div>
      <h4>Huge pages</h4>
      <Field label="Back guest memory with" hint="Reserve pages on the node first (vm.nr_hugepages).">
        <select value={size} onChange={(e) => setSize(Number(e.target.value))}>
          <option value={0}>ordinary pages</option>
          {t.host.hugepageSizesKiB.map((k) => (
            <option key={k} value={k}>
              {bytes(k * 1024)} pages
            </option>
          ))}
          {s.hugepages && !s.hugepageKiB && <option disabled>(default size, set elsewhere)</option>}
        </select>
      </Field>
      {size > 0 && (
        <div className={free(size) < need ? "error" : "muted"}>
          Needs {need} pages; {free(size)} free on the node.
          {free(size) < need && " The VM would not start."}
        </div>
      )}
      <footer style={{ justifyContent: "flex-start" }}>
        <button disabled={busy} onClick={() => save("hugepages", { pageSizeKiB: size })}>
          Save huge pages
        </button>
      </footer>
    </div>
  );
}

function Numa({ t, busy, save }: P) {
  const s = t.saved;
  const [mode, setMode] = useState(s.numaMode ?? "");
  const [nodeset, setNodeset] = useState(s.numaNodeset ?? (t.host.cells[0] ? String(t.host.cells[0].id) : "0"));
  return (
    <div>
      <h4>NUMA memory policy</h4>
      <div className="grid2">
        <Field label="Policy" hint={mode === "strict" ? "Allocation fails if the nodes have no memory left." : mode === "preferred" ? "Tries this node first, falls back to others." : undefined}>
          <select value={mode} onChange={(e) => setMode(e.target.value)}>
            <option value="">none (default)</option>
            <option value="strict">strict</option>
            <option value="preferred">preferred</option>
            <option value="interleave">interleave</option>
          </select>
        </Field>
        {mode && (
          <Field label="Host NUMA nodes" hint={`This node has: ${t.host.cells.map((c) => c.id).join(", ")}`}>
            <input value={nodeset} onChange={(e) => setNodeset(e.target.value)} />
          </Field>
        )}
      </div>
      {t.host.cells.length === 1 && <small>Single NUMA node here; a policy matters after a move to a larger node.</small>}
      <footer style={{ justifyContent: "flex-start" }}>
        <button disabled={busy} onClick={() => save("numa", { mode, nodeset: mode ? nodeset : "" })}>
          Save NUMA policy
        </button>
      </footer>
    </div>
  );
}
