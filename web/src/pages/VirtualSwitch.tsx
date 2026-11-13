// Virtual switches: add, change and remove a bridge on a node, and a network on a switch. Admin only.
// The backend is internal/api/hostnet.go; the topology view (Topology.tsx) places the buttons.
import { FormEvent, ReactNode, useEffect, useRef, useState } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
import { ApiError, get, post, Task } from "../api";
import { StatusLine, StatusDot } from "../Checks";
import Select from "../Select";
import { useEvents } from "../live";
import { useLiveDebounced } from "../liveDebounced";
import { ErrorBox, Field, Modal } from "../ui";
import DialogAdvanced from "./DialogAdvanced";
import { netEvents } from "./dcShared";
import {
  addBlocked,
  addrIP,
  changeWords,
  DEFAULT_CONFIRM_SECONDS,
  failureWords,
  freeCards,
  HostBridge,
  HostNic,
  HostnetState,
  NetBody,
  NetPlan,
  rolledBackWords,
  shownAddrs,
  WAITING_PROGRESS,
} from "./vswitchApi";
import "./vswitch.css";

/** The node's network state for admins: refreshed on network events and every few seconds while a change is pending. */
export function useHostnet(node: number, enabled: boolean) {
  const r = useLiveDebounced(
    () => (enabled ? get<HostnetState>(`/nodes/${node}/hostnet`) : Promise.resolve(undefined)),
    `hostnet:${node}:${enabled}`,
    netEvents,
    1500,
  );
  const pending = !!r.data?.status.pending;
  const reload = r.reload;
  useEffect(() => {
    if (!pending) return;
    const t = setInterval(reload, 5000);
    return () => clearInterval(t);
  }, [pending, reload]);
  return r;
}

/** A small collapsible part of a dialog step (the "Details" of a plan). Not the dialog's Advanced section. */
function Fold({ title, children }: { title: string; children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const Chevron = open ? ChevronDown : ChevronRight;
  return (
    <div className="vsfold">
      <button type="button" className="disc-head" aria-expanded={open} onClick={() => setOpen(!open)}>
        <Chevron size={16} aria-hidden="true" />
        <span className="disc-title">{title}</span>
      </button>
      {open && <div className="vsfoldbody">{children}</div>}
    </div>
  );
}

// --- the line above the switches: a pending change, or the last one that was rolled back ---

const dismissKey = (node: number) => `virticle.vsw.dismissed.${node}`;
function dismissed(node: number): string {
  try {
    return localStorage.getItem(dismissKey(node)) ?? "";
  } catch {
    return "";
  }
}

export function ChangeBanner({ node, st, reload }: { node: number; st: HostnetState; reload: () => void }) {
  const [gone, setGone] = useState(() => dismissed(node));
  const [left, setLeft] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const pending = st.status.pending;
  const fetched = useRef(Date.now());
  useEffect(() => {
    fetched.current = Date.now();
    setLeft(pending?.secondsLeft ?? 0);
    if (!pending) return;
    const t = setInterval(() => setLeft(Math.max(0, (pending.secondsLeft ?? 0) - Math.round((Date.now() - fetched.current) / 1000))), 1000);
    return () => clearInterval(t);
  }, [pending?.id, pending?.secondsLeft]);
  if (pending) {
    const rollBack = async () => {
      setBusy(true);
      setError(undefined);
      try {
        await post(`/nodes/${node}/hostnet/rollback`, { id: pending.id });
        reload();
      } catch (e: any) {
        setError(String(e.message ?? e));
      } finally {
        setBusy(false);
      }
    };
    return (
      <div className="vsbanner" data-vsw="pending">
        <StatusLine status="warning">
          A network change is waiting to be confirmed ({changeWords(pending)}). {left > 0 ? `${left} seconds left, then the node restores its previous settings.` : "The node restores its previous settings by itself."}
        </StatusLine>
        <button onClick={rollBack} disabled={busy}>
          Roll back now
        </button>
        <ErrorBox error={error} />
      </div>
    );
  }
  const last = st.status.recent[0];
  if (last && last.state === "rolled-back" && last.id !== gone) {
    const dismiss = () => {
      try {
        localStorage.setItem(dismissKey(node), last.id);
      } catch {
        /* not remembered */
      }
      setGone(last.id);
    };
    return (
      <div className="vsbanner" data-vsw="rolled-back">
        <StatusLine status="error">{rolledBackWords(last)}</StatusLine>
        <button className="ghost" onClick={dismiss}>
          Dismiss
        </button>
      </div>
    );
  }
  return null;
}

// --- watching the task of a change ---

function useTask(id: number | undefined) {
  const [task, setTask] = useState<Task | undefined>();
  useEvents((e) => {
    if (id !== undefined && e.type === "task" && e.task?.id === id) setTask(e.task);
  });
  useEffect(() => {
    if (id === undefined) return;
    let live = true;
    const look = () =>
      get<Task[]>("/tasks").then(
        (l) => {
          const t = l.find((x) => x.id === id);
          if (live && t) setTask(t);
        },
        () => {},
      );
    look();
    const t = setInterval(look, 3000);
    return () => {
      live = false;
      clearInterval(t);
    };
  }, [id]);
  return task;
}

type Step = "todo" | "active" | "done" | "failed";
function Steps({ steps }: { steps: { label: string; state: Step; note?: string }[] }) {
  return (
    <ol className="vssteps" aria-label="Progress">
      {steps.map((s) => (
        <li key={s.label} className={"vsstep " + s.state} aria-current={s.state === "active" ? "step" : undefined}>
          <StatusDot status={s.state === "done" ? "ok" : s.state === "failed" ? "error" : s.state === "active" ? "warning" : "pending"} />
          <span>
            {s.label}
            {s.note && <span className="muted"> {s.note}</span>}
          </span>
        </li>
      ))}
    </ol>
  );
}

/** Progress of one applied change: three steps and the result. */
function Run({ task, timeout, started }: { task: Task | undefined; timeout: number; started: number }) {
  const [now, setNow] = useState(Date.now());
  const waitStart = useRef(0);
  const waiting = !!task && task.status === "running" && task.progress >= WAITING_PROGRESS;
  if (waiting && !waitStart.current) waitStart.current = Date.now();
  useEffect(() => {
    if (!waiting) return;
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, [waiting]);
  void started;
  const ok = task?.status === "ok";
  const failed = task?.status === "failed";
  const left = Math.max(0, timeout - Math.round((now - (waitStart.current || now)) / 1000));
  const reachedWaiting = waiting || ok || (failed && task!.progress >= WAITING_PROGRESS);
  const steps: { label: string; state: Step; note?: string }[] = [
    { label: "Applying on the node", state: reachedWaiting ? "done" : failed ? "failed" : "active" },
    {
      label: "Waiting for the node",
      state: ok ? "done" : failed && reachedWaiting ? "failed" : waiting ? "active" : "todo",
      note: waiting ? (left > 0 ? `${left} seconds left` : "checking the result") : undefined,
    },
    { label: "Confirmed", state: ok ? "done" : "todo" },
  ];
  return (
    <>
      <Steps steps={steps} />
      {ok && <StatusLine status="ok">The change is confirmed.</StatusLine>}
      {failed && <StatusLine status="error">{failureWords(task!.error)}</StatusLine>}
      {!ok && !failed && <p className="muted">You can close this window: the change goes on and the node shows it here when it is done.</p>}
    </>
  );
}

// --- add, edit uplinks, edit VLANs, add or remove this node's address on a VLAN, edit MTU, remove ---

export type SwitchMode = "add" | "uplinks" | "vlans" | "address" | "removeaddr" | "mtu" | "remove";
const isDone = (t?: Task) => !!t && t.status !== "running";
const cardLine = (n: { name: string; state: string; addresses: string[]; speedMbps?: number }) =>
  `${n.name} (${n.state === "up" ? "link up" : n.state === "down" ? "no link" : "link unknown"}${n.speedMbps ? ", " + (n.speedMbps >= 1000 ? n.speedMbps / 1000 + " Gbit/s" : n.speedMbps + " Mbit/s") : ""}${shownAddrs(n.addresses).length ? ", " + shownAddrs(n.addresses).join(", ") : ", no address"})`;

const MODE_OPTIONS = [
  { value: "failover", label: "Failover (works with any switch)" },
  { value: "lacp", label: "Load balance, needs LACP on the switch" },
];
const MODE_HINT: Record<string, string> = {
  failover: "One card carries the traffic. If its link fails, another card takes over.",
  lacp: "The traffic is spread over all cards. The ports of the physical switch must be set up for LACP.",
};

/** The list of cards with a checkbox each, and the Mode select once there is a bond to set up. */
function UplinkPicker(props: { cards: HostNic[]; chosen: Set<string>; onChoose: (s: Set<string>) => void; locked?: string; showMode: boolean; bondMode: string; onMode: (m: string) => void }) {
  const toggle = (name: string, on: boolean) => {
    const s = new Set(props.chosen);
    on ? s.add(name) : s.delete(name);
    props.onChoose(s);
  };
  return (
    <>
      <Field label="Uplinks" group>
        {props.cards.length === 0 && <p className="muted">This node has no free network card.</p>}
        {props.cards.map((c) => (
          <label className="check" key={c.name}>
            <input type="checkbox" checked={props.chosen.has(c.name)} disabled={props.locked === c.name} onChange={(e) => toggle(c.name, e.target.checked)} />
            {cardLine(c)}
          </label>
        ))}
        {props.locked && <small>{props.locked} carries this node's address on the switch and stays until the switch is removed.</small>}
      </Field>
      {props.showMode && (
        <Field label="Mode" hint={MODE_HINT[props.bondMode]}>
          <Select label="Mode" value={props.bondMode} onChange={props.onMode} options={MODE_OPTIONS} />
        </Field>
      )}
    </>
  );
}

export function SwitchDialog(props: { node: number; st: HostnetState; mode: SwitchMode; bridge?: HostBridge; uplink?: string; vlanId?: number; onClose: () => void; onChanged: () => void }) {
  const { node, st, mode, bridge } = props;
  const free = freeCards(st);
  const firstFree = free.find((c) => c.name !== st.managementInterface);
  const [name, setName] = useState(st.nextBridgeName || "vsw0");
  const [chosen, setChosen] = useState<Set<string>>(() => new Set(mode === "uplinks" ? bridge?.uplinks ?? (bridge?.uplink ? [bridge.uplink] : []) : props.uplink ? [props.uplink] : firstFree ? [firstFree.name] : []));
  const [bondMode, setBondMode] = useState<string>(bridge?.bondMode || "failover");
  const [vlanOn, setVlanOn] = useState(st.vlanCapable);
  const initialIds = bridge?.vlans && bridge.vlans !== "2-4094" ? bridge.vlans : "";
  const [ids, setIds] = useState(mode === "vlans" ? initialIds : "");
  const [mtu, setMtu] = useState(mode === "mtu" ? String(bridge?.mtu || 1500) : "");
  const [vlanId, setVlanId] = useState("");
  const [method, setMethod] = useState("dhcp");
  const [address, setAddress] = useState("");
  const [defRoute, setDefRoute] = useState(false);
  const [gateway, setGateway] = useState("");
  const [secs, setSecs] = useState(String(DEFAULT_CONFIRM_SECONDS));
  const [stage, setStage] = useState<"form" | "plan" | "run">("form");
  const [plan, setPlan] = useState<NetPlan | undefined>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const [taskId, setTaskId] = useState<number | undefined>();
  const [started, setStarted] = useState(0);
  const task = useTask(taskId);
  const changed = useRef(false);

  const timeout = Math.min(600, Math.max(30, Number(secs) || DEFAULT_CONFIRM_SECONDS));
  const picked = [...chosen];
  const cardOf = (n: string) => st.interfaces.find((i) => i.name === n);
  // The card whose configuration the switch takes over: the one the controller uses, else the first with an address.
  const mover = mode === "add" ? (picked.includes(st.managementInterface ?? "") ? cardOf(st.managementInterface!) : picked.map(cardOf).find((c) => c && shownAddrs(c.addresses).length > 0)) : undefined;
  const mgmt =
    mode === "add"
      ? !!st.managementInterface && chosen.has(st.managementInterface)
      : mode === "address" || mode === "removeaddr"
        ? false
        : bridge?.name === st.managementInterface;
  const bondExists = !!bridge?.bond;
  const showMode = picked.length >= 2 || (mode === "uplinks" && bondExists);
  const mtuNum = mtu.trim() === "" ? 0 : Number(mtu);
  const sw = bridge?.name ?? "";
  const vlanAddr = bridge?.vlanAddresses?.find((v) => v.id === props.vlanId);
  const base = { timeoutSeconds: timeout };
  const body: NetBody =
    mode === "add"
      ? {
          action: "create",
          bridge: name.trim(),
          uplinks: picked.length ? picked : undefined,
          bondMode: picked.length >= 2 ? bondMode : undefined,
          moveAddresses: picked.length > 0,
          vlanAware: vlanOn,
          vlans: vlanOn ? ids.trim() : undefined,
          mtu: mtuNum || undefined,
          ...base,
        }
      : mode === "uplinks"
        ? { action: "set-uplinks", bridge: sw, uplinks: picked, bondMode: showMode ? bondMode : undefined, ...base }
        : mode === "vlans"
          ? { action: "set-vlans", bridge: sw, vlanAware: true, vlans: ids.trim(), ...base }
          : mode === "mtu"
            ? { action: "set-mtu", bridge: sw, mtu: mtuNum, ...base }
            : mode === "address"
              ? {
                  action: "add-vlan-address",
                  bridge: sw,
                  vlanId: Number(vlanId) || 0,
                  addressMethod: method as "dhcp" | "static",
                  address: method === "static" ? address.trim() : undefined,
                  defaultRoute: defRoute,
                  gateway: defRoute && method === "static" ? gateway.trim() : undefined,
                  ...base,
                }
              : mode === "removeaddr"
                ? { action: "remove-vlan-address", bridge: sw, vlanId: props.vlanId, ...base }
                : { action: "delete", bridge: sw, ...base };
  const direct = mode === "remove" || mode === "removeaddr";

  useEffect(() => {
    if (isDone(task) && !changed.current) {
      changed.current = true;
      props.onChanged();
    }
  });
  const close = () => {
    if (stage === "run") props.onChanged();
    props.onClose();
  };

  const review = async (e: FormEvent) => {
    e.preventDefault();
    if (direct) return apply();
    setBusy(true);
    setError(undefined);
    try {
      setPlan(await post<NetPlan>(`/nodes/${node}/hostnet/plan`, body));
      setStage("plan");
    } catch (er: any) {
      setError(String(er.message ?? er));
    } finally {
      setBusy(false);
    }
  };
  const apply = async () => {
    setBusy(true);
    setError(undefined);
    try {
      const r = await post<{ task: number }>(`/nodes/${node}/hostnet/apply?async=1`, body);
      setStarted(Date.now());
      setTaskId(r.task);
      setStage("run");
    } catch (er: any) {
      setError(String(er.message ?? er));
      if (er instanceof ApiError && stage === "plan") setStage("form");
    } finally {
      setBusy(false);
    }
  };

  const title = {
    add: "Add virtual switch",
    uplinks: `Edit uplinks of ${sw}`,
    vlans: `Edit VLANs of ${sw}`,
    address: `Add this node's address on ${sw}`,
    removeaddr: `Remove this node's address on VLAN ${props.vlanId} of ${sw}`,
    mtu: `Edit MTU of ${sw}`,
    remove: `Remove virtual switch ${sw}`,
  }[mode];
  const running = stage === "run" && !isDone(task);
  const lockedCard = mode === "uplinks" && bridge?.movedAddresses ? bridge.uplink : undefined;
  const pickerCards = mode === "uplinks" ? [...(bridge?.uplinks ?? []).map(cardOf).filter((c): c is HostNic => !!c), ...free] : free;

  return (
    <Modal title={title} onClose={close}>
      {stage === "form" && (
        <form onSubmit={review}>
          {mode === "add" && (
            <>
              <Field label="Name">
                <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={15} pattern="[A-Za-z0-9_\-]+" autoFocus />
              </Field>
              <UplinkPicker cards={free} chosen={chosen} onChoose={setChosen} showMode={showMode} bondMode={bondMode} onMode={setBondMode} />
              {free.length === 0 && <p className="muted">The new switch can only connect virtual machines on this node.</p>}
              {st.vlanCapable ? (
                <label className="check">
                  <input type="checkbox" checked={vlanOn} onChange={(e) => setVlanOn(e.target.checked)} />
                  VLANs
                </label>
              ) : (
                <p className="muted">VLANs need libvirt 11 or later on this node.</p>
              )}
              {mover && (
                <StatusLine status="info">
                  This node's address {shownAddrs(mover.addresses).map(addrIP).join(", ")} moves to the virtual switch. The node stays reachable at the same address.
                </StatusLine>
              )}
              {mgmt && (
                <StatusLine status="warning">
                  This is the card the controller uses to reach the node. If the node cannot be reached afterwards, it restores its previous settings by itself within {timeout} seconds.
                </StatusLine>
              )}
            </>
          )}
          {mode === "uplinks" && (
            <>
              <UplinkPicker cards={pickerCards} chosen={chosen} onChoose={setChosen} locked={lockedCard} showMode={showMode} bondMode={bondMode} onMode={setBondMode} />
              {mgmt && (
                <StatusLine status="warning">
                  This switch carries the address the controller uses to reach the node. If the node cannot be reached afterwards, it restores its previous settings by itself within {timeout} seconds.
                </StatusLine>
              )}
            </>
          )}
          {mode === "vlans" && (
            <>
              <Field label="Allowed VLAN ids" hint="Leave empty to allow all. For example 10,20,30-40.">
                <input value={ids} onChange={(e) => setIds(e.target.value)} placeholder="All" autoFocus />
              </Field>
              {bridge!.networks.length > 0 && <p className="muted">Networks on this switch keep their VLAN: {bridge!.networks.join(", ")}.</p>}
            </>
          )}
          {mode === "mtu" && (
            <>
              <Field label="MTU" hint="Between 1280 and 9000. The same value is set on the cards, the bond, the switch and the VLAN interfaces.">
                <input type="number" min={1280} max={9000} value={mtu} onChange={(e) => setMtu(e.target.value)} required autoFocus />
              </Field>
              <p className="muted">The ports of the physical switch and the other nodes must use the same MTU. Virtual machines need it in their own network settings too.</p>
              {mgmt && (
                <StatusLine status="warning">
                  This switch carries the address the controller uses to reach the node. If the node cannot be reached afterwards, it restores its previous settings by itself within {timeout} seconds.
                </StatusLine>
              )}
            </>
          )}
          {mode === "address" && (
            <>
              <Field label="VLAN id" hint={`Between 2 and 4094.${bridge?.vlans && bridge.vlans !== "2-4094" ? ` Allowed on this switch now: ${bridge.vlans}. A new id is added to the list.` : ""}`}>
                <input type="number" min={2} max={4094} value={vlanId} onChange={(e) => setVlanId(e.target.value)} required autoFocus />
              </Field>
              <Field label="Address">
                <Select label="Address" value={method} onChange={setMethod} options={[{ value: "dhcp", label: "From DHCP" }, { value: "static", label: "Enter an address" }]} />
              </Field>
              {method === "static" && (
                <Field label="IP address with prefix" hint="For example 10.30.0.5/24.">
                  <input value={address} onChange={(e) => setAddress(e.target.value)} required placeholder="10.30.0.5/24" />
                </Field>
              )}
            </>
          )}
          {mode === "removeaddr" && (
            <p>
              Remove this node's address{vlanAddr?.address ? ` ${vlanAddr.address}` : ""} on VLAN {props.vlanId} of <strong>{sw}</strong>? The VLAN stays allowed on the switch.
            </p>
          )}
          {mode === "remove" && (
            <>
              <p>
                Remove the virtual switch <strong>{sw}</strong> from this node?
                {bridge!.movedAddresses && bridge!.uplink ? ` This node's address returns to ${bridge!.uplink}.` : bridge!.uplink ? ` ${(bridge!.uplinks ?? [bridge!.uplink]).join(", ")} become plain network cards again.` : ""}
              </p>
              {mgmt && (
                <StatusLine status="warning">
                  This is the connection the controller uses to reach the node. If the node cannot be reached afterwards, it restores its previous settings by itself within {timeout} seconds.
                </StatusLine>
              )}
            </>
          )}
          {!direct && (
            <DialogAdvanced>
              {mode === "add" && vlanOn && (
                <Field label="Allowed VLAN ids" hint="Leave empty to allow all. For example 10,20,30-40.">
                  <input value={ids} onChange={(e) => setIds(e.target.value)} placeholder="All" />
                </Field>
              )}
              {mode === "add" && (
                <Field label="MTU" hint="Leave empty to keep the cards as they are. Between 1280 and 9000.">
                  <input type="number" min={1280} max={9000} value={mtu} onChange={(e) => setMtu(e.target.value)} placeholder="Unchanged" />
                </Field>
              )}
              {mode === "address" && (
                <>
                  <label className="check">
                    <input type="checkbox" checked={defRoute} onChange={(e) => setDefRoute(e.target.checked)} />
                    Use this VLAN for the default route of this node
                  </label>
                  {defRoute && method === "static" && (
                    <Field label="Gateway">
                      <input value={gateway} onChange={(e) => setGateway(e.target.value)} required placeholder="10.30.0.1" />
                    </Field>
                  )}
                </>
              )}
              <Field label="Confirm time (seconds)" hint="How long the node waits for the controller to confirm before it restores its settings.">
                <input type="number" min={30} max={600} value={secs} onChange={(e) => setSecs(e.target.value)} />
              </Field>
            </DialogAdvanced>
          )}
          <ErrorBox error={error} />
          <footer>
            <button type="button" onClick={close}>
              Cancel
            </button>
            {direct ? (
              <button className="danger" disabled={busy}>
                Remove
              </button>
            ) : (
              <button className="primary" disabled={busy || (mode === "uplinks" && picked.length === 0)}>
                Review
              </button>
            )}
          </footer>
        </form>
      )}
      {stage === "plan" && plan && (
        <div>
          <p>This will happen on the node:</p>
          <ul className="vsplan">
            {plan.summary.map((l) => (
              <li key={l}>{l}</li>
            ))}
          </ul>
          {(plan.warnings ?? []).map((w) => (
            <StatusLine key={w} status="warning">
              {w}
            </StatusLine>
          ))}
          {plan.managementAffected && (
            <StatusLine status="warning">
              This is the connection the controller uses to reach the node. If the node cannot be reached afterwards, it restores its previous settings by itself within {plan.timeoutSeconds} seconds.
            </StatusLine>
          )}
          <Fold title="Details">
            {(plan.files ?? []).map((f) => (
              <div key={f.path}>
                <div className="mono">
                  {f.action === "remove" ? "Remove" : "Write"} {f.path}
                </div>
                {f.content && <pre className="vspre">{f.content}</pre>}
              </div>
            ))}
            {(plan.commands ?? []).length > 0 && (
              <>
                <div className="muted">Commands, in order</div>
                <pre className="vspre">{(plan.commands ?? []).join("\n")}</pre>
              </>
            )}
          </Fold>
          <ErrorBox error={error} />
          <footer>
            <button type="button" onClick={() => setStage("form")}>
              Back
            </button>
            <button className="primary" onClick={apply} disabled={busy}>
              Apply
            </button>
          </footer>
        </div>
      )}
      {stage === "run" && (
        <div>
          <Run task={task} timeout={plan?.timeoutSeconds ?? timeout} started={started} />
          <footer>
            <button className={running ? "" : "primary"} onClick={close}>
              Close
            </button>
          </footer>
        </div>
      )}
    </Modal>
  );
}

// --- a network on a switch ---

/** Ids of the VLANs that networks on a switch already use, from its network list. */
export function AddNetworkDialog(props: {
  node: number;
  bridge: string;
  info?: HostBridge;
  vlanCapable: boolean;
  used: { network: string; vlan?: number }[];
  onClose: () => void;
  onDone: () => void;
}) {
  const { node, bridge, info } = props;
  const withVlan = props.vlanCapable && !!info?.vlanFiltering;
  const [name, setName] = useState("");
  const [vlan, setVlan] = useState("");
  const [peers, setPeers] = useState<{ node: number; name: string }[]>([]);
  const [same, setSame] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const [created, setCreated] = useState(false);

  const tag = vlan.trim() === "" ? 0 : Number(vlan);
  const ids = [...new Set(props.used.map((u) => u.vlan).filter((v): v is number => !!v))].sort((a, b) => a - b);

  // Other nodes with a virtual switch of the same name that could get the same network.
  useEffect(() => {
    if (!name.trim() || !info?.managed) {
      setPeers([]);
      return;
    }
    let live = true;
    const t = setTimeout(() => {
      get<{ nodes: { node: number; name: string }[] }>(`/nodes/${node}/hostnet/peers?bridge=${encodeURIComponent(bridge)}&network=${encodeURIComponent(name.trim())}&vlan=${tag || 0}`).then(
        (r) => live && setPeers(r.nodes),
        () => live && setPeers([]),
      );
    }, 400);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [node, bridge, name, tag, info?.managed]);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    const body = { name: name.trim(), mode: "bridge", bridgeName: bridge, ovs: false, vlan: tag };
    try {
      if (!created) await post(`/nodes/${node}/networks`, body);
      setCreated(true);
    } catch (er: any) {
      setError(String(er.message ?? er));
      setBusy(false);
      return;
    }
    const failed: string[] = [];
    if (same) {
      for (const p of peers) {
        try {
          await post(`/nodes/${p.node}/networks`, body);
        } catch (er: any) {
          failed.push(`${p.name}: ${String(er.message ?? er)}`);
        }
      }
    }
    setBusy(false);
    props.onDone();
    if (failed.length) setError(`The network ${body.name} was created, but not on every node. ${failed.join(" ")}`);
    else props.onClose();
  };

  return (
    <Modal title={`Add network on ${bridge}`} onClose={props.onClose}>
      <form onSubmit={submit}>
        <Field label="Name">
          <input value={name} onChange={(e) => setName(e.target.value)} required maxLength={40} autoFocus disabled={created} />
        </Field>
        {withVlan && (
          <Field
            label="VLAN"
            hint={
              (info?.vlans && info.vlans !== "2-4094" ? `Allowed on this switch: ${info.vlans}. ` : "Leave empty for no VLAN. ") +
              (ids.length ? `In use on this switch: ${ids.join(", ")}.` : "No VLAN is in use on this switch yet.")
            }
          >
            <input type="number" min={1} max={4094} value={vlan} onChange={(e) => setVlan(e.target.value)} placeholder="None" disabled={created} />
          </Field>
        )}
        {peers.length > 0 && !created && (
          <label className="check">
            <input type="checkbox" checked={same} onChange={(e) => setSame(e.target.checked)} />
            Create the same network on {peers.map((p) => p.name).join(", ")}
          </label>
        )}
        <ErrorBox error={error} />
        <footer>
          <button type="button" onClick={props.onClose}>
            {created ? "Close" : "Cancel"}
          </button>
          {!created && (
            <button className="primary" disabled={busy || !name.trim()}>
              Add network
            </button>
          )}
        </footer>
      </form>
    </Modal>
  );
}

export { addBlocked };
