// Add network: the first question (what the virtual machines connect to) and the private network made by libvirt.
// The other two answers lead into dialogs of VirtualSwitch.tsx: Add network on a switch, Add virtual switch.
import { FormEvent, useEffect, useState } from "react";
import { get, Network, post } from "../api";
import DialogAdvanced from "./DialogAdvanced";
import Select from "../Select";
import { ErrorBox, Field, Modal, useAction } from "../ui";

type Where = "switch" | "new" | "private";

/** The first step: an existing virtual switch, a new one on a network card, or a private network of libvirt. */
export function AddNetworkChooser(props: {
  switches: string[];
  /** Adding a virtual switch needs an administrator; `why` says why it cannot be done now. */
  newSwitch: { allowed: boolean; why?: string };
  onSwitch: (bridge: string) => void;
  onNewSwitch: () => void;
  onPrivate: () => void;
  onClose: () => void;
}) {
  const { switches, newSwitch } = props;
  const canNew = newSwitch.allowed && !newSwitch.why;
  const [where, setWhere] = useState<Where>(switches.length ? "switch" : canNew ? "new" : "private");
  const [bridge, setBridge] = useState(switches[0] ?? "");
  const go = (e: FormEvent) => {
    e.preventDefault();
    if (where === "switch") props.onSwitch(bridge);
    else if (where === "new") props.onNewSwitch();
    else props.onPrivate();
  };
  return (
    <Modal title="Add network" onClose={props.onClose}>
      <form onSubmit={go}>
        <Field label="What should the virtual machines connect to?" group>
          <label className="check">
            <input type="radio" name="addnet-where" checked={where === "switch"} disabled={switches.length === 0} onChange={() => setWhere("switch")} />
            An existing virtual switch
          </label>
          {where === "switch" && switches.length > 0 && <Select label="Virtual switch" mono value={bridge} onChange={setBridge} options={switches.map((s) => ({ value: s, label: s }))} />}
          {switches.length === 0 && <small>This node has no virtual switch yet.</small>}
          {newSwitch.allowed && (
            <>
              <label className="check">
                <input type="radio" name="addnet-where" checked={where === "new"} disabled={!canNew} onChange={() => setWhere("new")} />
                A new virtual switch on a network card
              </label>
              {newSwitch.why && <small>{newSwitch.why}</small>}
            </>
          )}
          <label className="check">
            <input type="radio" name="addnet-where" checked={where === "private"} onChange={() => setWhere("private")} />
            A private network made by libvirt
          </label>
          {where === "private" && <small>NAT, with its own addresses, or isolated. Under Advanced: routed, open, or your own definition.</small>}
        </Field>
        <footer>
          <button type="button" onClick={props.onClose}>
            Cancel
          </button>
          <button className="primary">Next</button>
        </footer>
      </form>
    </Modal>
  );
}

const modes: Record<string, string> = {
  nat: "NAT",
  isolated: "Isolated",
};
const otherModes: Record<string, string> = {
  "": "Same as Type",
  routed: "Routed",
  open: "Open",
  xml: "Custom (XML)",
};

const ip4 = (a: string) => a.split(".").reduce((n, o) => n * 256 + Number(o), 0);
/** Whether two "a.b.c.d/bits" subnets share addresses; anything else (no address) overlaps nothing. */
const overlaps = (a: string, b: string) => {
  const [ai, ab] = a.split("/"), [bi, bb] = b.split("/");
  if (!ab || !bb) return false;
  const size = 2 ** (32 - Math.min(Number(ab), Number(bb)));
  return Math.floor(ip4(ai) / size) === Math.floor(ip4(bi) / size);
};
/** The first private /24 from 10.77.1.0 upwards that no network of the node overlaps (the server picks the same). */
export function nextFreeAddress(existing: string[]): string {
  for (let n = 1; n <= 254; n++) {
    const subnet = `10.77.${n}.0/24`;
    if (!existing.some((e) => overlaps(e, subnet))) return `10.77.${n}.1/24`;
  }
  return "";
}

/** A NAT or isolated network libvirt makes for itself; Advanced has routed, open, custom XML, DHCP range, bridge name and DNS domain. */
export function PrivateNetworkDialog({ node, onClose, onDone }: { node: number; onClose: () => void; onDone: () => void }) {
  const base = `/nodes/${node}/networks`;
  const [type, setType] = useState("nat");
  const [other, setOther] = useState("");
  const mode = other || type;
  const [f, setF] = useState({ name: "", bridgeName: "", dhcp: true, dhcpStart: "", dhcpEnd: "", domain: "", xml: "" });
  const [typedAddress, setTypedAddress] = useState<string | undefined>();
  const set = (k: keyof typeof f) => (e: { target: { value: string } }) => setF({ ...f, [k]: e.target.value });
  const act = useAction();
  // What the node already has, for the next free address.
  const [existing, setExisting] = useState<Network[] | undefined>();
  useEffect(() => {
    let live = true;
    get<Network[]>(base).then((r) => live && setExisting(r), () => live && setExisting([]));
    return () => {
      live = false;
    };
  }, [base]);
  const free = existing ? nextFreeAddress(existing.map((n) => n.address ?? "")) : "";
  const address = typedAddress ?? free;
  const dhcp = mode === "isolated" ? f.dhcp : true; // the checkbox exists for Isolated only
  const rangeHint = (last: string) => (/^\d+\.\d+\.\d+\.\d+\/24$/.test(address) ? address.replace(/\.\d+\/\d+$/, "." + last) : "");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const body = mode === "xml" ? { xml: f.xml } : { name: f.name, mode, bridgeName: f.bridgeName, address, dhcp: dhcp && address !== "", dhcpStart: f.dhcpStart, dhcpEnd: f.dhcpEnd, domain: f.domain };
    if (await act.run(() => post(base, body))) {
      onDone();
      onClose();
    }
  };
  return (
    <Modal title="New private network" onClose={onClose}>
      <form onSubmit={submit}>
        <Field label="Type">
          <Select
            label="Type"
            value={type}
            onChange={(v) => {
              setType(v);
              setOther("");
            }}
            options={Object.entries(modes).map(([k, v]) => ({ value: k, label: v }))}
          />
        </Field>
        {mode === "xml" ? (
          <Field label="Network definition" hint="Checked against libvirt's schema.">
            <textarea spellCheck={false} rows={12} value={f.xml} onChange={set("xml")} required placeholder="Network definition (XML)" autoFocus />
          </Field>
        ) : (
          <>
            <Field label="Name">
              <input value={f.name} onChange={set("name")} required autoFocus />
            </Field>
            <Field label={mode === "isolated" ? "Address (optional)" : "Address of the node on this network"} hint="For example 10.77.1.1/24">
              <input value={address} onChange={(e) => setTypedAddress(e.target.value)} required={mode !== "isolated"} placeholder="10.77.1.1/24" />
            </Field>
          </>
        )}
        <DialogAdvanced>
          <Field label="Other network types">
            <Select label="Other network types" value={other} onChange={setOther} options={Object.entries(otherModes).map(([k, v]) => ({ value: k, label: v }))} />
          </Field>
          {mode !== "xml" && (
            <>
              {mode === "isolated" && (
                <label className="check">
                  <input type="checkbox" checked={f.dhcp} disabled={address === ""} onChange={(e) => setF({ ...f, dhcp: e.target.checked })} /> Hand out addresses with DHCP
                </label>
              )}
              {dhcp && address !== "" && (
                <div className="grid2">
                  <Field label="DHCP range start" hint="Defaults to the first free address.">
                    <input value={f.dhcpStart} onChange={set("dhcpStart")} placeholder={rangeHint("2")} />
                  </Field>
                  <Field label="DHCP range end">
                    <input value={f.dhcpEnd} onChange={set("dhcpEnd")} placeholder={rangeHint("254")} />
                  </Field>
                </div>
              )}
              <div className="grid2">
                <Field label="Bridge name (optional)" hint="Created by libvirt; automatic if empty.">
                  <input value={f.bridgeName} onChange={set("bridgeName")} placeholder="virbr1" maxLength={15} />
                </Field>
                <Field label="DNS domain (optional)">
                  <input value={f.domain} onChange={set("domain")} placeholder="lab.example" />
                </Field>
              </div>
            </>
          )}
        </DialogAdvanced>
        <ErrorBox error={act.error} />
        <footer>
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="primary" disabled={act.busy}>
            Create network
          </button>
        </footer>
      </form>
    </Modal>
  );
}
