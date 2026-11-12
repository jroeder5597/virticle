# Objectives

A web front end for KVM/QEMU/libvirt that stays fully compatible with `virsh`.
One Go binary, no service on the nodes: everything goes over SSH to each node's
libvirt socket, and the few things that need root run through one small program,
the node command (`virticle-node`), which the controller starts on demand.
Libvirt is the source of truth; the app keeps only users, the node list and task history.

Status: `[x]` done and verified against the lab, `[~]` in progress, `[ ]` open.

## Foundation
- [x] Spike: libvirt RPC over SSH, events, upload, live migration with disk copy, live snapshot, snapshot-preserving migration (`cmd/spike`)
- [x] Server binary with embedded web UI, SQLite state, local login
- [x] Node add/remove, persistent connections with reconnect (reconnect after a node outage not yet tested)
- [x] Live inventory driven by libvirt events: VMs, networks and pools (network and pool events are read from `virsh net-event`/`pool-event` over SSH)
- [x] Task log: every action with progress, result and libvirt error text
- [~] Raw XML editor with schema validation (VMs, networks, creating pools; volumes open)

## VM basics
- [x] Lifecycle: start, shutdown, reboot, force off, reset, pause, resume, delete
- [x] Autostart on host boot; graceful shutdown with timeout before force off
- [x] VNC console in the browser (noVNC, tunnelled over the node's SSH connection)
- [x] Serial/text console in the browser (xterm.js)
- [x] Edit CPU, memory, disks, network interfaces through forms (Hardware tab; says whether a change applied live or needs a restart)
- [~] Hot-plug disks and NICs, vCPU and memory changes, online disk resize (done; hot-unplug depends on the guest). USB devices open
- [x] Firmware options at creation: UEFI, Secure Boot, virtual TPM, Windows defaults; add/remove TPM on a shut-off VM (BIOS↔UEFI switch on an existing VM not offered)
- [x] Live stats: CPU, memory, disk, network per VM and per host (10 minutes kept in memory, no database)
- [x] Guest info on the VM card: IPs, OS, hostname, filesystems (via the agent, with DHCP leases as fallback for IPs)
- [~] Guest agent prompt: adds the channel, lists install commands, detects Windows and attaches or downloads the virtio-win ISO (Windows path untested on a real guest)

## Storage
- [~] Pools: create, start, stop, remove (dir tested; NFS, LVM, iSCSI forms and raw XML written but untested)
- [x] Volumes: create, resize, clone, upload, download from URL (the node fetches it with curl), delete
- [x] CD-ROM media: insert, swap, eject (live); boot order (config, next start)
- [~] Import existing disks: qcow2, raw, VMDK and single-VM OVA tested; VDI, VHDX untested

## Provisioning
- [~] Create VM from ISO, disk image or empty disk (form works; ISO path not yet tested with a real installer)
- [x] Create VM from a cloud image with cloud-init (hostname, user, password, SSH keys, packages, raw user-data); seed ISO built in pure Go
- [x] Templates: mark a VM as a template; full and linked clones (same node)
- [x] Inject SSH keys and user/password when cloning a template (cloud-init seed)
- [x] Sysprep on clone, Linux: `virt-sysprep` where the node has it, else "seal template" through the guest agent
- [~] Sysprep on clone, Windows: seal via `sysprep /generalize` through the agent and unattend ISO are written but UNTESTED (no Windows guest in the lab)

## Snapshots and backup
- [~] Snapshots with memory, tree view, revert, delete (done). Default type chosen per node (see note). Open: quiesce untested with a real agent, UEFI guests untested
- [x] Scheduled snapshots with retention (schedule stored in domain metadata, stored in UTC and shown in the browser time zone)
- [~] Backup and restore to another pool or node (live, no snapshot left behind); export/import a VM as one tar file. Scheduled backups with retention (one unexplained stall seen in testing, not reproduced). Open: TPM state and snapshots are not part of a backup

## Networking
- [~] Virtual network editor: NAT, isolated, routed, open, bridged, DHCP reservations (done); VLAN tags only for Open vSwitch bridges, untested on real OVS

## Multi-node
- [x] Node command and two ways to reach a node: Service account (sudo rule for the node command only) or Root. `virticle node prepare` installs it, upgrades it when the controller reports an older version, and `unprepare` removes it. Verified on the lab in both access modes on Debian 13 nodes and on Rocky 10 nodes (SELinux enforcing): the full browser suite passes on each pair. Found on Rocky: the serial console sometimes closed right after connecting during boot, a bug in the libvirt client library with libvirt 11.10, fixed in `third_party/go-libvirt` (3 of 10 runs failed before, 0 of 12 after); QEMU there takes the CPU flag pdcm away from a running VM, so the cluster baseline now leaves it out. Not run: arm64 nodes; a real OVA import and real virt-sysprep through the node command were covered only by the browser suite where it covers them
- [x] Direct node-to-node file transfer with a restricted node key (migration with local disks, restores on another node) and live migration coordinated by the controller, for any pair of Service account and Root nodes. Verified on the lab (Debian 13 nodes): a mixed pair (one Service account node, one Root node) passed offline migration both ways with identical checksums, live migration with disk copy, and backup with checksum and restore. During a live migration of a VM with a 1 GB disk and 512 MB memory the controller machine's total network traffic was under 0.5 MB
- [x] Live migration with pre-flight checks (CPU, networks, free space, passthrough, name clashes, node-to-node path); UEFI and TPM guests verified before the node command replaced the shell script; the node command path was re-run on the lab (see above), UEFI and TPM guests were not re-tested
- [x] Migration with disk copy; snapshot-preserving migration (external snapshots); source disk cleanup; cancel
- [~] Offline migration (done, carries internal snapshots too); UEFI/NVRAM and TPM paths untested
- [x] Encrypted migration streams (QEMU TLS for memory and disk mirror; controller-managed CA, per-node setup/removal, pre-flight warning when unencrypted). Certificate install verified on Debian and Rocky; an encrypted migration between two RHEL-family nodes is untested
- [x] Evacuate a node (planner, per-VM tasks, cancel, maintenance flag); show pending host updates (read-only)

## Hardware
- [x] Passthrough picker: PCI and USB with IOMMU groups, in-use and safety checks, migration warning. Verified on real hardware with a whole GPU (Quadro P1000 and its audio function: attach, guest sees it, host driver takes it back). USB passthrough, hot-plug of a PCI device and SR-IOV/vGPU are untested or not built
- [x] CPU pinning, CPU model and topology, hugepages, NUMA policy (multi-cell NUMA untested)

## Operations
- [x] HTTPS for the web UI (self-signed certificate generated for non-loopback listeners, or your own with `-tls-cert`/`-tls-key`)
- [x] Bulk actions and tags (tags stored in libvirt `<metadata>`; bulk actions fan out from the browser)
- [x] Released under the Apache License 2.0 (`LICENSE`, `NOTICE` with third-party licences); release packages are not signed yet
- [x] Users and roles (admin, operator, viewer), API tokens, audit log, login rate limiting
- [x] Alerts: pool full, node down, host memory, failed scheduled job, VM crashed or paused on I/O error, certificate expiry; optional webhook (`-alert-webhook`)
- [x] Resource tree (Datacenter → nodes → VMs, storage, networks, host NICs) with quick actions; node page with tabs (summary, VMs, storage, network, updates, settings)
- [x] Theme from the user's DESIGN.md: dark by default, light option, bundled fonts, no animation. Idle cost measured: 0 running animations, at most one stats request per shown host every 5 s, none on pages without charts
- [x] Browser suite (`make e2e`, 35 spec files with 217 `test(` declarations counted from the source; re-run with the node command on Debian 13 nodes: 212 passed, 10 skipped with Service account nodes; 210 passed, 10 skipped with Root nodes) including a no-ellipsis check and a fit check: no element cut off or spilling out of its card at 1280 and 1440 px, both themes, plus phone width
- [x] Move a disk to another pool: live (block copy and pivot) or offline, with pre-flight checks, progress and cancel. Verified on Debian and Rocky; not tested under guest writes; dir pools only tested
- [x] Image catalogue: ten built-in cloud images, downloaded by the node and verified by checksum (same-origin checksum: catches corruption, not a compromised mirror); `<data>/images.json` adds or overrides entries
- [x] Cloud-init profiles (stored in the controller database, passwords as hashes only), selectable when creating and cloning
- [x] Console preview card on the VM overview (still image, refreshed only while visible), standalone console page, console toolbar (Ctrl+Alt+Del, key combos, paste text, screenshot, full screen)
- [x] go-libvirt could wedge a node connection when a stream ended abruptly: fixed with a small patch kept in `third_party/go-libvirt`, plus a per-node watchdog that reconnects an unresponsive connection. Screenshots still use virsh (a separate library bug)
- [ ] Later: scheduled replication to a standby node; optional shared storage and HA

## Design notes

**Sysprep.** Two mechanisms, chosen per node and guest:
- Linux, node has libguestfs: run `virt-sysprep` and `virt-customize` on the
  clone's disk while it is off. Works on any Linux image, no guest cooperation.
- Linux, no libguestfs: "seal" the template once through the guest agent
  (clear machine-id, remove host keys, `cloud-init clean`), then give each
  clone a cloud-init seed ISO built by the controller. Needs cloud-init in the
  template.
- Windows: the SID can only be changed by Windows itself. "Seal template"
  runs `sysprep /generalize /oobe /shutdown` through the guest agent; each
  clone gets an `unattend.xml` (account, password, hostname) on an attached
  ISO so first boot is unattended.

**Snapshot type.** The web UI has no type choice; the API still accepts `mode`. For a live snapshot with memory, "automatic" picks
external (guest keeps running, survives live migration), verified on Rocky 10
with SELinux (libvirt 11.10): create, revert and delete of a chain all work.
It falls back to internal (guest pauses, blocks live migration, offline
migration carries it) on nodes whose QEMU is confined by AppArmor, where
libvirt cannot delete external snapshots ("block-commit: Permission denied",
Debian 13), and when the VM's current snapshot is already internal. Shut-off
VMs get internal snapshots; live disk-only snapshots are always external.

**App metadata.** One root element `<virticle:meta>` in namespace
`https://virticle.invalid/ns/1` holds one child per feature
(`template`, `snapshot-schedule`, `tags`). Clones do not inherit it.
Before the rename the namespace was `https://virt-proj.invalid/ns/1` (prefix
`vp`): it is still read when no new element exists, and every write stores the
complete content under the new namespace and removes the old element.

**Access levels.** Every API route needs an entry in `routeLevels`
(`internal/api/access.go`); an unlisted route requires admin and fails a unit
test. Behind a reverse proxy, name it with `-trusted-proxy` so the same-origin
check and per-IP rate limit use the forwarded host and client address.

**One controller per set of nodes.** Scheduled snapshots are driven by the
controller from VM metadata, so two controllers pointed at the same nodes
would both run every schedule. `-no-scheduler` turns one of them off, and an
alert appears when another controller's scheduled snapshots are seen.

The project is called Virticle (earlier working name: `virt-proj`).

## Build and run

    make build                      # needs Go and Node.js
    ./bin/virticle -listen 127.0.0.1:8080 -data ./data

The first visit creates the administrator account. `data/` holds the SQLite
database and the controller's SSH key. Prepare each node from the Nodes page
(**Prepare this node**) or with `virticle node prepare`; it installs the node
command and the controller's key. See `docs/install-node.md`.
