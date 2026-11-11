// Picks a host bridge of a node: a searchable list of the node's bridge interfaces with state and addresses.
// The list is fetched once, when this component first shows (the dialog shows it only for "Host bridge").
import { useEffect, useState } from "react";
import { get, Network } from "./api";
import Combo from "./Combo";
import "./bridgeselect.css";

export interface HostNIC {
  name: string;
  state: "up" | "down" | "unknown";
  type: string;
  kind?: string;
  addresses: string[];
}

/** A Linux bridge, or an Open vSwitch bridge (type "other", kind "openvswitch"). */
export const isBridge = (n: HostNIC) => n.type === "bridge" || n.kind === "openvswitch";

/** The addresses worth showing: IPv4 and global IPv6, not link-local. */
export const shownAddresses = (n: HostNIC) => n.addresses.filter((a) => !a.toLowerCase().startsWith("fe80:"));

const LIBVIRT = "Managed by libvirt networks";

function Face({ n, net }: { n?: HostNIC; net?: string }) {
  if (!n) return <span className="muted bs-face">No bridge chosen</span>;
  const addr = shownAddresses(n);
  return (
    <span className="bs-face">
      <span className="bs-row">
        <span className="mono bs-name">{n.name}</span>
        <span className={"badge s-" + (n.state === "up" ? "running" : "shutoff")}>{n.state === "up" ? "Up" : "Down"}</span>
      </span>
      <span className="bs-row bs-sub">
        <span className="mono bs-addr">{addr.length ? addr.join("  ") : "no address"}</span>
        {n.kind === "openvswitch" && <span>Open vSwitch</span>}
        {net && <span>used by libvirt network {net}</span>}
      </span>
    </span>
  );
}

export default function BridgeSelect(props: { node: number; value: string; onChange: (name: string) => void; networks: Network[] }) {
  const [nics, setNics] = useState<HostNIC[]>();
  const [error, setError] = useState<string>();
  const [manual, setManual] = useState(false);
  useEffect(() => {
    let live = true;
    get<{ interfaces: HostNIC[] }>(`/nodes/${props.node}/interfaces`).then(
      (r) => live && setNics(r.interfaces),
      (e) => live && setError(String(e.message ?? e)),
    );
    return () => {
      live = false;
    };
  }, [props.node]);
  const netOf = (name: string) => props.networks.find((n) => n.bridge === name)?.name;
  const bridges = (nics ?? []).filter(isBridge);
  const items = bridges.map((n) => ({ key: n.name, nic: n, net: netOf(n.name), group: netOf(n.name) ? LIBVIRT : undefined, search: [shownAddresses(n).join(" "), netOf(n.name) ?? "", n.kind === "openvswitch" ? "open vswitch ovs" : ""].join(" ") }));

  if (manual || error)
    return (
      <div className="bs">
        <input aria-label="Bridge device" className="mono" value={props.value} placeholder="br0" onChange={(e) => props.onChange(e.target.value)} />
        <small className="muted">
          {error ? `The node's interfaces could not be read (${error}). ` : "The server checks that the node has a bridge of this name. "}
          {!error && (
            <button type="button" className="link" onClick={() => setManual(false)}>
              Choose from the list
            </button>
          )}
        </small>
      </div>
    );
  if (!nics) return <div className="muted bs">Loading</div>;
  return (
    <div className="bs">
      <Combo
        items={items}
        value={props.value}
        onChange={props.onChange}
        label="Bridge device"
        searchLabel="Search bridges by name or address"
        face={(i) => <Face n={i?.nic} net={i?.net} />}
        option={(i) => <Face n={i.nic} net={i.net} />}
        groupHint={{ [LIBVIRT]: "Usually better: pick the libvirt network itself." }}
        empty={
          <>
            <strong>No bridge interfaces on this node.</strong> Create one on the node (NetworkManager, systemd-networkd, /etc/network/interfaces).{" "}
            <button type="button" className="link" onClick={() => setManual(true)}>
              Enter a name manually
            </button>
          </>
        }
        footer={
          items.length > 0 && (
            <button type="button" className="link" onClick={() => setManual(true)}>
              Enter a name manually
            </button>
          )
        }
      />
    </div>
  );
}
