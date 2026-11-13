# User guide

How to do the everyday things in the web UI. To install, see
[install.md](install.md). To add hosts, see [install-node.md](install-node.md).

- [The screen](#the-screen)
- [Nodes](#nodes)
- [Virtual machines](#virtual-machines)
- [Console](#console)
- [Hardware](#hardware)
- [Snapshots](#snapshots)
- [Backups](#backups)
- [Migration](#migration)
- [Storage](#storage)
- [Networks](#networks)
- [Users, tokens and the audit log](#users-tokens-and-the-audit-log)
- [Troubleshooting](#troubleshooting)

## The screen

![Datacenter summary](img/datacenter.png)

- **Top bar**: the main pages, alerts, theme, your account.
- **Tree** (left): nodes with their VMs, storage and networks. Type in
  **Filter** to narrow it. Right-click an entry for its actions. Drag the edge
  to resize.
- **Tasks** (bottom): the last actions with progress and result. Running tasks
  stay on top. Drag the top edge to resize, or collapse it with the arrow.
  **All tasks** opens the full log.
- **Status** is always a dot and a word: green Ok, yellow Warning, red Error.

Long actions (snapshots, migration, backups, downloads) run in the background.
You can leave the page; the task keeps going.

## Nodes

![Nodes](img/nodes.png)

A node is a host that runs your VMs.

**This machine as a node.** While there are no nodes, the **Datacenter** page
offers **Set up this machine as a node** when the controller's own machine can
run VMs. SSH must be running on it (`sudo systemctl enable --now sshd`). Choose
the button, enter an administrator login for this machine (used once, not
stored) and choose **Add node**. The node is added when prepare has finished
without errors. The page shows the machine's network address, because other
nodes use that address for migrations; change it if they cannot reach it.

To add another host:

1. Open **Nodes**, then **Add node**.
2. Enter the address and an administrator login. **Add node** installs
   libvirt, QEMU, the guest disk tools and the node command
   (`virticle-node`) if they are missing, sets up the access the controller
   needs, and adds the node. Choose **Check** first to see what would change.
   The only option is **Root access instead of a service account**; leave it
   off unless you want Root. See [install-node.md](install-node.md).
3. For a host that you already prepared in a shell, open **Advanced** and
   choose **Already prepared**. Enter its address and SSH user
   (`virticle-node` for a Service account, `root` for Root). A node prepared
   by version 0.2.0 uses `virticle`.

![Add node dialog](img/add-node.png)

When the node's **Settings** say it was prepared by an older version, choose
**Prepare this node** there.

What you can do with a node:

| To do this | Do this |
| --- | --- |
| See a node's summary, VMs, storage, networks, updates and settings | Click the node. |
| Find out why a node is Offline | **Settings > Test connection**. It runs each connection step and shows which one fails. **Settings** warns when the node is not prepared. |
| Keep new migrations away from a node, or empty it | **Maintenance**. The node is never chosen as a migration target. When the node has VMs, tick **Move its virtual machines to other nodes** to live-migrate them away. **Evacuate** appears while a node is in maintenance. |

## Virtual machines

### Create

![New virtual machine](img/new-vm.png)

1. **Virtual machines > New VM** (or right-click a node > **Add VM**).
2. Pick what to install from:
   - **Installer ISO**: boots an ISO from a pool. Upload ISOs under Storage.
   - **Disk image**: copies a cloud image. Open **Cloud-init** to set the
     user, password and SSH key. The guest agent is installed too.
   - **Empty disk**.
3. A Linux VM gets UEFI firmware unless you choose BIOS under **Advanced** (a node without UEFI firmware gets BIOS). UEFI guests usually need at least 512 MiB. For Windows, set **Guest OS type** to **Windows**: it picks UEFI, Secure
   Boot and a virtual TPM for you.
4. **Advanced** holds the CPU type, firmware, Secure Boot, TPM and **Existing
   volume** (use a volume as the disk as it is). Leave **CPU type** on
   **Cluster baseline** unless you need host features: it is the best CPU every
   online node can run, so live migration works.
5. **Create**. Every new VM gets a serial console.

### Manage

![VM overview](img/vm-overview.png)

- The **Overview** tab shows a console preview, state, guest agent, addresses,
  passthrough devices and live usage.
- **Actions** holds everything else:

| Menu | Items |
| --- | --- |
| Power | Start, stop and restart, gracefully or forced. |
| Edit Settings | Name, CPU, memory, and the other Hardware cards. |
| Snapshots | Take, manage, schedule. |
| Clone | Clone the VM. A template is cloned linked; **Advanced** offers a full copy. |
| Convert to template | Optionally resets the identity of a running guest. **Convert back to a VM** undoes it. |
| Migrate | To another node, or move a disk to another pool. |
| More | **Edit XML**, and **Delete VM** (its disks are deleted too unless you untick the box). |

Sync guest clock is under **Power**.

- **Tags** sit beside the name. Click **+ Add Tag**; filter by tag in the tree
  and the VM list.
- Guest details (OS, addresses, disk use inside the guest) need the QEMU guest
  agent installed in the VM.

## Console

![VNC console](img/vm-console.png)

- **Display (VNC)** is the VM's screen. **Serial console** is a text terminal.
- **Keys** sends combinations the browser would swallow. **Paste text** types
  your clipboard into the guest.
- A VM without a serial console: the **Console** tab offers to add one.
- Viewers cannot open a console.

## Hardware

![Hardware](img/vm-hardware.png)

- Each card edits one area: CPU and memory, firmware and TPM, boot order,
  CD-ROM, disks, network interfaces, passthrough and **Advanced** (CPU mode,
  pinning, huge pages).
- A change that cannot apply to a running VM is saved and marked as applying
  at the next start.
- **Passthrough** lists the host's PCI devices. A device shares its IOMMU
  group with others; the dialog shows what moves together. The host needs
  IOMMU enabled in firmware and on the kernel command line. A graphics card
  that the host itself is using cannot be attached.
- **Actions > More > Edit XML** edits the full libvirt definition.

## Snapshots

![Snapshots](img/vm-snapshots.png)

- **Take snapshot** runs as a task with a progress bar.
- The tree shows which snapshot the VM is based on (**You are here**).
- The round arrow reverts; the bin deletes.
- **Set up schedule**: interval or daily, keep the newest N.
- A snapshot with memory pauses the guest while memory is saved. A disk-only
  snapshot does not.

## Backups

1. Open the VM's **Backups** tab.
2. **Back up now**: pick the target node and pool. A running VM keeps running.
3. **Restore** a backup as a new VM or over the original.
4. **Save schedule** sets interval or daily runs and how many to keep.

The **Backups** page lists every backup on every node. A backup can be
downloaded as one file and imported on another installation. The format is in
[reference.md](reference.md#backup-format).

## Migration

![Migration pre-flight](img/migrate.png)

1. Choose **Actions > Migrate > Migrate to another node**.
2. Pick the target node.
3. Read the pre-flight checks. This is the list of checks shown before a
   migration. It covers CPU, memory, networks, storage, SSH between the nodes
   and the migration ports.
   - A red row blocks the move and names the fix.
   - A yellow row is a warning.
4. Choose **Migrate**. With **Live migration** ticked the VM keeps running.

Good to know:

- Disks are copied along. You do not need shared storage.
- The controller coordinates a live migration. Memory and disk data go
  directly from node to node. The nodes need no SSH trust between them for it.
- When files have to be copied (disks on local storage, snapshot files, a
  backup restored on another node), they go straight from node to node. The
  controller sets up a key on each node for this. The key can only deliver a
  new file into a storage pool. It works between any two nodes, Service
  account or Root.
- Two pre-flight rows tell you about the network between the nodes:

  | Row | It tells you |
  | --- | --- |
  | **SSH between nodes** | Whether a file copy is needed and whether the path works. |
  | **Migration ports** | Whether the target's firewall lets the migration data through. |

- The streams are encrypted when both nodes are set up. The controller sets
  this up by itself on every prepared node; see **Settings > Encrypted
  migration** on the node.
- A Debian node and a RHEL-family node cannot exchange VMs.

### Migration network

By default a node receives migration data at its management address, the one
the controller uses. When a node has more than one network card, choose which
one carries the data, so that management traffic is not held up by a big
transfer:

1. Open the node, **Settings**, and choose **Change** next to **Migration
   network**. The row appears only when the node has more than one address.
   (The same list is under **Advanced** in **Edit connection**. In the
   **Topology** tab, **Use for migration** next to an address does the same,
   and a **Migration** mark shows the address in use.)
2. Pick the card, for example `enp2s0 (10.0.1.5), 10 Gb/s`. **Same as
   management** is the default.

Saving checks that the other nodes can reach the address on the migration
ports and on SSH, and renews the node's encryption certificate for it at once.
A problem with another node is a warning; the setting is saved anyway. The
data of live migrations, copied files (offline migration, snapshots) and
backups restored on another node all use this network, so the firewall must
allow the migration ports and SSH there. The pre-flight row **Migration
network** names the cards in use.

When the data travels over the same card as management, the transfer is
limited so that it cannot fill the link:

| Situation | Limit |
| --- | --- |
| Migration has its own card on both nodes | None. |
| Migration shares the management card, link speed known | 80% of the link, for example 800 Mbit/s on 1 Gb/s. The lower of the two nodes' limits applies. |
| Migration shares the management card, link speed unknown (virtual network cards often report none) | None. The pre-flight row **Transfer limit** says so. |

Change this for a node under **Advanced** (**Limit on a shared link**):
**80% of the link (default)**, **No limit**, or **Custom (Mbit/s)**. The limit
covers the memory stream, the live disk copy and file copies. Libvirt counts
the whole disk, including empty space, so a large sparse disk takes
longer than its stored data suggests. Two networks on one subnet need a route
that sends the traffic to the chosen card; use a separate subnet or VLAN for a
dedicated migration network.

## Storage

![Storage](img/storage.png)

- Select a pool to see its volumes.
- **Image catalogue** downloads common cloud images. **Download from URL** and
  **Upload image or ISO** bring your own.
- **Import disk or OVA** converts foreign disk formats.
- **New pool** adds a directory, NFS, LVM or iSCSI pool. Only directory pools
  are tested.
- Per volume: clone, resize, delete.

## Networks

![Networks](img/network.png)

- **New network** creates a NAT, routed, isolated, open or bridge network.
- Select a network to see its DHCP leases and add address reservations.
- **Host interfaces** lists each node's bridges and NICs.

### Network topology

The **Topology** tab, on the Datacenter page (all nodes) and on each node page (that node), draws how virtual
machines reach the network, like a VMware standard switch. Viewers and operators see it read-only; administrators can
change it (see [Virtual switches](#virtual-switches)). **Refresh** reloads it; it also reloads
by itself when networks or virtual machines change. In the Datacenter view the first three nodes are open and the
others open on a click, and a network that has the same name on several nodes gets the same coloured edge.

| Part | What it is |
| --- | --- |
| Switch | A bridge on the node: a Linux or Open vSwitch bridge, or the bridge libvirt made for one of its networks. |
| Uplinks (right) | The network cards or bonds attached to the bridge, with link state, speed and addresses. A NAT, routed or isolated libvirt network has none and says so instead. |
| Networks (left) | The libvirt networks on the bridge (mode, VLAN, DHCP range) and **Direct to bridge** for VM interfaces that use the bridge itself. Each lists its virtual machines with MAC and IP address. |
| This node's address | The node's own address on the switch. |

**Unused network cards** lists cards that belong to no bridge. The card or bridge that holds the address the
controller uses to reach the node is marked **Management**. Addresses of virtual machines come from DHCP leases,
so a guest with a fixed address shows none. Uplinks of Open vSwitch bridges are not listed.

### Virtual switches

Administrators can create and change virtual switches on the Topology tab of a node (and of the Datacenter page, where
each node has its own button). Nothing else is needed on the node: Virticle writes the node's own network settings.

| Word in the app | What it is |
| --- | --- |
| Virtual switch | A Linux bridge on the node. |
| Uplink | The network card the virtual switch uses to reach the outside network. A switch without one connects the virtual machines on this node only. |
| Network | What a virtual machine attaches to. A network on a virtual switch is a libvirt network in bridge mode, so it appears in **New VM** and **Add network interface** like any other. |
| VLAN | A tag on a network, so that several networks share one switch and one cable. |
| This node's address | The node's own address. When the uplink had one, it moves to the virtual switch and stays the same. |

**Add a virtual switch**

1. Open the node's **Topology** tab and select **Add virtual switch**. On a card under **Unused network cards**, select
   **Use for a new virtual switch** instead.
2. Enter a name (the dialog offers `vsw0`, `vsw1` and so on) and choose the **Uplink**, or **None (internal only)**.
   Keep **VLANs** selected if you want VLAN tags. **Advanced** has the allowed VLAN ids (empty allows all) and the time
   the node waits for the controller to confirm (90 seconds).
3. Select **Review**. The dialog lists in plain words what will happen on the node; **Details** has the files and
   commands.
4. Select **Apply**. The dialog shows three steps: applying on the node, waiting for the node, confirmed. You can close
   it; the result is also in the task log.

**Add a network with a VLAN.** Select **Add network** in the box of the virtual switch, enter a name and, if the
switch has VLANs, a VLAN number (empty means no tag). The dialog shows the ids already in use on the switch. When other
nodes have a virtual switch with the same name, **Create the same network on ...** is selected: a virtual machine can
only migrate to a node that has a network of the same name. **Actions** on the switch has **Edit VLANs** (refused while a
network uses an id you remove) and **Remove** (not possible while networks or virtual machines use the switch; the
node's address returns to the card). A bridge that somebody made by hand shows **Not managed by Virticle** and cannot be
changed here.

**If the node becomes unreachable.** Moving the address of the card the controller uses is the risky case. The node
saves its previous settings first and restores them by itself if the controller does not confirm within the confirm time.
The dialog then says "The node did not come back in time and restored its previous settings." If you reload the page
while a change is waiting, the node's section shows it with the seconds left and **Roll back now**; a change that was
rolled back is shown as one line you can dismiss.

**Which nodes.** Nodes that use NetworkManager, or netplan with systemd-networkd. A node that uses ifupdown is not
supported yet (the button is disabled and says so). VLANs on a virtual switch need libvirt 11 or later on the node. A
node prepared with an older version of Virticle must be prepared again.

## Users, tokens and the audit log

| Page | Who | What it does |
| --- | --- | --- |
| **Users** | Admin | Add accounts as viewer, operator or admin. See [Roles](reference.md#roles). |
| **Account** | Everyone | Change your password, create API tokens. |
| **Audit** | Admin | Lists every state-changing request, who made it and from where. |
| **Web access** | Admin | HTTPS and reverse proxy settings. See [web-tls.md](web-tls.md). |

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Node shows Offline | Node **Settings > Test connection**. It names the failing step. |
| Guest agent "Not responding" | Install and start `qemu-guest-agent` in the VM. |
| Migration blocked | Read the red pre-flight row. Usually a missing network or pool on the target, or a firewall between the nodes (SSH between nodes, Migration ports). |
| Migration or backup refused for a node | **Settings** says the node is not prepared: it has no node command, or one from an older version. Choose **Prepare this node** there, or run `virticle node prepare` for it again (see [install.md](install.md#upgrading-from-010)). |
| Locked out after a web access change | Wait 60 seconds for the automatic revert, or run `virticle web reset` and restart. |
| "No ISO images on this node" | Upload one under **Storage**. |
| A task failed | **Tasks** shows the libvirt error in the Result column. |
