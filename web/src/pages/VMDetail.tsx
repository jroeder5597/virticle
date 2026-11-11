import { useEffect, useState } from "react";
import { ArrowLeft } from "lucide-react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { get, put, VMDetail } from "../api";
import { useLive } from "../live";
import { useRole } from "../auth";
import { ErrorBox, StateBadge, useAction } from "../ui";
import { SerialConsole, VNCConsole } from "./Console";
import Hardware from "./Hardware";
import SnapshotsTab from "./Snapshots";
import BackupsTab from "./Backups";
import VMActions from "./VMActions";
import Overview from "./VMOverview";
import { TagEditor } from "./Tags";

export default function VMDetailPage() {
  const { node: nodeParam, uuid = "" } = useParams();
  const node = Number(nodeParam);
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") ?? "overview";
  const role = useRole();
  const vm = useLive(() => get<VMDetail>(`/nodes/${node}/vms/${uuid}`), `${node}/${uuid}`, (e) =>
    e.type === "node" ? true : e.type === "domain" ? e.uuid === uuid : e.task?.status !== "running",
  );
  const act = useAction();
  // libvirt sends no event when the guest agent comes up, so look again while it is not connected.
  const waitingForAgent = vm.data?.state === "running" && vm.data.agent !== "connected";
  useEffect(() => {
    if (!waitingForAgent) return;
    // Often for the first two minutes (a guest that is booting), rarely after that (a guest without the agent).
    let tries = 0;
    let h: ReturnType<typeof setTimeout>;
    const again = () => {
      h = setTimeout(() => {
        if (document.visibilityState !== "hidden") vm.reload();
        again();
      }, tries++ < 24 ? 5000 : 60000);
    };
    again();
    return () => clearTimeout(h);
  }, [waitingForAgent, vm.reload]);
  // After the tab changed, bring a card of it into view (instantly).
  const [jump, setJump] = useState<string | undefined>();
  useEffect(() => {
    if (!jump) return;
    document.getElementById(jump)?.scrollIntoView({ block: "start" });
    setJump(undefined);
  }, [jump, tab]);
  const base = `/nodes/${node}/vms/${uuid}`;
  const d = vm.data;

  if (!d) {
    return (
      <>
        <Link to="/" className="backlink"><ArrowLeft size={14} aria-hidden="true" /> Virtual machines</Link>
        <ErrorBox error={vm.error} />
      </>
    );
  }
  const goto = (t: string, card?: string) => {
    setParams(t === "overview" ? {} : { tab: t });
    setJump(card);
  };
  const tabs = [
    ["overview", "Overview"],
    ["hardware", "Hardware"],
    ["console", "Console"],
    ["snapshots", "Snapshots"],
    ["backups", "Backups"],
    ["xml", "XML"],
  ].filter(([id]) => id !== "console" || role !== "viewer"); // a console is control

  return (
    <>
      <Link to="/" className="backlink"><ArrowLeft size={14} aria-hidden="true" /> Virtual machines</Link>
      <div className="head">
        <h1>
          {d.name} <StateBadge state={d.state} />
          {d.template && <span className="badge s-template">Template</span>}
        </h1>
        <div className="actions">
          <VMActions d={d} base={base} goto={goto} onChanged={vm.reload} run={act.run} busy={act.busy} />
        </div>
      </div>
      <ErrorBox error={act.error ?? vm.error} />
      <TagEditor tags={d.tags ?? []} base={base} onChanged={vm.reload} />
      <div className="tabs">
        {tabs.map(([id, label]) => (
          <button key={id} className={tab === id ? "on" : ""} onClick={() => setParams(id === "overview" ? {} : { tab: id })}>
            {label}
          </button>
        ))}
      </div>
      {tab === "overview" && <Overview d={d} base={base} />}
      {tab === "hardware" && <Hardware d={d} base={base} onChanged={vm.reload} />}
      {tab === "console" && <ConsoleTab d={d} />}
      {tab === "snapshots" && <SnapshotsTab d={d} base={base} />}
      {tab === "backups" && <BackupsTab d={d} base={base} />}
      {tab === "xml" && <XMLEditor d={d} base={base} onSaved={vm.reload} />}
    </>
  );
}

function ConsoleTab({ d }: { d: VMDetail }) {
  const kinds = [...(d.hasVNC ? ["vnc"] : []), ...(d.hasSerial ? ["serial"] : [])];
  const [kind, setKind] = useState(kinds[0]);
  if (d.state === "shutoff") return <div className="empty">Start the VM to open its console.</div>;
  if (kinds.length === 0) return <div className="empty">This VM has neither a VNC display nor a serial console. Add one on the XML tab.</div>;
  return (
    <>
      {kinds.length > 1 && (
        <div className="segmented">
          <button className={kind === "vnc" ? "on" : ""} onClick={() => setKind("vnc")}>
            Display (VNC)
          </button>
          <button className={kind === "serial" ? "on" : ""} onClick={() => setKind("serial")}>
            Serial console
          </button>
        </div>
      )}
      {kind === "vnc" ? <VNCConsole node={d.node} uuid={d.uuid} name={d.name} /> : <SerialConsole node={d.node} uuid={d.uuid} />}
    </>
  );
}

function XMLEditor({ d, base, onSaved }: { d: VMDetail; base: string; onSaved: () => void }) {
  const [text, setText] = useState(d.xml);
  const [dirty, setDirty] = useState(false);
  const [saved, setSaved] = useState(false);
  const act = useAction();
  const viewer = useRole() === "viewer";
  // Follow outside changes until the user starts editing.
  useEffect(() => {
    if (!dirty) setText(d.xml);
  }, [d.xml, dirty]);
  const save = async () => {
    if (await act.run(() => put(`${base}/xml`, { xml: text }))) {
      setDirty(false);
      setSaved(true);
      onSaved();
    }
  };
  return (
    <div className="xml">
      <p className="muted">
        The full libvirt definition, the same document <code>virsh edit</code> shows. libvirt validates it on save.
        {d.state !== "shutoff" && " The VM is running: saved changes apply the next time it is started from off."}
      </p>
      <textarea
        spellCheck={false}
        value={text}
        readOnly={viewer}
        aria-label="Domain XML"
        onChange={(e) => {
          setText(e.target.value);
          setDirty(true);
          setSaved(false);
        }}
      />
      <ErrorBox error={act.error} />
      <footer>
        {saved && <span className="muted">Saved.</span>}
        <button
          disabled={!dirty}
          onClick={() => {
            setText(d.xml);
            setDirty(false);
            act.clear();
          }}
        >
          Revert
        </button>
        <button className="primary" disabled={!dirty || act.busy} onClick={save}>
          Save
        </button>
      </footer>
    </div>
  );
}
