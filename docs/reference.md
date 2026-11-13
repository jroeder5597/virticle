# Reference

## Commands

| Command | Does |
| --- | --- |
| `virticle [flags]` | Runs the controller. |
| `virticle version` | Prints version, commit and build date. |
| `virticle setup [--check] [--undo]` | Run once on the controller's machine after installing, as root (`sudo virticle setup`). Opens the web port in firewalld or ufw, makes sure the service runs and prints the address to open. `--check` changes nothing; `--undo` removes the firewall rule it added. Takes `-data`. See [install.md](install.md#package-debian-ubuntu-rhel-like). |
| `virticle node prepare\|unprepare` | Prepares a host as a node, or undoes it. See [install-node.md](install-node.md). |
| `virticle web show\|set\|reset` | Reads or changes the stored web access settings. `show` and `reset` take only `-data`. See [web-tls.md](web-tls.md). |

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data` | `/var/lib/virticle` if it exists and holds a database, else `data` | Directory for the database and the controller's SSH key. Also for `virticle web` and `virticle node`. |
| `-listen` | none | Address of the web UI and API. Given, it overrides what the **Web access** page stores and makes that page read-only. Leave it out to manage web access from the UI. |
| `-no-scheduler` | off | Do not run scheduled snapshots and backups from this controller. |
| `-alert-webhook` | empty | Deprecated. URL that receives a JSON POST when an alert is raised or cleared. Given, it is listed on the **Alerts** page as one more destination that cannot be edited. |

Without `-listen` and without stored web settings, a data directory with no
users serves HTTPS with a self-signed certificate on port 8443 of all
interfaces; one that already has users keeps plain HTTP on `127.0.0.1:8080`.

Deprecated, hidden from `-help`, and still working with a notice in the log:
`-tls-cert`, `-tls-key`, `-insecure-http`, `-trusted-proxy` (use the **Web
access** page or `virticle web set`), `-alert-webhook` (use **Alerts**), and `-alert-pool-warn` (85),
`-alert-pool-critical` (95), `-alert-memory` (90), `-alert-cert-days` (30),
whose defaults stay.

## Environment

| Variable | Meaning |
| --- | --- |
| `VIRTICLE_LISTEN` | Default for `-listen`. |
| `VIRTICLE_JUMP_PASSWORD` | Password for node jump hosts. |

The package reads these from `/etc/virticle/virticle.env`. Deprecated like
their flags: `VIRTICLE_TLS_CERT`, `VIRTICLE_TLS_KEY`, `VIRTICLE_INSECURE_HTTP`,
`VIRTICLE_TRUSTED_PROXY`, and `VIRTICLE_VIRTIO_WIN_PATTERN` (volume name
pattern that counts as the virtio-win ISO).

## Roles

People can also sign in through an identity provider or a directory, and local accounts can use two-step sign-in; see [sign-in.md](sign-in.md). `virticle user reset-2fa USER` and `virticle user reset-password USER` are the shell recovery commands.

| Role | Can |
| --- | --- |
| `viewer` | Read everything except the audit log. Cannot open a console. |
| `operator` | Viewer, plus VM, storage, network, snapshot, backup, migration and console actions. |
| `admin` | Operator, plus nodes, users, web access and the audit log. |

- A route that is not listed in `internal/api/access.go` requires `admin`.
- Failed sign-ins are rate limited per user and per address.

## API tokens

1. Create a token under **Account**. The secret is shown once.
2. Give it a role (yours or lower) and an optional expiry.
3. Send it as a bearer token:

       curl -H "Authorization: Bearer virticle_xxxxxxxx" https://vm.example.org/api/vms

       curl -X POST -H "Authorization: Bearer virticle_xxxxxxxx" -H 'Content-Type: application/json' \
            -d '{"action":"start"}' https://vm.example.org/api/nodes/1/vms/<uuid>/action

Token requests are written to the audit log. Account settings need a web
session, not a token.

## Alert webhook body

A destination of type **Webhook** receives a `POST` with `Content-Type: application/json` and
`User-Agent: virticle-alerts` for every alert that is raised (or changes severity) and every alert that clears:

```json
{"event":"raise","alert":{"id":"pool:1:data","kind":"pool-usage","severity":"critical","title":"Pool data is 97% full","message":"Storage pool data on kvm1 uses 970 GiB of 1000 GiB.","node":1,"link":"/storage","since":1700000000},"time":1700000100}
```

- `event`: `raise`, `clear`, or `test` (the **Send test message** button; its alert has `"id":"test"`).
- `alert.kind`: `node-down`, `pool-usage`, `host-memory`, `failed-job`, `vm-state`, `tls-expiry`,
  `scheduler-conflict`, `net-link`, `net-bond`, `net-drift`. `severity` is `warning` or `critical`. `node` is
  the node id and `link` the page in the web UI (both may be missing); `since` and `time` are Unix seconds.
- One alert per request, always; the webhook is never summarised. Any 2xx answer is success. Other answers are
  retried after 5 s, 30 s and 2 min, except 401, 403, 404, 410 and other 4xx (not 408, 429), which are not.
  On 429 the `Retry-After` header is honoured.

With a **Secret** set, the request has the header `X-Virticle-Signature: sha256=<hex>`, where `<hex>` is the
lowercase hex of HMAC-SHA256 over the exact request body with the secret as key. To verify, compute the same over
the raw bytes you received (before parsing) and compare in constant time:

```python
import hmac, hashlib
def valid(secret: bytes, body: bytes, header: str) -> bool:
    want = "sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(want, header)
```

The API for the Alerts page: `GET/POST /api/notify/destinations`, `PUT/DELETE /api/notify/destinations/{id}` and
`POST /api/notify/destinations/{id}/test` (admin). The address is accepted on create and update and never
returned: the list carries `host` and `tail` (the last four characters). `GET`/`PUT /api/alerts/settings` of
earlier versions no longer exist.

## Where state lives

- **libvirt on the nodes**: VMs, networks, pools, volumes, snapshots. Per-VM
  app data (template mark, tags, schedules, CPU model marker) sits in one
  `<metadata>` element of the domain XML, namespace
  `https://virticle.invalid/ns/1`.
- **The data directory** (`-data`):
  - `virticle.db`: users, password hashes, sessions, token hashes, node list,
    web access settings, alert destinations (addresses and secrets encrypted), task log, audit log.
  - `virticle.db.bak-<version>-<time>`: copy made before a schema upgrade
    (newest 3 kept).
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: node host keys, trusted on first use.
  - `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/`: certificates.
  - `instance-id`: stamped into scheduled snapshots.
  - `secret.key`: the one key that encrypts every stored secret (two-step sign-in seeds, identity provider and
    directory passwords, alert webhook addresses and signing secrets), created on first use, mode 0600. Keep it with
    the database; without it those secrets must be entered again. An older `settings.key` is renamed to it.
- **Memory only**: live stats (about 10 minutes), alert state, rate-limit
  counters.

## New VM defaults

- **CPU**: the newest model every online node can run (`custom`, exact match),
  so the VM can live-migrate anywhere in the cluster. `host-model` and
  `host-passthrough` can be chosen instead.
- **Clock** (Linux): `rtc` catch-up, `pit` delay, `hpet` off, `kvmclock` on.
  The guest clock is synced after resume, snapshot revert and live migration
  when the guest agent runs.
- **Serial console**: added to every new VM; the Console tab adds one to a VM that has none.
- **Firmware** (Linux): UEFI, with the UEFI variables in a qcow2 file so the VM can take internal snapshots. A node without UEFI firmware gets BIOS. Windows always gets UEFI.

## Migration network and limit

- Each node has a migration network: the address other nodes send migration data and file copies to. Default: the
  management address. `GET` and `PUT /api/nodes/{id}/migration-network` read and change it (`address`, and `limit`).
- Limit on a shared link, per node: `""` (default, 80% of the link speed when known, else none), `"none"` or
  `"custom:<Mbit/s>"`. It applies only while migration data shares the interface of the management address. The lower
  limit of the two nodes wins. `bandwidthMiB` in a migrate request replaces it.

## Schedules

- Stored in the VM's metadata and run by the controller. You enter and see times in your browser's time zone; they are stored in UTC.
- **Snapshots**: interval or daily, keep newest N. Shut-off VMs are skipped.
  Only snapshots the scheduler made are pruned.
- **Backups**: interval or daily, target node and pool, keep the newest N.
- A failed run is retried after 5 minutes and raises an alert.
- Run one controller per set of nodes. With several, start all but one with
  `-no-scheduler`.

## Backup format

A directory `<vm>-<UTC timestamp>.vmbackup` in a directory-backed pool:

- `manifest.json`: VM name and UUID, time, source node, disks, SHA-256 checksums (not for scheduled backups).
- `domain.xml`: the VM definition.
- `<target>.qcow2`: one flattened image per writable disk.
- `nvram.fd`: for UEFI guests; the manifest records its format (`nvramFormat`: `qcow2`, or `raw` for VMs made before qcow2 stores).

A running VM is backed up with libvirt's backup API and keeps running. The
directory is renamed into place last, so a backup is complete or absent. A
backup restores as a new VM or as the original, and exports to one
`.vmexport.tar`.

## Node access

A node is reached in one of two ways. Both run the same program as root: the node command
(`/usr/local/libexec/virticle-node`), which `virticle node prepare` installs. See [install-node.md](install-node.md).

| Access | How to set it up | What the controller does |
| --- | --- | --- |
| **Service account** (recommended) | `virticle node prepare` | Signs in as `virticle-node` (`virticle` on a node that 0.2.0 prepared) and runs the node command with `sudo -n`. The sudo rule allows that program and nothing else. |
| **Root** | `virticle node prepare --root` | Signs in as root and runs the node command directly. A Service account is safer. |

A node that has neither is "Not prepared": the features that need root are off. A node whose account has general
passwordless sudo but no node command is also not prepared.

## Host bridges

A node's Linux bridges can be created, changed and deleted from the controller (admin only, API
`/api/nodes/{node}/hostnet`; `GET .../hostnet/peers?bridge=&network=&vlan=` lists the other nodes that have a managed bridge of that name and no network of that name, for the UI's "create the same network" box). Only Linux bridges are handled; Open vSwitch is not. The change touches the network the
controller reaches the node over, so it is built to undo itself.

**What a change does.** The node command first saves the current settings (the configuration files it will touch, the
NetworkManager profile of the uplink, the running addresses and routes) in `/var/lib/virticle-node/net/`, readable by
root only. Then it arms a rollback timer (a transient systemd timer, independent of the SSH session), and only then
changes the network, in a transient systemd unit of its own, so it completes even when the SSH connection dies.
"Create a bridge with an uplink and move the addresses" gives the bridge the uplink's addresses, gateway, DNS and MTU, and
its MAC address (a DHCP lease follows the MAC address, so the node keeps its address; with systemd-networkd the bridge also takes over the card's DHCP client identifier, otherwise a server that keys its leases by client id hands out another address), and leaves the uplink as a plain
port. The node keeps the address the controller has stored, so the controller reconnects by itself.

**If the node becomes unreachable.** The controller waits for the node to come back, checks that libvirt answers, and
confirms the change; the node then keeps it. If it does not confirm within **90 seconds** (adjustable from 30 to 600), the
node restores its previous settings by itself and the task is recorded as failed ("The node did not come back in time. It
restores its previous network settings by itself; check again in a minute."). When the node is reachable again, its status
(`GET .../hostnet`, `recent`) says that the change was rolled back and why (`timer`, `apply failed`, `check failed`). A
change that cannot even apply (a command fails, the bridge does not get its address in 40 seconds) is rolled back at once,
without waiting for the timer. Only one change can be pending per node. Changes are logged to the journal with their id
(`journalctl -t virticle-node`); the last five are kept under `/var/lib/virticle-node/net/changes/`.

**From the node's console.** If neither the apply nor the timer got the node back, log in at the console as root and run

    /usr/local/libexec/virticle-node net-rollback

which restores the pending change (or give the id from `virticle-node net-status`). It is safe to run twice. A change that
was already confirmed is permanent: undo it by deleting the bridge.

**If the node restarts before the confirmation.** The rollback timer is a transient unit and is gone after a reboot. The
first change installs a small persistent unit, `virticle-net-boot.service` (it does nothing unless a change is pending),
which runs `virticle-node net-boot` at boot: the change gets its grace period again, counted from the boot (the same
number of seconds it had, 30 to 600), and is rolled back when nobody confirms it. A node therefore never stays on an
unconfirmed configuration for good. `virticle node unprepare` removes the unit with the node command.

**Several uplinks (a bond).** A switch can have two or more physical cards. They become the members of a Linux bond
(`vswbond0` for the switch `vsw0`, otherwise `b-` and the switch name, 15 characters at most) and the bond is the bridge's only
port. Two modes are offered: `failover` is `active-backup` with `miimon 100`; `lacp` is `802.3ad` with `lacp_rate fast`,
`xmit_hash_policy layer2+3` and `miimon 100`. `balance-alb` and `balance-tlb` are not offered: they rewrite MAC addresses of the
frames and the cards' ARP replies, which breaks a bridge that carries MAC addresses of virtual machines. Changing the cards
(`set-uplinks`): a card is added to the bond, a card is released (it gets its own configuration back), a one-card switch becomes
a bond, the mode changes (the members are released and the bond is set again in place; the bond is down for a moment). A bond
whose cards are removed down to one stays a bond with one member (no second change of the bridge port is needed); the switch
is only collapsed by deleting it. The card whose addresses moved to the bridge cannot be removed. A card that joins with an address of its own
loses it (the plan says so) and gets it back when it leaves. The bond and the bridge both get the MAC address of the card whose
configuration moved, and all members take the bond's MAC address (active-backup), so a DHCP server sees one MAC address and keeps the lease;
the VLAN ids go on the bond. Written: networkd, `05-virticle-BOND.netdev` (`Kind=bond`, `[Bond]` Mode, `MIIMonitorSec=100ms`,
`LACPTransmitRate`, `TransmitHashPolicy`), `05-virticle-BOND.network` (`Bridge=` and the `[BridgeVLAN]` entries) and one
`05-virticle-BRIDGE-CARD.network` per card (`Bond=`); NetworkManager, `virticle-bond-BOND` (`bond.options`, port of the bridge,
`bridge-port.vlans`) and one `virticle-port-CARD` per card (`controller BOND`, `port-type bond`). Rollback and delete restore
each member's own configuration. Status (shown in the topology): `/proc/net/bonding/BOND` gives each member's link, the active
member (failover) and, for LACP, the partner MAC address: all zeros means the physical switch did not answer. LACP needs
the cards to report their speed; the plan warns when they do not or when the speeds differ.

**The node's address on a VLAN.** On a managed, VLAN-aware switch the node gets an interface `SWITCH.ID` (a VLAN device on the
bridge, 15 characters at most) with DHCP or a static IPv4 address. The bridge becomes a member of the VLAN itself
(`bridge vlan add dev BRIDGE vid ID self`) and the port carries the id tagged: the id is added to the switch's allowed list when needed
(the plan says so). Without the default route option the address adds no default route (static: no gateway; DHCP: gateway,
DNS, NTP and search domains of the lease are ignored). Written: networkd, `05-virticle-SWITCH.ID.netdev` (`Kind=vlan`) and `.network`,
`VLAN=` and `[BridgeVLAN]` in the bridge's `.network`; NetworkManager, `virticle-vlan-SWITCH.ID` (`vlan.parent`, `vlan.id`) and `bridge.vlans` on
the bridge. The address interface is created without touching the bridge's own configuration. Removing an address that the controller uses is
refused, and so is changing the VLAN list or deleting the switch in a way that cuts such an address. Deleting the switch removes its
VLAN addresses. Moving the node's main (management) address onto a VLAN is not supported: the controller would have to reach the
node at a new address through a path that this change cannot check beforehand.

**MTU.** `set-mtu` (and the MTU field when a switch is added) sets one value on every device of the switch: each card,
the bond, the bridge and the VLAN interfaces (networkd `MTUBytes=` in the `.netdev` or `[Link]` of each file; NetworkManager `802-3-ethernet.mtu` of each profile), and
applies it at once with `ip link set` (cards last when it goes down, first when it goes up). When a card leaves the switch, or the
switch is deleted, the card gets its earlier MTU back. libvirt 11.3 and 11.10 refuse `<mtu>` in a network with `forward mode='bridge'`, so
networks on a host bridge cannot carry it; a guest needs the MTU in its own settings (not tried in the lab).

**Protocol 12.** The node command protocol is 12 (it was 11): the request and the saved state of a switch have new fields and the
controller shows bond and VLAN device state. A node prepared with protocol 11 shows "prepare the node again". (The old node command would
also refuse the new requests, because it rejects fields it does not know; the version check gives the clear message instead.)

**What it refuses.** Ports of a bond (or of a bridge) as uplinks, and cards that are already in another bond; an uplink that is already a port; wireless, virtual and
libvirt-managed (`virbr*`, `vnet*`) interfaces; changing an interface that carries the controller's connection without
moving its addresses; a second change while one is pending; deleting a bridge that VMs or libvirt networks use; changing
or deleting bridges that virticle did not create (they are only listed); VLANs when the node's libvirt is older than 11.0.
Only bridges this feature created are changed: it keeps a record of them (`/var/lib/virticle-node/net/managed/`).

**VLANs.** A VLAN-aware bridge has VLAN filtering on; its uplink allows the chosen VLAN ids (2-4094 by default, tagged),
and the bridge's own address stays untagged (VLAN 1). A libvirt network with `forward mode='bridge'` on such a bridge can
carry a VLAN tag (`vlan` in the network request, libvirt 11.0 or newer): the tag must be one the uplink allows.
The node's own addresses on a VLAN are described below.

**Configuration systems.** Detected at run time, never by distribution name:

| System | Written | State |
|---|---|---|
| NetworkManager | connections `virticle-BRIDGE` (bridge), `virticle-port-UPLINK` (and `virticle-bond-BOND`, `virticle-vlan-IF`); the uplink's own connection is kept and only set not to start by itself | tried on a real node (Rocky Linux 10, NetworkManager 1.56): create, move the management address, VLANs, rollback by timer and by hand, delete, reboots |
| netplan with systemd-networkd, or plain systemd-networkd | `05-virticle-*.netdev` and `.network` files in `/etc/systemd/network`; the files of netplan and the administrator are not edited. They sort before netplan's `10-netplan-*.network`, survive a reboot, `netplan apply` and cloud-init | netplan with networkd tried on a real node (Debian 13): the same tests as above; plain networkd without netplan runs the same code but was not tried |
| ifupdown (`/etc/network/interfaces`) | nothing: bridges are refused with a plain message | not supported |

## Known limits

- A Debian node and a Rocky Linux node cannot migrate VMs to each other
  (emulator path and machine type differ).
- Snapshots and TPM state are not part of a backup.
- Only file-based, non-shareable disks can be backed up.
- Internal snapshots pause the guest while memory is saved and block live
  migration.
- A VM keeps one kind of snapshot until all its snapshots are deleted (libvirt cannot delete a tree that mixes internal and external ones). On a node that confines QEMU with AppArmor (Debian) external snapshots cannot be reliably deleted, so a running VM there can only take snapshots that save the running state; a live snapshot without it is always external.
- A UEFI VM made before qcow2 variable stores keeps a raw one: its snapshots are external, and reverting a snapshot does not bring back the UEFI variables of that moment (internal snapshots of a qcow2 store do).
- No high availability, no shared-storage features, no replication.
- Not verified end to end: ACME issuance, the packages and the systemd service
  on a clean machine, arm64. See [ROADMAP.md](../ROADMAP.md).

## Upgrading from the working name virt-proj

Replace the binary and start it with the same `-data` directory.

- `virt-proj.db` is renamed to `virticle.db` at start.
- The old `VIRT_JUMP_PASSWORD` and `VIRT_VIRTIO_WIN_PATTERN` names no longer
  work; use the `VIRTICLE_` names.
- Per-VM metadata in the old namespace is read and moved on the next write.
