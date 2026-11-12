# Installing Virticle

Virticle is one controller program. It needs nothing on the machines it
manages (the "nodes") except SSH access and libvirt. To get a node ready, see
[install-node.md](install-node.md). For HTTPS and reverse proxies, see
[web-tls.md](web-tls.md).

On this page:

- [Package](#package-debian-ubuntu-rhel-like), [container](#container) or [binary](#binary)
- [First run](#first-run)
- [Upgrading from 0.1.0](#upgrading-from-010)
- [Upgrade and rollback](#upgrade-and-rollback)
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
| Node | Any other distribution with libvirt and SSH | Not tested |

| Install type | Notes |
| --- | --- |
| `.deb` (Debian, Ubuntu) and `.rpm` (RHEL family) | Include a systemd service. Not yet installed on a clean machine in testing. Not signed: check downloads against `checksums.txt`. |
| `.tar.gz` | The plain binary. |
| Container | Built from the source tree. No image is published. |

The controller is pure Go (no CGO, no C libraries), so the binary is static.

## Package (Debian, Ubuntu, RHEL-like)

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
| Manual and documentation | `/usr/share/man/man1/virticle.1.gz`, `/usr/share/doc/virticle/` |
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

Check it with `systemctl status virticle`, `journalctl -u virticle` and
`virticle version`.

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
no new privileges, only the right to bind low ports, system call filter). It can write only to
`/var/lib/virticle`. A certificate file named on the **Web access** page must be readable by
the user `virticle`, for example under `/etc/virticle/`.

## Container

    docker build -t virticle .      # in an LXC: podman build --network=host
    docker run -d --name virticle -p 127.0.0.1:8443:8443 -v virticle-data:/data virticle

or `docker compose up -d --build` with the `compose.yaml` of this repository.

- The image is based on `gcr.io/distroless/static` and runs as the non-root
  user 65532.
- All state is in the volume `/data`. Port 8443 is exposed.
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

1. Open the page: `https://<address>:8443`, the address in the journal line
   `open ... to create the administrator`.
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
| `virticle.db` | Users, password hashes, API token hashes, node list, task and audit logs |
| `id_ed25519`, `id_ed25519.pub` | The controller's SSH key for the nodes |
| `known_hosts` | Host keys of the nodes |
| `instance-id` | Identifier of this controller |
| `tls-cert.pem`, `tls-key.pem` | The certificate, when the controller made one |

Back this directory up. Stop the service first, or copy while it is idle. VMs,
networks and storage live in libvirt on the nodes, not here.

## Upgrading from 0.1.0

Version 0.2.0 manages nodes in a new way. **Nodes prepared with 0.1.0 must be
prepared again.** Until you do, the node shows "prepared by an older version".
The features that need root on the node, such as migration, backups and
downloads into a pool, are off for it.

What changed on the nodes:

- The root shell script from 0.1.0 is replaced by the node command
  (`virticle-node`). It is a small program that is run on demand. No service
  runs on a node.
- There are now exactly two kinds of access: **Service account** and **Root**.
  Full sudo and "add a node without preparing it" no longer exist.
- Any two nodes can exchange files, including a Service account node with a
  Root node.

Steps:

1. Install the new controller package, or replace the binary, and restart the
   controller. See [Upgrade and rollback](#upgrade-and-rollback).
2. For each node, prepare it again. Use either way:
   - In the web UI: open the node, then **Settings > Prepare this node**.
   - In a shell: `virticle node prepare admin@host`. Add `--root` if the node
     is a Root node.
3. Open the node in the web UI. **Settings** no longer warns that the node is
   not prepared.

A node that you added with full sudo, or as root without preparing it, needs
the same step. Nodes that 0.2.0 prepared keep their account `virticle` when you
prepare them again; no second account is made. Newly prepared nodes use the
account `virticle-node`. A running controller notices a newly prepared node within about
30 seconds. Preparing again replaces the node command and removes the old
script. VMs, networks and pools on the node are not touched.

## Upgrade and rollback

To upgrade, do one of these:

- Install the newer package: `sudo apt-get install ./virticle_NEW.deb` or
  `sudo dnf upgrade ./virticle-NEW.rpm`. Your edits in
  `/etc/virticle/virticle.env` are kept, and the package restarts a running
  service.
- Rebuild the image and recreate the container.
- Replace the binary and restart it.

Before a start applies a database schema change, the controller copies the
database to `virticle.db.bak-<oldversion>-<timestamp>` in the data directory
and logs the file name.

- The copy is a consistent single file.
- The newest 3 copies are kept.
- No copy is made when the schema version does not change.
- To find the line, run `journalctl -u virticle` and look for `database schema`.

Rollback to the previous version:

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

Rolling back the controller does not bring back the shell script that
0.1.0 used on the nodes. Preparing a node again with 0.2.0 removes that script.

## Uninstall

    sudo apt-get remove virticle      # keeps /var/lib/virticle and /etc/virticle
    sudo apt-get purge virticle       # also deletes both and the user virticle
    sudo dnf remove virticle          # keeps /var/lib/virticle and the user

The data directory is never deleted by `remove`.

Uninstalling the controller does not change the nodes. The controller's public
key stays in their `authorized_keys`. To take the access away, run
`virticle node unprepare admin@host` for each node before you uninstall. It
removes the account, key, node command and sudo rule, and leaves packages,
VMs, pools and networks alone. See [install-node.md](install-node.md).
