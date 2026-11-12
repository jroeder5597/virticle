# Installing Virticle

Virticle is one controller program. It needs nothing on the machines it
manages (the "nodes") except SSH access and libvirt; see `install-node.md` for
preparing a node and `web-tls.md` for HTTPS and reverse proxies.

Releases are at <https://github.com/jroeder5597/virticle/releases>. Below,
`RELEASES_URL` is `https://github.com/jroeder5597/virticle/releases/download`.

## Supported platforms

| Role | Platform | Status |
| --- | --- | --- |
| Controller | Linux amd64 | Tested |
| Controller | Linux arm64 | Built, not tested |
| Controller | Other operating systems or architectures | Not built |
| Managed node | Debian 13 | Tested |
| Managed node | Rocky Linux 10 | Tested |
| Managed node | Any other distribution with libvirt and SSH | Not tested |

Packages: `.deb` for Debian and Ubuntu, `.rpm` for RHEL-like systems, both
with a systemd service. The `.deb` and `.rpm` have not been installed on every
release of those distributions; the install steps were exercised on a single
Debian-like system. A container image and a plain `.tar.gz` are also built.
The controller is pure Go (no CGO, no C libraries), so the binary is static.

## Package (Debian, Ubuntu, RHEL-like)

With the install script (it downloads the package, checks its SHA-256 against
`checksums.txt` and calls `apt-get` or `dnf`; read it first, `--dry-run` shows
what it would do):

    VIRTICLE_RELEASES_URL=RELEASES_URL sh scripts/install.sh --version 1.2.3

Or by hand:

    # Debian family
    sudo apt-get install ./virticle_1.2.3_amd64.deb
    # RHEL family
    sudo dnf install ./virticle-1.2.3-1.x86_64.rpm

The package installs:

- `/usr/bin/virticle`
- `/usr/lib/systemd/system/virticle.service`
- `/etc/virticle/virticle.env` (configuration; upgrades keep your edits)
- `/usr/share/man/man1/virticle.1.gz` and documentation in
  `/usr/share/doc/virticle/`

and creates the system user and group `virticle`. The service listens on
`127.0.0.1:8080` only.

- Debian family: the service is enabled and started by the installation.
- RHEL family: the service is not started or enabled by the installation
  (distribution convention). Start it with `sudo systemctl enable --now virticle`.

Check it: `systemctl status virticle`, `journalctl -u virticle`,
`virticle version`.

### Configuration

Edit `/etc/virticle/virticle.env` and run `sudo systemctl restart virticle`.
`VIRTICLE_ARGS` holds extra flags (`virticle -help` lists them; `reference.md`
describes each), for example `-listen`, `-tls-cert`, `-tls-key`,
`-trusted-proxy`. `VIRTICLE_JUMP_PASSWORD` is the password for node jump hosts.
The file is readable by root only.

Listening on a port below 1024 (80, 443) needs a capability the service does
not have. Add a drop-in with `sudo systemctl edit virticle`:

    [Service]
    CapabilityBoundingSet=CAP_NET_BIND_SERVICE
    AmbientCapabilities=CAP_NET_BIND_SERVICE

The service is sandboxed (`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
no new privileges, no capabilities, system call filter) and can write only to
`/var/lib/virticle`. A certificate given with `-tls-cert` must be readable by
the user `virticle`, for example under `/etc/virticle/`.

## Container

    docker build -t virticle .      # in an LXC: podman build --network=host
    docker run -d --name virticle -p 127.0.0.1:8080:8080 -v virticle-data:/data virticle

or `docker compose up -d --build` with the `compose.yaml` of this repository.
The image is `gcr.io/distroless/static` based, runs as the non-root user
65532, keeps all state in the volume `/data` and exposes port 8080. Because the
container listens on a non-loopback address, the controller serves HTTPS with
a self-signed certificate it generates in `/data`; open `https://127.0.0.1:8080`.
Behind a reverse proxy that ends TLS, pass `-trusted-proxy <proxy address>`
(see `web-tls.md`). Environment variables such as `VIRTICLE_JUMP_PASSWORD` are
passed with `-e` or `env_file`. No registry has been chosen, so no image is
published.

## Binary

Download `virticle_VERSION_linux_amd64.tar.gz` (or `arm64`) from
`RELEASES_URL`, check it against `checksums.txt` (`sha256sum -c --ignore-missing
checksums.txt`), unpack it and run:

    ./virticle -listen 127.0.0.1:8080 -data ./data

To build from source: `make build` (needs Go and Node.js), result `bin/virticle`.
`make dist` cross-compiles linux/amd64 and linux/arm64 into `dist/`.

## First run

Open the page (`http://127.0.0.1:8080` for the package, or the address you
set). The first visit asks you to create the administrator account; the
password needs at least 10 characters. After that, add nodes: prepare each
node from the Nodes page or with `virticle node prepare` (`install-node.md`
describes the preparation).

## Where data lives

| Install | Data directory |
| --- | --- |
| Package | `/var/lib/virticle` (owner `virticle`, mode 0700) |
| Container | the volume mounted at `/data` |
| Binary | the directory given with `-data` (default `./data`) |

It holds `virticle.db` (users, password hashes, API token hashes, node list,
task and audit logs), `id_ed25519` and `id_ed25519.pub` (the controller's SSH
key for the nodes), `known_hosts`, `instance-id` and, when generated,
`tls-cert.pem` and `tls-key.pem`. Back this directory up (stop the service
first, or copy while it is idle). VMs, networks and storage live in libvirt on
the nodes, not here.

## Upgrade and rollback

Upgrade: install the newer package (`apt-get install ./virticle_NEW.deb`,
`dnf upgrade ./virticle-NEW.rpm`), or pull the new image and recreate the
container, or replace the binary and restart. Edits in
`/etc/virticle/virticle.env` are kept. A running service is restarted by the
package.

Before a start applies a database schema change, the controller copies the
database to `virticle.db.bak-<oldversion>-<timestamp>` in the data directory
(a consistent single-file copy) and logs the file name. The newest 3 copies are
kept. No copy is made when the schema version does not change. Check
`journalctl -u virticle` for the line starting with `database schema`.

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

## Uninstall

    sudo apt-get remove virticle      # keeps /var/lib/virticle and /etc/virticle
    sudo apt-get purge virticle       # also deletes both and the user virticle
    sudo dnf remove virticle          # keeps /var/lib/virticle and the user

The data directory is never deleted by `remove`. After uninstalling,
authorized_keys entries of the controller's public key remain on the nodes;
remove them there if you no longer want the controller to have access.
