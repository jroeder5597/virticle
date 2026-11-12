# Virticle

A web UI for KVM/QEMU/libvirt hosts. One program with the UI built in. It
manages your hosts (called "nodes") over SSH and installs no service on them.

![Datacenter summary](docs/img/datacenter.png)

Libvirt stays the source of truth. A VM made with `virsh` and a VM made in
Virticle look the same.

## What it does

| Area | Features |
| --- | --- |
| VMs | Create from cloud images, ISOs or templates. Clone. cloud-init. UEFI, Secure Boot, virtual TPM. Hot-plug hardware. Edit the libvirt XML. |
| Consoles | VNC and serial, in the browser. |
| Migration | Live and offline, between nodes, with a list of pre-flight checks. Node maintenance and evacuation. |
| Snapshots and backups | Manual or scheduled, with retention. Restore, export, import. |
| Storage and networks | Pools, volumes, uploads, an image catalogue. NAT and bridged networks, DHCP reservations. |
| Passthrough | PCI devices and GPUs. |
| Access | Users with three roles, API tokens, audit log, task log, alerts. |
| HTTPS | Self-signed, your own certificate, ACME, or behind a reverse proxy. |

## Quick start

Four steps: install the controller, create the administrator, add a node,
create a VM. The controller is the machine that runs Virticle. It needs a
Linux amd64 machine that can reach your nodes over SSH.

### 1. Install the controller

Pick one.

| Way | Commands |
| --- | --- |
| Package, Debian or Ubuntu | `sudo apt-get install ./virticle_<version>_amd64.deb` |
| Package, RHEL family | `sudo dnf install ./virticle-<version>-1.x86_64.rpm` then `sudo systemctl enable --now virticle` |
| Container | `docker compose up -d --build` |
| From source (needs Go and Node.js) | `make build` then `./bin/virticle -data ./data` |

Download packages from
[Releases](https://github.com/jroeder5597/virticle/releases). A package runs
a systemd service on `127.0.0.1:8080`. The container serves HTTPS with a
self-signed certificate on `127.0.0.1:8080`. More in
[docs/install.md](docs/install.md).

### 2. Create the administrator

1. Open `http://127.0.0.1:8080` (`https://` for the container).
2. Choose a user name and a password of at least 10 characters.

### 3. Add a node

**The machine the controller runs on.** If the controller is installed on the
machine that will run your virtual machines, the **Datacenter** page offers
**Set up this machine as a node** while there are no nodes. SSH must be running
on the machine (`sudo systemctl enable --now sshd`).

1. Open **Datacenter**.
2. Choose **Set up this machine as a node**.
3. Enter an administrator login for this machine (root, or a user that may use
   sudo). It is used once and is not stored.
4. Choose **Prepare**. When it has finished without errors the node is added.

To add another machine:

A node is a Linux host with hardware virtualisation. Debian 13 and Rocky
Linux 10 are tested. A node must be **prepared** before you add it. Preparing
installs libvirt and QEMU and sets up the access the controller needs.

1. Open **Nodes**, then **Add node**.
2. Choose **Prepare this node**.
3. Enter the address and an administrator login for the host. It is used once
   and is not stored.
4. Choose **Check** to see what would change. It changes nothing. Then
   choose **Prepare**. There is nothing else to tick, except **Root access
   instead of a service account** if you want that.
5. When prepare has finished without errors, choose **Add this node**.

![Add node dialog](docs/img/add-node.png)

You can prepare from a shell instead: `virticle node prepare admin@host`.
Add `--check` to change nothing.

Prepare gives the controller one of two kinds of access:

| Access | What the controller uses | When to choose it |
| --- | --- | --- |
| **Service account** (default) | The account `virticle-node`. It may run the node command as root through one sudo rule, and nothing else. | Almost always. |
| **Root** (`--root`) | The root account. | You do not want an extra account on the node. |

The node command is `virticle-node`, a small program that prepare installs on
the node. The controller runs it over SSH when something needs root. No
service runs on the node. Details are in
[docs/install-node.md](docs/install-node.md).

A host that already runs libvirt is prepared the same way. Its networks,
pools and VMs stay as they are. The host that runs the controller can be a
node too; the controller's own account `virticle` is not used for that.

### 4. Create a VM

1. Open **Storage**, select a pool, choose **Image catalogue** and download a
   cloud image.
2. Open **Virtual machines**, then **New VM**. Set the name, node, **Disk
   image**, CPU, memory and network.
3. Open the VM and choose its **Console** tab.

![VM overview](docs/img/vm-overview.png)

## Everyday use

| To do this | Do this |
| --- | --- |
| Find a node or VM | Use the tree on the left. Drag its edge to resize. |
| See what happened | Look at the **Tasks** panel at the bottom. Every action shows progress and result. Long actions run in the background. |
| Read a status | A green, yellow or red dot with the word Ok, Warning or Error. |
| Move a VM | **Actions > Migrate**. The pre-flight list says what blocks the move. |
| Switch to HTTPS or a reverse proxy | **Web access** (administrator). No restart. A change you do not confirm is undone after 60 seconds. |

![Migration pre-flight](docs/img/migrate.png)

Live migration is coordinated by the controller. Memory and disk data go
directly from node to node. When a migration or restore has to copy files,
they also go from node to node, with a key that the controller sets up. That
key can only deliver a new file into a storage pool. You set up nothing by
hand. The **SSH between nodes** row of the pre-flight list shows whether it
works.

## Upgrading from 0.1.0

Nodes prepared with 0.1.0 must be prepared again. Steps are in
[docs/install.md](docs/install.md#upgrading-from-010).

## Documentation

| If you want to | Read |
| --- | --- |
| Use the UI, with screenshots, and fix common problems | [docs/user-guide.md](docs/user-guide.md) |
| Install, upgrade, roll back, uninstall | [docs/install.md](docs/install.md) |
| Prepare a node, see what is installed on it, audit the node command | [docs/install-node.md](docs/install-node.md) |
| Set up HTTPS, ACME or a reverse proxy | [docs/web-tls.md](docs/web-tls.md) |
| Look up flags, roles, API tokens, data, schedules, limits | [docs/reference.md](docs/reference.md) |
| Build, test, release | [docs/development.md](docs/development.md) |
| See what is done, untested and open | [ROADMAP.md](ROADMAP.md) |

## Contributing and security

- Bugs and ideas: open an issue. Code: see [CONTRIBUTING.md](CONTRIBUTING.md).
- Vulnerabilities: report privately, see [SECURITY.md](SECURITY.md).

## Status

Early release. Tested: a controller on Linux amd64, and nodes on Debian 13
and Rocky Linux 10, each in both access modes.

Not tested: arm64, the packages on a clean machine, ACME certificate
issuance. A Debian node and a RHEL-family node cannot exchange VMs. Packages
are not signed. No container image is published.

## Licence

[Apache License 2.0](LICENSE). Third-party notices are in [NOTICE](NOTICE).
