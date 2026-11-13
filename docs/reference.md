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
| `-alert-webhook` | empty | URL that receives a JSON POST when an alert is raised or cleared. Given, it wins over the **Alerts** page and makes its field read-only. |

Without `-listen` and without stored web settings, a data directory with no
users serves HTTPS with a self-signed certificate on port 8443 of all
interfaces; one that already has users keeps plain HTTP on `127.0.0.1:8080`.

Deprecated, hidden from `-help`, and still working with a notice in the log:
`-tls-cert`, `-tls-key`, `-insecure-http`, `-trusted-proxy` (use the **Web
access** page or `virticle web set`), and `-alert-pool-warn` (85),
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

## Where state lives

- **libvirt on the nodes**: VMs, networks, pools, volumes, snapshots. Per-VM
  app data (template mark, tags, schedules, CPU model marker) sits in one
  `<metadata>` element of the domain XML, namespace
  `https://virticle.invalid/ns/1`.
- **The data directory** (`-data`):
  - `virticle.db`: users, password hashes, sessions, token hashes, node list,
    web access settings, task log, audit log.
  - `virticle.db.bak-<version>-<time>`: copy made before a schema upgrade
    (newest 3 kept).
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: node host keys, trusted on first use.
  - `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/`: certificates.
  - `instance-id`: stamped into scheduled snapshots.
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

**What it refuses.** Bonds and ports of a bond as uplinks; an uplink that is already a port; wireless, virtual and
libvirt-managed (`virbr*`, `vnet*`) interfaces; changing an interface that carries the controller's connection without
moving its addresses; a second change while one is pending; deleting a bridge that VMs or libvirt networks use; changing
or deleting bridges that virticle did not create (they are only listed); VLANs when the node's libvirt is older than 11.0.
Only bridges this feature created are changed: it keeps a record of them (`/var/lib/virticle-node/net/managed/`).

**VLANs.** A VLAN-aware bridge has VLAN filtering on; its uplink allows the chosen VLAN ids (2-4094 by default, tagged),
and the bridge's own address stays untagged (VLAN 1). A libvirt network with `forward mode='bridge'` on such a bridge can
carry a VLAN tag (`vlan` in the network request, libvirt 11.0 or newer): the tag must be one the uplink allows.
Putting the host's own address on a VLAN is not supported.

**Configuration systems.** Detected at run time, never by distribution name:

| System | Written | State |
|---|---|---|
| NetworkManager | two connections, `virticle-BRIDGE` (bridge) and `virticle-port-UPLINK`; the uplink's own connection is kept and only set not to start by itself | tried on a real node (Rocky Linux 10, NetworkManager 1.56): create, move the management address, VLANs, rollback by timer and by hand, delete, reboots |
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
