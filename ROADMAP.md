# Objectives

A web front end for KVM/QEMU/libvirt that stays fully compatible with `virsh`.
One Go binary, no agent on the nodes: everything goes over SSH to each node's
libvirt socket. Libvirt is the source of truth; the app keeps only users, the
node list and task history.

Status: `[x]` done and verified against the lab, `[~]` in progress, `[ ]` open.

## Foundation
- [x] Spike: libvirt RPC over SSH, events, upload, live migration with disk copy, live snapshot, snapshot-preserving migration (`cmd/spike`)
- [x] Server binary with embedded web UI, SQLite state, local login
- [x] Node add/remove, persistent connections with reconnect (reconnect after a node outage not yet tested)
- [~] Live inventory driven by libvirt events (VM changes made in `virsh` appear at once; pool and network events still open)
- [x] Task log: every action with progress, result and libvirt error text
- [~] Raw XML editor with schema validation (done for VMs and for creating pools; networks and volumes open)

## VM basics
- [x] Lifecycle: start, shutdown, reboot, force off, reset, pause, resume, delete
- [~] Autostart on host boot (done); graceful shutdown with timeout before force off (open)
- [x] VNC console in the browser (noVNC, tunnelled over the node's SSH connection)
- [x] Serial/text console in the browser (xterm.js)
- [x] Edit CPU, memory, disks, network interfaces through forms (Hardware tab; says whether a change applied live or needs a restart)
- [~] Hot-plug disks and NICs, vCPU and memory changes, online disk resize (done; hot-unplug depends on the guest). USB devices open
- [ ] Firmware options: UEFI, Secure Boot, virtual TPM
- [ ] Live stats: CPU, memory, disk, network per VM and per host
- [~] Guest info on the VM card: IPs from the agent with DHCP leases as fallback (done); OS and filesystems (open)
- [~] Guest agent prompt on the VM card: adds the virtio channel in one click and lists install commands per OS; still open: detect the guest OS, attach the virtio-win ISO for Windows

## Storage
- [~] Pools: create, start, stop, remove (dir tested; NFS, LVM, iSCSI forms and raw XML written but untested)
- [x] Volumes: create, resize, clone, upload, download from URL (the node fetches it with curl), delete
- [x] CD-ROM media: insert, swap, eject (live); boot order (config, next start)
- [ ] Import existing disks: qcow2, raw, VMDK, OVA

## Provisioning
- [~] Create VM from ISO, disk image or empty disk (form works; ISO path not yet tested with a real installer)
- [x] Create VM from a cloud image with cloud-init (hostname, user, password, SSH keys, packages, raw user-data); seed ISO built in pure Go
- [x] Templates: mark a VM as a template; full and linked clones (same node)
- [x] Inject SSH keys and user/password when cloning a template (cloud-init seed)
- [x] Sysprep on clone, Linux: `virt-sysprep` where the node has it, else "seal template" through the guest agent
- [~] Sysprep on clone, Windows: seal via `sysprep /generalize` through the agent and unattend ISO are written but UNTESTED (no Windows guest in the lab)

## Snapshots and backup
- [~] Snapshots with memory, tree view, revert, delete (done). Open: default type (see note), quiesce untested with a real agent, UEFI guests untested
- [x] Scheduled snapshots with retention (schedule stored in domain metadata; times in UTC)
- [ ] Backup and restore to another pool or host; export/import a VM as one file

## Networking
- [ ] Virtual network editor: NAT, isolated, bridged, VLAN tags, DHCP reservations

## Multi-node
- [x] Live migration with pre-flight checks (CPU, networks, free space, passthrough, SSH trust, name clashes)
- [x] Migration with disk copy; snapshot-preserving migration (external snapshots); source disk cleanup; cancel
- [~] Offline migration (done, carries internal snapshots too); UEFI/NVRAM and TPM paths untested
- [ ] Encrypted migration streams (TLS)
- [ ] Evacuate a node; show pending host updates

## Hardware
- [ ] Passthrough picker: PCI, GPU, USB with IOMMU groups; warns that it blocks live migration
- [ ] CPU pinning, hugepages, NUMA

## Operations
- [ ] HTTPS for the web UI (currently plain HTTP, bound to localhost by default)
- [ ] Bulk actions and tags (tags stored in libvirt `<metadata>`)
- [ ] Users and roles, API tokens, audit log
- [ ] Alerts: pool full, node down, failed scheduled job
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

**Snapshot type.** Internal snapshots (inside the qcow2) are the default:
create, revert and delete work everywhere, but the guest pauses while memory
is saved and they block live migration (offline migration carries them).
External snapshots keep the guest running and survive live migration, but on
the Debian/AppArmor lab nodes libvirt fails to delete them ("block-commit:
Permission denied", an AppArmor profile problem outside this app). To decide
after testing external snapshots on an SELinux host (Rocky).

**App metadata.** One root element `<vp:meta>` in namespace
`https://virt-proj.invalid/ns/1` holds one child per feature
(`template`, `snapshot-schedule`). Clones do not inherit it.

**Open:** project name (working name `virt-proj`).

## Build and run

    make build                      # needs Go and Node.js
    ./bin/virt-proj -listen 127.0.0.1:8080 -data ./data

The first visit creates the administrator account. `data/` holds the SQLite
database and the controller's SSH key; its public half is shown in the
"Add node" dialog and must be added to `authorized_keys` on each node.
