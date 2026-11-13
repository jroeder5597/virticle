# Networking backlog

Networking features that were considered and are **not built**. Each entry
says what it is, why someone would want it, and what is known about building
it. Written on 2026-11-13 for a later session.

For what exists today, see [user-guide.md](user-guide.md) (Networks, Virtual
switches, Network topology) and [reference.md](reference.md) (Host bridges).

## What is built

Virtual switches with VLANs, several uplinks, node addresses on a VLAN, MTU, the
migration network, network alerts, the drift warning and the speed limit per
network card are built. They are described in the
[user guide](user-guide.md#networks), not here.

## Node networking

| Feature | Why | Notes |
| --- | --- | --- |
| Change a node's own IP address, gateway or DNS from the UI | Today this means logging in to the node. | Same risk as moving the management address: needs the rollback timer and the controller reconnecting at the new address. The node record's address must change only after the node answered at the new one. |
| ifupdown nodes (Debian server installs) | Virtual switches are refused on them. | ifupdown and ifupdown2 behave differently. Needs a lab node with each. `net-info` already reports the stack as `ifupdown`, `supported:false`. |
| Plain systemd-networkd without netplan | Not tested. | Code path exists (`netplan-networkd` is the tested one). Mark `stackTested` after a lab run. |
| IPv6 | The address move and the migration network were tested with IPv4 only. | Check: router advertisements on the bridge, DHCPv6 identity, IPv6-only nodes, link-local addresses in certificates. |
| Open vSwitch | Port mirroring, tunnels, OpenFlow. | Shelved on purpose: VLANs work on Linux bridges from libvirt 11. Packages: in Debian, Ubuntu and Fedora; on RHEL 10 family only through the CentOS NFV SIG repository. libvirt's NAT networks can never use it. SELinux needs an extra policy package. |
| Restrict the migration ports to the other nodes | Ports 49152 to 49215 are open to any source. | Means updating every node's firewall when a node is added or removed. firewalld rich rules or a zone per cluster; ufw `allow from`. |
| Load balance without switch support (`balance-alb`, `balance-tlb`) | Spread traffic without LACP. | Known to misbehave as a bridge port because of MAC rewriting. Only offer after a real test. |
| Real LACP test | The lab network cannot speak LACP. | Needs two cards on a physical switch with LACP configured. |
| Management address on a VLAN, success path | The lab cannot prove it: the controller reaches lab nodes on the untagged network. | Needs a network where the controller can reach the VLAN. |

## VM networking (libvirt supports these; the UI does not offer them)

| Feature | Why | Notes |
| --- | --- | --- |
| MAC and IP spoofing protection | A VM cannot pretend to be another on a shared network. | libvirt nwfilter: `clean-traffic`, `no-mac-spoofing`, `no-ip-spoofing`. One checkbox per network card; needs the nwfilter driver running on the node. |
| VLAN trunk to a VM | A VM that is a router or firewall. | `<vlan trunk='yes'>` with several tags; works on Linux bridges from libvirt 11. |
| Port isolation | VMs on one network that must not talk to each other. | `<port isolated='yes'/>` on the interface (libvirt 6.1 and later). |
| A default speed limit per network | Limit every card on a network at once. | `<bandwidth>` on the libvirt network or a portgroup. The per-card limit is built; this is the network-level default. |
| SR-IOV and direct attachment (macvtap) | Near-native network speed. | Passthrough of virtual functions exists as PCI passthrough; a managed pool of virtual functions (`<forward mode='hostdev'>`) does not. Blocks live migration unless handled. |
| DHCP and DNS for bridged VLAN networks | libvirt serves these only on its own NAT and isolated networks. | Would mean running dnsmasq per VLAN on a node, or documenting how to use existing infrastructure. Probably better left to the user's network. |

## Cluster

| Feature | Why | Notes |
| --- | --- | --- |
| A switch definition shared by all nodes (distributed switch) | One place to define switches and networks; nodes kept in sync. | Today each node is configured by itself; "create the same network on other nodes" is a one-time convenience and the drift warning reports differences. A shared definition needs a stored desired state and a reconcile step per node, each change under the rollback timer. |
| Apply a virtual switch change to several nodes at once | Fewer clicks on larger setups. | Must apply one node at a time and stop at the first failure. |
| Traffic statistics per uplink and per network | Capacity planning. | Counters are in `/sys/class/net/*/statistics`; the stats code keeps 10 minutes in memory. |
| LLDP neighbour information | See which physical switch port a card is plugged into. | Needs `lldpd` or `systemd-networkd` LLDP on the node. |

## Controller

| Feature | Why | Notes |
| --- | --- | --- |
| Open the controller's web port from the web UI | Today `sudo virticle setup` must be run after a port change. | The controller runs without root. Possible only when the controller's machine is also a prepared node, through a narrow node command verb. Weigh against the extra root surface. |
| Per-node SSH port in the Add node dialog | Nodes whose SSH is not on port 22. | `host:port` works in the address field; the start panel's SSH check assumes 22. |
