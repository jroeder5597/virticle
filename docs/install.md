# Installing Virticle

Virticle is one controller program. It needs nothing on the machines it
manages (the "nodes") except SSH access and libvirt. To get a node ready, see
[install-node.md](install-node.md). For HTTPS and reverse proxies, see
[web-tls.md](web-tls.md).

On this page:

- [Package repository](#install-from-the-package-repository) (preferred), [downloaded package](#package-from-a-download-debian-ubuntu-rhel-like), [container](#container) or [binary](#binary)
- [First run](#first-run)
- [Upgrading](#upgrading), with the steps from 0.2.0 and from 0.1.0, and how to roll back
- [Uninstall](#uninstall)

Releases are at <https://github.com/jroeder5597/virticle/releases>. Below,
`RELEASES_URL` is `https://github.com/jroeder5597/virticle/releases/download`.

## Supported platforms

| Role | Platform | Status |
| --- | --- | --- |
| Controller | Linux amd64 | Tested |
| Controller | Linux arm64 | Built, not tested |
| Controller | Other operating systems or architectures | Not built |
| Node | Debian 13 | Tested |
| Node | Rocky Linux 10 | Tested |
| Controller and node on one machine | Fedora 43 (firewalld, SELinux enforcing), Ubuntu 24.04 (ufw, AppArmor, libvirt 10.0) | Tested |
| Node | Any other distribution with libvirt and SSH | Not tested |

| Install type | Notes |
| --- | --- |
| `.deb` (Debian, Ubuntu) and `.rpm` (RHEL family) | Include a systemd service. Installed on a fresh Fedora 43 and a fresh Ubuntu 24.04 machine and followed through to a running VM. The files on the Releases page are not signed (check them against `checksums.txt`); the package repository below is signed. |
| `.tar.gz` | The plain binary. |
| Container | Built from the source tree. No image is published. |

The controller is pure Go (no CGO, no C libraries), so the binary is static.

## Install from the package repository

This is the preferred way. The repository is signed, and later versions arrive
with your normal updates (`sudo apt upgrade` or `sudo dnf upgrade`). It serves
Linux amd64 and arm64.

**Debian, Ubuntu**

1. Add the signing key:

       sudo install -d -m 0755 /etc/apt/keyrings
       sudo curl -fsSL https://jroeder5597.github.io/virticle/virticle.gpg -o /etc/apt/keyrings/virticle.gpg

2. Add the repository:

       echo "deb [signed-by=/etc/apt/keyrings/virticle.gpg] https://jroeder5597.github.io/virticle/apt stable main" | sudo tee /etc/apt/sources.list.d/virticle.list

3. Install:

       sudo apt update
       sudo apt install virticle

**Fedora, Rocky Linux, AlmaLinux, RHEL**

1. Add the repository (it names the signing key):

       sudo curl -fsSL https://jroeder5597.github.io/virticle/virticle.repo -o /etc/yum.repos.d/virticle.repo

2. Install. `dnf` shows the key's fingerprint and asks you to accept it once:

       sudo dnf install virticle

**Check the key.** The fingerprint of the signing key is
`FINGERPRINT-TO-BE-FILLED`. For Debian and Ubuntu, `gpg --show-keys
/etc/apt/keyrings/virticle.gpg` prints it. For dnf, compare it with the one
`dnf` shows before you accept. The same fingerprint is on
<https://jroeder5597.github.io/virticle/>.

**Then run the setup.** The package never opens the firewall. Run
`sudo virticle setup` once after installing (described below).

## Package from a download (Debian, Ubuntu, RHEL-like)

Choose one way. In both, replace `1.2.3` with the version you want.

**With the install script.** It downloads the package, checks its SHA-256
against `checksums.txt` and calls `apt-get` or `dnf`. Read it first.
`--dry-run` shows what it would do.

    VIRTICLE_RELEASES_URL=RELEASES_URL sh scripts/install.sh --version 1.2.3

**By hand.**

    # Debian family
    sudo apt-get install ./virticle_1.2.3_amd64.deb
    # RHEL family
    sudo dnf install ./virticle-1.2.3-1.x86_64.rpm

The package installs:

| What | Where |
| --- | --- |
| Program | `/usr/bin/virticle` |
| Service | `/usr/lib/systemd/system/virticle.service` |
| Configuration | `/etc/virticle/virticle.env` (upgrades keep your edits) |
| Manual and documentation | `/usr/share/man/man1/virticle.1.gz`, `/usr/share/doc/virticle/` (the README and the `docs/` folder with the pictures, laid out as in the repository so that the links work) |
| System user and group | `virticle` |

A new install serves HTTPS with a self-signed certificate on port 8443 of all
interfaces; the journal line `open https://<address>:8443 to create the
administrator` says where. An installation from before this default, which has
an administrator and no stored web settings, keeps listening on
`127.0.0.1:8080` and the **Web access** page says so. The same rule applies to
every start without a `-listen` flag: a data directory without users gets the
HTTPS default, one with users keeps what it has.

| Family | After installing |
| --- | --- |
| Debian, RHEL | The service is enabled and started. An upgrade restarts it when it was running. |

### Setup: open the firewall

The package does not touch the firewall. Run this once after installing:

    sudo virticle setup

It opens the web port when firewalld or ufw is active, makes sure the service
runs, and prints the address to open. Running it again changes nothing; if you
later change the web port, run it again and it moves the rule to the new port.
The **Web access** page tells you to run it while a port change waits for
confirmation: setup then opens the new port and keeps the old one open, so run
it once more after you chose **Keep this setting** to close the old port.
`sudo virticle setup --check` only looks, and `sudo virticle setup --undo`
removes the firewall rule it added (do this before you uninstall). Then open
the address it prints.

Check the service with `systemctl status virticle`, `journalctl -u virticle` and
`virticle version`.

### Ports and firewall

| Where | Port | Needed for |
| --- | --- | --- |
| Controller | The web port, `8443` by default (set on the **Web access** page) | Browsers and the API. `virticle setup` opens it. |
| Each node | SSH, `22` or the port you gave for the node | The controller signs in to the node. Open to the controller only if you restrict SSH. |
| Each node, from the other nodes | TCP `49152` to `49215` | Data of live migrations. `virticle node prepare` opens it. |

Consoles (VNC, serial) go through the web port and SSH and need no port of
their own. Only firewalld and ufw are handled. With other firewall rules
(nftables, iptables) the command tells you the ports to open yourself.

### Configuration

1. Edit `/etc/virticle/virticle.env`. Only root can read it.
2. Run `sudo systemctl restart virticle`.

The file holds:

- `VIRTICLE_ARGS`: extra flags, for example `-no-scheduler`. `virticle -help`
  lists them and [reference.md](reference.md#flags) describes each. The web
  address and certificate are set on the **Web access** page.
- `VIRTICLE_JUMP_PASSWORD`: the password for node jump hosts (a jump host is a
  machine the controller connects through to reach a node).

The service may listen on ports below 1024 (80 and 443) so that the **Web
access** page can use them.

The service is sandboxed (`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
no new privileges, only the right to bind low ports, system call filter, only
the address families IPv4, IPv6, Unix and netlink; netlink is how it lists the
network interfaces for the **Web access** page). It can write only to
`/var/lib/virticle`. A certificate file named on the **Web access** page must be readable by
the user `virticle`, for example under `/etc/virticle/`.

## Container

    docker build -t virticle .      # in an LXC: podman build --network=host
    docker run -d --name virticle -p 127.0.0.1:8443:8443 -v virticle-data:/data virticle

or `docker compose up -d --build` with the `compose.yaml` of this repository.

- The image is based on `gcr.io/distroless/static` and runs as the non-root
  user 65532.
- All state is in the volume `/data`. Port 8443 is exposed (8080 too, for
  volumes from 0.2.0, see [From 0.2.0 to 0.3.0](#from-020-to-030)).
- The controller serves HTTPS with a self-signed certificate that it makes in
  `/data`. Open `https://127.0.0.1:8443`. The image passes no `-listen`, so
  the **Web access** page manages the listener (see [web-tls.md](web-tls.md)).
- Pass environment variables such as `VIRTICLE_JUMP_PASSWORD` with `-e` or
  `env_file`.
- No registry has been chosen, so no image is published.

## Binary

1. Download `virticle_VERSION_linux_amd64.tar.gz` (or `arm64`) from
   `RELEASES_URL`.
2. Check it against `checksums.txt`:
   `sha256sum -c --ignore-missing checksums.txt`.
3. Unpack it and run:

       ./virticle -data ./data

To build from source, run `make build` (needs Go and Node.js). The result is
`bin/virticle`. `make dist` cross-compiles linux/amd64 and linux/arm64 into
`dist/`.

## First run

1. Open the address that `sudo virticle setup` printed, for example
   `https://<address>:8443` (the journal line `open ... to create the
   administrator` says the same).
2. Create the administrator account. The password needs at least 10
   characters.
3. Add nodes. If this machine will run your VMs too, choose **Set up this
   machine as a node** on the **Datacenter** page (SSH must be running on it;
   the administrator login is used once). Otherwise add each node from the **Nodes** page (**Add node**) or prepare it with
   `virticle node prepare`. [install-node.md](install-node.md) describes the
   preparation.

## Where data lives

| Install | Data directory |
| --- | --- |
| Package | `/var/lib/virticle` (owner `virticle`, mode 0700) |
| Container | the volume mounted at `/data` |
| Binary | the directory given with `-data` (default `./data`) |

The directory holds:

| File | Content |
| --- | --- |
| `virticle.db` | Users, password hashes, API token hashes, node list, web access and alert settings, task and audit logs |
| `id_ed25519`, `id_ed25519.pub` | The controller's SSH key for the nodes |
| `known_hosts` | Host keys of the nodes |
| `instance-id` | Identifier of this controller |
| `secret.key` | The key that encrypts stored secrets (two-step seeds, sign-in and notification secrets) |
| `tls-cert.pem`, `tls-key.pem` | The certificate, when the controller made one |
| `metrics.db` | The performance history (7 days). Can be deleted; only the history is lost |
| `backups/` | The daily controller backups, when switched on |

The full list is in [reference.md](reference.md#where-state-lives).

Back it up with `sudo virticle backup`, which works while the controller runs
(not by copying files). VMs, networks and storage live in libvirt on the nodes,
not here. See [controller-backup.md](controller-backup.md) for backup, restore,
moving the controller and recovery without a backup.

## Upgrading

This is the one place for upgrade steps. To upgrade the controller, do one of
these:

- From the package repository: `sudo apt update && sudo apt upgrade` or
  `sudo dnf upgrade`.
- Install the newer package: `sudo apt-get install ./virticle_NEW.deb` or
  `sudo dnf upgrade ./virticle-NEW.rpm`. Your edits in
  `/etc/virticle/virticle.env` are kept.
- Rebuild the image and recreate the container.
- Replace the binary and restart it.

A package restarts a running service. Before a start applies a database schema
change, the controller copies the database to
`virticle.db.bak-<oldversion>-<timestamp>` in the data directory and logs the
file name (find it with `journalctl -u virticle`, look for `database schema`).
The copy is a consistent single file, the newest 3 are kept, and no copy is
made when the schema version does not change.

### From 0.2.0 to 0.3.0

Nodes need to be prepared again, because the node command has a new version
(protocol 19, it was 10). Until you do, a node shows "prepared by an older
version" and the features that need root on it are off: migration, backups,
downloads into a pool, and the network changes. VMs keep
running.

1. **Upgrade the controller** as above. A backup first is a good idea:
   `sudo virticle backup -o /root/backups/` (see
   [controller-backup.md](controller-backup.md)).
2. **Check the web address.** The new HTTPS default (self-signed, port 8443)
   applies to **fresh installs only**. A data directory that already has users
   and no stored web settings keeps serving plain HTTP on `127.0.0.1:8080`, as
   before. The **Web access** page says so. To reach the controller from other
   computers, choose a mode on that page. Settings you stored with 0.2.0 are
   kept.
3. **Run `sudo virticle setup`** if you want it to open the web port in the
   firewall. The package never does this by itself. You do not need it when
   the controller keeps listening on `127.0.0.1`. Run it again after you
   change the web port.
4. **Prepare each node again.** Either way works:
   - In the web UI: open the node, then **Settings > Prepare this node**.
   - In a shell: `virticle node prepare admin@host`. Add `--root` if the node
     is a Root node.

   Preparing replaces the node command and now also opens the migration ports
   (TCP `49152` to `49215`) in firewalld or ufw. A node that 0.2.0 prepared
   keeps its account `virticle`; new nodes use `virticle-node`. VMs, networks
   and pools are not touched. A running controller notices a prepared node
   within about 30 seconds.
5. **Open each node** and check that **Settings** no longer warns that the node
   is not prepared.

What you get without doing anything else: performance history (starts to fill
at once), alerts for network links, the network drift warning, notifications
(an alert webhook address from 0.2.0 becomes the destination **Webhook** under
**Settings > Alerts**), and the daily controller backup (off until you switch
it on under **Settings > Controller backup**). Existing VMs keep their firmware. A VM
keeps the kind of snapshot it already has (see
[Known limits](reference.md#known-limits)).

**Container.** The new image keeps a data volume from 0.2.0 (an
administrator, no stored web settings) answering where 0.2.0 did: HTTPS with a
self-signed certificate on port 8080 inside the container. Recreate the
container from the new image with the same `-p 127.0.0.1:8080:8080 -v ...:/data`
as before and nothing else changes. The **Web access** page says so. When you
choose a mode there, the controller moves to the port you set (8443 by default),
so publish that port too. A new volume starts on port 8443 as described under
[Container](#container). Tried with both images built from the tags `v0.2.0`
and this version.

### From 0.1.0

0.2.0 manages nodes in a new way, so **nodes prepared with 0.1.0 must be
prepared again**, as in step 4 above (use `--root` for a node that you gave
root). The root shell script of 0.1.0 is replaced by the node command; the
prepare removes the old script. There are now exactly two kinds of access,
**Service account** and **Root**: full sudo and "add a node without preparing
it" no longer exist, so such a node needs the same step. Then upgrade on to
0.3.0 as above.

### From the working name virt-proj

Replace the binary and start it with the same `-data` directory.

- `virt-proj.db` is renamed to `virticle.db` at start.
- The old `VIRT_JUMP_PASSWORD` and `VIRT_VIRTIO_WIN_PATTERN` names no longer
  work; use the `VIRTICLE_` names.
- Per-VM metadata in the old namespace is read and moved on the next write.

### Roll back

To go back to the previous version:

1. Stop the service: `sudo systemctl stop virticle`.
2. Restore the copy, with the service user as owner:
   `sudo cp -p /var/lib/virticle/virticle.db.bak-<old>-<time> /var/lib/virticle/virticle.db`
   and remove any `virticle.db-wal` and `virticle.db-shm` beside it.
3. Install the old package (`sudo apt-get install --allow-downgrades ./virticle_OLD.deb`,
   `sudo dnf downgrade ./virticle-OLD.rpm`), or put the old binary back.
4. Start it: `sudo systemctl start virticle`.

Changes made after the upgrade (users, nodes, tokens) are lost by the
restore. Do not start the old binary on the upgraded database without the
restore: it logs a warning, but an older version may not understand it.

A backup made before the upgrade does the same for the whole data directory:
`sudo virticle restore FILE` with the service stopped
([controller-backup.md](controller-backup.md#d-roll-back-an-upgrade)).

Rolling back the controller does not undo the node command. A node that was
prepared again keeps the new one and shows "prepared by an older version"
to the old controller; prepare it again with the old version.

## Uninstall

    sudo apt-get remove virticle      # keeps /var/lib/virticle and /etc/virticle
    sudo apt-get purge virticle       # also deletes both and the user virticle
    sudo dnf remove virticle          # keeps /var/lib/virticle and the user

Run `sudo virticle setup --undo` first to remove the firewall rule that
`virticle setup` added.

The data directory is never deleted by `remove`. Make a backup first if you may
want the controller again ([controller-backup.md](controller-backup.md)).

Uninstalling the controller does not change the nodes. The controller's public
key stays in their `authorized_keys`. To take the access away, run
`virticle node unprepare admin@host` for each node before you uninstall. It
removes the account, key, node command and sudo rule, and leaves packages,
VMs, pools and networks alone. See [install-node.md](install-node.md).
