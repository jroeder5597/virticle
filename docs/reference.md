# Reference

## Commands

| Command | Does |
| --- | --- |
| `virticle [flags]` | Runs the controller. |
| `virticle version` | Prints version, commit and build date. |
| `virticle setup [--check] [--undo]` | Run once on the controller's machine after installing, as root (`sudo virticle setup`). Opens the web port in firewalld or ufw, makes sure the service runs and prints the address to open. `--check` changes nothing; `--undo` removes the firewall rule it added. Takes `-data`. See [install.md](install.md#setup-open-the-firewall). |
| `virticle node prepare\|unprepare` | Prepares a host as a node, or undoes it. See [install-node.md](install-node.md). |
| `virticle backup [-o FILE\|DIR] [--encrypt] [--passphrase-file FILE]` | Writes the controller's own data (database, SSH key, certificates) to one file, while the controller runs. Takes `-data`. |
| `virticle restore FILE` | Puts such a file back; the controller must be stopped. Takes `-data` and `--passphrase-file`. See [controller-backup.md](controller-backup.md). |
| `virticle web show\|set\|reset` | Reads or changes the stored web access settings. `show` and `reset` take only `-data`. See [web-tls.md](web-tls.md). |
| `virticle user reset-2fa\|reset-password USER` | Recovery on the controller's machine: turn two-step sign-in off for a user, or set a new password for a local account. Takes `-data` and, for `reset-password`, `--password-stdin`. See [sign-in.md](sign-in.md#two-step-sign-in-authenticator-app). |

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data` | `/var/lib/virticle` if it exists and holds a database, else `data` | Directory for the database and the controller's SSH key. Also for `virticle web` and `virticle node`. |
| `-listen` | none | Address of the web UI and API. Given, it overrides what the **Web access** page stores and makes that page read-only. Leave it out to manage web access from the UI. |
| `-version` | | Print the version, commit and build date and exit (same as `virticle version`). |
| `-no-scheduler` | off | Do not run scheduled snapshots and backups from this controller. |
| `-alert-webhook` | empty | Deprecated. URL that receives a JSON POST when an alert is raised or cleared. Given, it is listed under **Settings > Alerts** as one more destination that cannot be edited. |

Without `-listen` and without stored web settings, a data directory with no
users serves HTTPS with a self-signed certificate on port 8443 of all
interfaces; one that already has users keeps plain HTTP on `127.0.0.1:8080`.

Deprecated, hidden from `-help`, and still working with a notice in the log:
`-tls-cert`, `-tls-key`, `-insecure-http`, `-trusted-proxy` (use the **Web
access** page or `virticle web set`), `-alert-webhook` (use **Settings > Alerts**), and `-alert-pool-warn` (85),
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
  `scheduler-conflict`, `net-link`, `net-bond`, `net-drift`, `storage-unavailable`, `rolling-update`, `replica-age`, `stale-copy`. `severity` is `warning` or `critical`. `node` is
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
  - `metrics.db`: the performance history of VMs and nodes, up to 7 days
    (see [metrics.md](metrics.md)). It is not part of the schema-upgrade copy
    and can be deleted at any time; only the history is lost.
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: node host keys, trusted on first use.
  - `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/`: certificates.
  - `instance-id`: stamped into scheduled snapshots.
  - `secret.key`: the one key that encrypts every stored secret (two-step sign-in seeds, identity provider and
    directory passwords, alert webhook addresses and signing secrets), created on first use, mode 0600. Keep it with
    the database; without it those secrets must be entered again. An older `settings.key` is renamed to it.
  - `migration-ca/`: the certificate authority of encrypted migration.
  - `backups/`: the daily controller backups (when switched on); `last-backup.json`: the time of the last backup; `.virticle.lock`: held while the controller runs.
  - `virticle backup` writes everything above that cannot be rebuilt into one file; see [controller-backup.md](controller-backup.md).
- **Memory only**: the newest statistics of every node and VM (the live
  charts show the last 10 minutes of them, and `/metrics` serves the latest),
  alert state, rate-limit counters.

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

## Replication

- Setting: the VM's metadata, child `replication` (standby node id and name, pool, `every-minutes`, replica directory, last result).
  The standby's domain has the child `replica`; after a failover the old VM has `stale-copy` and the new one `failed-over`.
  Older versions ignore these.
- Replica directory: a hidden work directory `.virticle-rp.XXXXXX` in a pool directory of the standby. Per disk a base
  `b<G>-<disk>.qcow2` (generation G, a full copy) and one overlay `p<S>-<disk>.qcow2` per point S, each holding only what changed since
  the point before; `p<S>.json` (written last, never changed) makes point S exist. The newest 2 points are kept; older ones are
  written into the base, oldest first. A full copy is a new generation; older generations go only after its first point is complete.
  Cutting any step short leaves a usable replica (the next run or the failover finishes the tidying).
- Checkpoints: target (description) `replica:<standby node id>`. Backups count and delete only their own (at most 4), replication
  keeps exactly one; snapshot revert, migration and disk move drop both.
- A run: backup job from the checkpoint (full when none), sent with the migration network peer transfer, checked by SHA-256, linked
  with a header-only rebase, manifest written, old points merged, then the old checkpoint deleted. A shut-off VM is copied only
  when its disk files changed since the last point (mtime and size). If the copy can be incremental it is started paused (the vCPUs
  never run; passthrough devices are refused for replication anyway), the same job runs, and QEMU is ended with a graceful destroy so
  it saves its bitmaps; the VM's `replication` child carries `paused-copy="yes"` meanwhile, Start and Resume are refused, and a controller
  restarted in between ends the paused VM in its next scheduler round. Any other case converts the disks whole and starts nothing.
- Failover order: a VM that takes over gets `failed-over gen="N+1"`, N being the generation of the source (the replica's `src-gen`,
  copied at every run from the source's own `failed-over`). The old VM is marked and powered off only when its own generation is lower
  (or it has none); equal generations do nothing. `at` (the controller's clock) is informational only. The decision is
  `decideOldCopy` (replica_reconcile.go), a pure function with a table test.
- Moving a replicated VM to its standby: the replica's domain is undefined (its directory stays), the source's `replication` child and
  its checkpoints are removed, the migration runs, and then the old directory is deleted and replication to the old node is set up
  (full copy); a failed migration puts the replica domain and the setting back. Deleting a node switches off replication to it on
  the other nodes.
- Damage: a `repl-commit` that was killed leaves refcounts behind the data; the verb repairs with `qemu-img check -r all`, commits again
  (which rewrites every block of the overlay, so the base equals the overlay over the old base whatever the first run wrote) and requires a
  clean `qemu-img check`; otherwise it fails with `replica-damaged`, the next run clears the replica directory and copies whole, and a
  failover refuses with the advice to delete the replica. Lab: 40 rounds of kill -9 at random points, repair, commit, `qemu-img compare`
  against the view taken before the commit: all identical.
- Bitmaps: a persistent `virticle-` or `backup-` bitmap with the `in-use` flag in an image that no QEMU has open (a killed QEMU) is removed
  with `qemu-img bitmap --remove` (node verb `qcow2-bitmap-rm`, which takes the image lock and so fails on a running VM), or with
  `block-dirty-bitmap-remove` for a bitmap a running QEMU calls inconsistent, before a disk resize and when a checkpoint could only be
  deleted metadata only. Their checkpoint records are deleted with them.
- A run is full when: no point, the replica is damaged, the checkpoint is gone (killed QEMU, migration, revert, restore), or the
  disk layout changed. One run per VM and per source node at a time; a backup and a run of one VM exclude each other.
- Scheduler: every 20 s the loop starts runs that are due (`-no-scheduler` runs none; `POST .../replication/run` does one). A
  failed run is retried after 5 minutes and recorded as a failed task, which raises the alert `failed-job` until a run succeeds.
- Alerts: `replica-age` (copy older than twice its interval plus a minute; clears when a run succeeds), `stale-copy`.
- API: `GET|PUT|DELETE .../vms/{uuid}/replication`, `POST .../replication/run`, `POST .../replication/cancel`, `GET|DELETE
  .../replica`, `POST .../replica/failover` (`{"mode":"planned"|"unplanned"}`, refused with 409 when the situation changed).
  `DELETE .../vms/{uuid}?replica=1` also removes the replica. Task kinds: `replicate`, `failover`, `replication stop`, `replica delete`.
- Refused: raw disks, shareable or non-file disks, passthrough devices, a standby with another VM of that name or UUID, a missing
  network or bridge on the standby, no pool with room.
- Failover: the newest point whose chain reads; its overlays are written into the base, the base moved to the pool as `<vm>.qcow2`
  (`<vm>-<disk>.qcow2` for more disks), UEFI variables placed, the definition of that point defined, the VM started. Can be repeated
  after any failure. The old VM is stale-marked by the controller when its node is online again (alert loop, once a minute).

## Backup format

A directory `<vm>-<UTC timestamp>.vmbackup` in a directory-backed pool:

- `manifest.json`: VM name and UUID, time, source node, disks, SHA-256 checksums (always for a backup of a running VM with qcow2 disks, since it can become part of a chain; a restore checks every file it reads).
- `domain.xml`: the VM definition.
- `<target>.qcow2`: one flattened image per writable disk. In an incremental backup it holds only the blocks that changed
  since the parent, and has no backing file.
- `nvram.fd`: for UEFI guests; the manifest records its format (`nvramFormat`: `qcow2`, or `raw` for VMs made before qcow2 stores).

A running VM is backed up with libvirt's backup API and keeps running. The
directory is renamed into place last, so a backup is complete or absent. A
backup restores as a new VM or as the original, and exports to one
`.vmexport.tar`.

### Incremental backups

- Each backup of a running VM with qcow2 disks leaves a libvirt checkpoint (a QEMU dirty bitmap per disk), named
  `virticle-<time>-<random>`, with the target (`node:pool`) as its description. The next backup asks libvirt for the blocks
  changed since the checkpoint of the newest backup that still has one.
- The manifest of an incremental backup is format 2 (an older Virticle refuses it instead of restoring part of a disk) and adds
  `kind`, `parent` (a backup in the same pool) and `checkpoint`. A full backup stays format 1, with `kind`, `checkpoint`,
  `fullReason` and the disks' `source` added; backups from before have none of these and count as full.
- A backup is full when: the VM is shut off (copied with qemu-img, no checkpoint); a disk is not qcow2; libvirt is older
  than 7.0; no earlier backup of the VM in the target has its checkpoint any more (restart after a killed QEMU, migration,
  restore, snapshot revert, disk move); the disks (target, size, and the base image at the bottom of the backing chain) differ from the parent's; a backup in the chain is
  missing or its files differ from the manifest; the chain already has 7 backups; or libvirt refuses the incremental
  backup (then the VM's checkpoints are dropped and a full backup is made).
- The checkpoints of the same target older than the new one are deleted after a backup; a VM keeps at most 4. A failed or
  cancelled backup deletes the checkpoint it made.
- Retention keeps the newest N and every backup they build on, and deletes newest first. A restore reads the chain with one
  `qemu-img convert` over a `json:` description of the images; nothing reads the VM's disks.
- Snapshot revert, migration (live or offline) and disk move drop the VM's checkpoints first. Taking or deleting a snapshot (internal,
  or external with its overlay merged back) and a resize keep them: libvirt carries the change tracking into an overlay and
  merges it back (tried on the lab: create, two stacked overlays, middle and active merge, with writes during the merge;
  every restore matched). Deleting a backup is refused while a backup of the same VM runs.
- Two limits of libvirt 11.3, both harmless to the data. An external snapshot taken while a checkpoint exists leaves that
  checkpoint's bitmap in the read-only base image, where libvirt cannot remove it; it stays there and QEMU keeps updating it.
  A libvirt restart during a backup leaves a temporary bitmap in the VM; the next backup removes it through the QEMU
  monitor (libvirt marks the VM as tainted in its log) and carries on.

## Node access

A node is reached in one of two ways. Both run the same program as root: the node command
(`/usr/local/libexec/virticle-node`), which `virticle node prepare` installs. See [install-node.md](install-node.md).

| Access | How to set it up | What the controller does |
| --- | --- | --- |
| **Service account** (recommended) | `virticle node prepare` | Signs in as `virticle-node` (`virticle` on a node that 0.2.0 prepared) and runs the node command with `sudo -n`. The sudo rule allows that program and nothing else. |
| **Root** | `virticle node prepare --root` | Signs in as root and runs the node command directly. A Service account is safer. |

A node that has neither is "Not prepared": the features that need root are off. A node whose account has general
passwordless sudo but no node command is also not prepared.

## Host bridges (virtual switches)

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

**Protocol 17.** The node command protocol is 17 (0.2.0 had 10). A node prepared with an older version shows "prepared
by an older version" and must be prepared again ([install.md](install.md#upgrading)); the old node command would also
refuse the new requests, because it rejects fields it does not know. Protocol 17 adds `repl-overlay`, `repl-rebase`, `repl-commit`, `repl-ls`, `mtime` (replication) and `qcow2-bitmap-rm`. Protocol 14 (incremental backups): `convert-start` takes the
incremental images after the format, and `backup-detach` is new.

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
| netplan with systemd-networkd, or plain systemd-networkd | `05-virticle-*.netdev` and `.network` files in `/etc/systemd/network`; the files of netplan and the administrator are not edited. They sort before netplan's `10-netplan-*.network`, survive a reboot, `netplan apply` and cloud-init | netplan with networkd tried on a real node (Debian 13): the same tests as above; plain networkd without netplan runs the same code but was not tried. To tell which file configures a card, the node evaluates the `[Match]` keys `Name`, `OriginalName`, `MACAddress`, `PermanentMACAddress`, `Kind`, `Type`, `Virtualization` and, for non-wireless cards, `WLANInterfaceType`, which covers the files in `/usr/lib/systemd/network` on Debian 13 and Ubuntu 24.04. A file with another key (for example `Driver` or `Path`) that sorts before the card's own file stops the review with a message that says to give the card a file of its own |
| ifupdown (`/etc/network/interfaces`) | nothing: bridges are refused with a plain message | not supported |

## Shared storage (NFS)

| Call | Meaning |
|---|---|
| `GET /api/storage/shared` | NFS pools grouped by name: nodes that have it, nodes missing it, nodes that have a pool of that name for another server or export, state, size. A server given by name on one node and by address on another is one share. |
| `POST /api/storage/shared` | `{name, server, path, nodes?}` adds a netfs pool on every online node (or the listed ones). Answers `complete`, `message`, `note` (a sentence about a mount that works but has a catch: NFS version 3), per-node results with the NFS `version`; repeatable. |
| `DELETE /api/storage/shared/{name}` | Unmounts and forgets it on all nodes; refused while a defined VM has a file below the mount, or a node is unreachable. |

Facts

- The pool is a libvirt `netfs` pool named NAME, mounted at `/mnt/NAME` on every node, set to autostart, with
  `<format type='auto'/>` (plain `mount -t nfs`; hard mount; highest version offered). Add creates and removes a
  small test volume to check that a disk made by root can be used by the VM user.
- A node that boots while the server is away has the pool stopped; the controller starts a stopped autostart NFS
  pool when the server answers on port 2049 and the node has been up for less than an hour.
- `node prepare` writes `/etc/systemd/system/virtqemud.service.d/10-virticle-storage.conf` (Wants and After
  `virtstoraged.service`) on nodes with separate libvirt daemons, so the NFS pools are mounted before VMs
  autostart. Without it a VM on NFS fails to autostart with "no such file".
- Pool state `unavailable` means the mount did not answer `stat -f` within 4 s, or is not an NFS mount. The
  answer comes from a probe (an SSH command with `timeout -s KILL`) that the controller repeats every few seconds
  for nodes that have NFS pools; pages use the last answer and never wait for it. Libvirt is not asked to read or
  refresh a pool that is unavailable. Statistics of VMs with a disk below an unavailable mount are not collected
  (QEMU does not answer the monitor while it waits for its disk), and the VM detail leaves out the disk figures.
- Storage calls to one node are limited to three at a time and the short ones to 6 seconds (the libvirt proxy
  daemon serves the controller's connection with five calls at once; the others stay free for VMs and networks).
  When three are stuck, further storage calls fail at once with "the storage service of this node is not
  answering" until one returns.
- Alert kind `storage-unavailable`, one per node and pool, critical, debounced like the network alerts (raised on
  the second check in a row, about two minutes; cleared on the second check that finds the share answering).
- Migration: a file is shared when source and target have netfs pools with the same export and mount path and
  the same server (by name or by address); it is not copied, not mirrored and not deleted from the source.
  Internal snapshots do not block a live migration when every disk is shared. Disk move, clone and delete
  refuse a VM with a disk on an unavailable share.
- Backups: a backup stored on a shared pool is listed once. Restoring into an NFS pool works (the node command
  falls back to link-and-unlink where the file system has no `RENAME_NOREPLACE`).
- Deleting a volume libvirt cannot remove (it removes files on NFS as the file's owner) falls back to the node
  command, as root.

## Rolling updates

Planning is `internal/api/rollplan.go`, the run `internal/api/rolling.go`. The state of a run is stored in the table
`rolling_runs` (schema 6) after every step; a controller that starts and finds a run in state `running` carries on with it
(also after its own machine restarted). A step in progress when the controller stopped runs again: every step looks at what is
true first (where the VM is, whether the update ran, whether the boot id changed).

| Request | Role | What it does |
| --- | --- | --- |
| `POST /api/rolling-update/plan` `{"nodes":[ids]}` | admin | The plan: per node `updates`, `reboot`, `moves`, `stuck` (VMs that cannot move, with reasons), `action` (`update`, `skip`, `current`); `refusals`; `needsChoice` |
| `POST /api/rolling-update` `{"nodes":[ids],"unmovable":"shutdown"\|"skip"}` | admin | Plans again, refuses with 409 and the first refusal, else starts one task `rolling-update` |
| `GET /api/rolling-update` | viewer | `{run, active}`: the newest run with its nodes, steps (`pending running ok skipped failed`) and, when stopped, `condition` |
| `POST /api/rolling-update/cancel` | admin | Stop after the current step |
| `POST /api/rolling-update/continue`, `/restore`, `/dismiss` | admin | Run the failed step again and go on; put things back; hide the alert |

Order: ids ascending, the controller's machine (an address that is this machine) last. Steps per node: maintenance, evacuate,
stop-vms, update, reboot, wait, verify, maintenance-off, start-vms, return. The restart is needed when the system says so
(`needs-restarting -r`, `/var/run/reboot-required`), a newer kernel is installed, or the update contained a kernel, firmware,
glibc, systemd, dbus, libvirt or QEMU package. Waits: restart 12 minutes, package tool 100 minutes, VM shutdown 3 minutes.
Alert kind `rolling-update`: critical after a failure, warning after a stop, until continued, put back or dismissed.

Node command, protocol **15**: `update-start`, `update-status` (JSON), `update-run` (internal, runs in the unit
`virticle-update`), `node-reboot` (restart in 5 seconds, unit `virticle-reboot`), `selfcheck [repair]` (lines `ok|fixed|problem`, tab, id, tab, text).
The update log is `/var/lib/virticle-update/update.log` on the node. dnf: `check-update --refresh`, then `upgrade -y`. apt:
`update`, then `apt-get -y --with-new-pkgs -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold upgrade` with
`DEBIAN_FRONTEND=noninteractive`; a source that fails to refresh fails the run.

## Known limits

- A Debian node and a Rocky Linux node cannot migrate VMs to each other
  (emulator path and machine type differ).
- Snapshots and TPM state are not part of a backup.
- Only file-based, non-shareable disks can be backed up.
- Internal snapshots pause the guest while memory is saved and block live
  migration (not when every disk is on shared storage).
- A VM keeps one kind of snapshot until all its snapshots are deleted (libvirt cannot delete a tree that mixes internal and external ones). On a node that confines QEMU with AppArmor (Debian) external snapshots cannot be reliably deleted, so a running VM there can only take snapshots that save the running state; a live snapshot without it is always external.
- A UEFI VM made before qcow2 variable stores keeps a raw one: its snapshots are external, and reverting a snapshot does not bring back the UEFI variables of that moment (internal snapshots of a qcow2 store do).
- No high availability, no shared-storage features, no replication.
- Not verified end to end: ACME issuance, the packages and the systemd service
  on a clean machine, arm64. See [ROADMAP.md](../ROADMAP.md).
