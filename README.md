# Virticle

A web UI for KVM/QEMU/libvirt hosts. One program with the UI built in. It
manages your hosts (called "nodes") over SSH and installs no service on them.

![Datacenter summary](docs/img/datacenter.png)

Libvirt stays the source of truth. A VM made with `virsh` and a VM made in
Virticle look the same.

## What it does

| Area | Features |
| --- | --- |
| VMs | Create from cloud images, ISOs or templates. Clone. cloud-init. UEFI by default, Secure Boot, virtual TPM. Hot-plug hardware. Speed limit per network card. Edit the libvirt XML. |
| Consoles | VNC and serial, in the browser. |
| Migration | Live and offline, between nodes, with a list of pre-flight checks. Node maintenance and evacuation. A migration network per node, with an automatic limit on a shared link. |
| Snapshots and backups | Manual or scheduled, with retention. Restore, export, import. Backup and restore of the controller itself. |
| Storage | Pools, volumes, uploads, an image catalogue. |
| Networks | NAT and bridged networks, DHCP reservations. Virtual switches with VLANs, several uplinks (failover or LACP), rollback if a change cuts the node off. A topology view per datacenter and per node. Warnings when nodes differ. |
| Monitoring | Performance history for 7 days with charts, a `/metrics` endpoint for Prometheus, alerts, and notifications to Slack, Mattermost, Discord or a webhook. |
| Passthrough | PCI devices and GPUs. |
| Access | Users with three roles, two-step sign-in, sign-in through OIDC or LDAP, API tokens, audit log, task log. |
| HTTPS | Self-signed (the default), your own certificate, ACME, or behind a reverse proxy. |

## Quick start

Four steps: install the controller, create the administrator, add a node,
create a VM. The controller is the machine that runs Virticle. It needs a
Linux amd64 machine that can reach your nodes over SSH.

### 1. Install the controller

Pick one.

| Way | Commands |
| --- | --- |
| Package repository (preferred) | Add the signed apt or dnf repository once, then `sudo apt install virticle` or `sudo dnf install virticle`. Steps in [docs/install.md](docs/install.md#install-from-the-package-repository). |
| Downloaded package, Debian or Ubuntu | `sudo apt-get install ./virticle_<version>_amd64.deb` |
| Downloaded package, RHEL family | `sudo dnf install ./virticle-<version>-1.x86_64.rpm` |
| Container | `docker compose up -d --build` |
| From source (needs Go and Node.js) | `make build` then `./bin/virticle -data ./data` |

Download packages from
[Releases](https://github.com/jroeder5597/virticle/releases). A package runs a
systemd service that serves HTTPS with a self-signed certificate on port 8443
of all interfaces. It does not touch the firewall: run `sudo virticle setup`
once. It opens the web port in firewalld or ufw and prints the address to
open. The container serves the same on port 8443 (compose.yaml publishes it on
`127.0.0.1`). More in [docs/install.md](docs/install.md).

### 2. Create the administrator

1. Open the address that `sudo virticle setup` printed, `https://<address>:8443`
   (the journal says the same: `journalctl -u virticle`). Your browser warns
   about the self-signed certificate. A source build started by hand in a data
   directory without users behaves the same.
2. Choose a user name and a password of at least 10 characters.

### 3. Add a node

A node is a Linux host with hardware virtualisation. Debian 13 and Rocky
Linux 10 are tested. Adding a node prepares it: libvirt, QEMU and a small
node command are installed and the access the controller needs is set up.
Nothing on the host is removed. A host that already runs libvirt is prepared
the same way, and its networks, pools and VMs stay as they are.

**The machine the controller runs on.** While there are no nodes, the
**Datacenter** page offers **Set up this machine as a node**. SSH must be
running on the machine (`sudo systemctl enable --now sshd`). Enter an
administrator login for this machine (root, or a user that may use sudo; used
once, not stored) and choose **Add node**.

**Another machine.**

1. Open **Nodes**, then **Add node**.
2. Enter the address and an administrator login for the host. It is used once
   and is not stored.
3. Choose **Check** to see what would change. It changes nothing. Then choose
   **Add node**, which prepares the host and adds it.

![Add node dialog](docs/img/add-node.png)

From a shell: `virticle node prepare admin@host` (add `--check` to change
nothing). The controller signs in to the node as the account `virticle-node`,
which may run the node command as root through one sudo rule and nothing else.
**Root access instead of a service account** (`--root`) is the alternative.
Details are in [docs/install-node.md](docs/install-node.md).

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
| See how a VM was doing | The **Performance** tab of the VM or node. |
| Change the web address, certificate or port | **Settings > Web access** (administrator). A change you do not confirm is undone after 60 seconds. |
| Back up the controller | **Settings > Controller backup**, or `sudo virticle backup`. |

![Migration pre-flight](docs/img/migrate.png)

Live migration is coordinated by the controller. Memory and disk data go
directly from node to node. When a migration or restore has to copy files,
they also go from node to node, with a key that the controller sets up. That
key can only deliver a new file into a storage pool. You set up nothing by
hand.

## Upgrading

Upgrade the package, then prepare every node again (the node command has a new
version). Steps for 0.2.0 to 0.3.0, and for older versions, are in
[docs/install.md](docs/install.md#upgrading).

## Documentation

| If you want to | Read |
| --- | --- |
| Use the UI, with screenshots, and fix common problems | [docs/user-guide.md](docs/user-guide.md) |
| Install, upgrade, roll back, uninstall | [docs/install.md](docs/install.md) |
| Prepare a node, see what is installed on it, audit the node command | [docs/install-node.md](docs/install-node.md) |
| Back up the controller, restore it, move it, recover without a backup | [docs/controller-backup.md](docs/controller-backup.md) |
| Set up HTTPS, ACME or a reverse proxy | [docs/web-tls.md](docs/web-tls.md) |
| Two-step sign-in, sign-in with an identity provider (OIDC) or a directory (LDAP, Active Directory) | [docs/sign-in.md](docs/sign-in.md) |
| Feed Prometheus or Grafana | [docs/metrics.md](docs/metrics.md) |
| Look up commands, flags, roles, API tokens, data, schedules, limits | [docs/reference.md](docs/reference.md) |
| Build, test, release | [docs/development.md](docs/development.md) |
| See what is done, untested and planned | [ROADMAP.md](ROADMAP.md) |

## Contributing and security

- Bugs and ideas: open an issue. Code: see [CONTRIBUTING.md](CONTRIBUTING.md).
- Vulnerabilities: report privately, see [SECURITY.md](SECURITY.md).

## Status

Early release. Tested: a controller on Linux amd64, and nodes on Debian 13
and Rocky Linux 10, each in both access modes.

Not tested: arm64, ACME certificate issuance, LACP against a real switch,
sign-in with identity providers other than the test provider, and virtual
switches on nodes that use ifupdown (refused) or plain systemd-networkd. A
Debian node and a RHEL-family node cannot exchange VMs. The files on the
Releases page are not signed (the package repository is). No container image
is published.

## Licence

[Apache License 2.0](LICENSE). Third-party notices are in [NOTICE](NOTICE).

Created with the assistance of LLM coding tools.
