# Virticle

A web front end for KVM/QEMU/libvirt hosts. It is one Go binary with the web UI
embedded. It installs nothing on the hosts ("nodes"): everything goes over SSH
to each node's libvirt socket, plus shell commands over the same SSH
connection where libvirt has no API (file copies, `qemu-img`, `df`).

Libvirt is the source of truth. VMs, networks, pools, volumes and snapshots
are read from libvirt every time, so objects made with `virsh` and objects
made in the UI look the same. The controller itself keeps only users,
sessions, API tokens, the node list, the task log and the audit log.

`ROADMAP.md` lists what is done, what is untested and what is open.

## Install

Packages (`.deb`, `.rpm` with a systemd service), a container image and a
plain binary are built by the release pipeline; no release has been published
yet. See `docs/install.md` for installing, first-run setup, where data lives,
upgrade and rollback, uninstall, and the table of supported platforms
(controller tested on Linux amd64; managed nodes tested on Debian 13 and Rocky
10). `virticle version` prints the version.

## Quick start

Requirements to build: Go (see `go.mod`) and Node.js.

    make build                 # builds the web UI, then bin/virticle
    ./bin/virticle -listen 127.0.0.1:8080 -data ./data

`make test` runs `go vet` and the unit tests. `go build` alone fails until the
web UI has been built once, because the UI is embedded from `web/dist`.

On the first visit the page asks for the administrator account (the setup
endpoint only works while no user exists). The password must have at least 10
characters.

### Adding a node

1. Start the controller once. It creates an SSH key pair in the data directory
   (`id_ed25519`, `id_ed25519.pub`).
2. In the UI open Nodes, Add node. The dialog shows the controller's public key.
   Append it to `~/.ssh/authorized_keys` of the SSH user on the node (default
   `root`). The user must be able to use the libvirt system socket
   (`/run/libvirt/libvirt-sock` or the other usual paths).
3. Enter the node's address and user. An optional jump host (`user@host`) can
   be given; its password is read from the environment variable
   `VIRTICLE_JUMP_PASSWORD` of the controller process.
4. Host keys are trusted on first use and stored in `known_hosts` in the data
   directory.

Node-to-node SSH trust is needed in addition for anything that moves files
between nodes: migration, and backups whose target pool is on another node.
These commands run on the source node and connect to the target node under
the node's `SSH user@<target node's host name>`, without a password:

1. On the source node, as that user: `ssh-keygen -t ed25519` (if it has no key).
2. `ssh-copy-id <user>@<target host name>`, then `ssh <user>@<target host name> true`
   once to accept the host key.
3. The target's host name must resolve from the source node.

The pre-flight checks of migration and the planning step of a cross-node backup
test this and report the fix.

Backups and the node-side file work need `qemu-img` and `tar` on the node.

## Flags

All flags of `bin/virticle` (output of `-help`):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | Address to serve the web UI and API on. |
| `-data` | `data` | Directory for the database and the controller's SSH key. |
| `-tls-cert`, `-tls-key` | empty | PEM certificate and key to serve HTTPS with. Both or neither. |
| `-insecure-http` | off | Serve plain HTTP on a non-loopback address. Logs a warning: passwords and sessions travel unencrypted. |
| `-trusted-proxy` | empty | Comma-separated IPs/CIDRs of reverse proxies whose `X-Forwarded-For`, `-Proto` and `-Host` headers are believed. Empty: those headers are ignored. With this set, a loopback or private `-listen` address serves plain HTTP without `-insecure-http`, because TLS ends at the proxy. See [Reverse proxy](#reverse-proxy). |
| `-no-scheduler` | off | Do not run scheduled snapshots and backups from this controller. Use it on all but one controller when several manage the same nodes. |
| `-alert-pool-warn` | `85` | Storage pool usage in percent that raises a warning alert. |
| `-alert-pool-critical` | `95` | Storage pool usage in percent that raises a critical alert. |
| `-alert-memory` | `90` | Host memory use in percent that raises an alert. |
| `-alert-cert-days` | `30` | Alert when the web UI certificate expires within this many days. |
| `-alert-webhook` | empty | `http(s)` URL that receives a JSON POST whenever an alert is raised or cleared. Empty: no notifications. |

Environment: `VIRTICLE_JUMP_PASSWORD` is the password for node jump hosts.
`VIRTICLE_VIRTIO_WIN_PATTERN` overrides the volume name pattern that counts as
the virtio-win ISO. The old names `VIRT_JUMP_PASSWORD` and
`VIRT_VIRTIO_WIN_PATTERN` still work and log a deprecation line at start.

## TLS

- Listening on a loopback address (`127.0.0.1`, `::1`, `localhost`): plain HTTP.
- Listening on any other address without `-tls-cert`: the controller generates
  a self-signed certificate (`tls-cert.pem`, `tls-key.pem` in the data
  directory) and logs its SHA-256 fingerprint. Delete the files to have a new
  one made. An alert is raised when the certificate is close to expiry.
- `-tls-cert`/`-tls-key` use your own certificate.
- `-insecure-http` on a non-loopback address serves plain HTTP anyway.
- With `-trusted-proxy`, a loopback or private address (RFC 1918, unique-local,
  link-local) serves plain HTTP with no warning. A public or wildcard address
  (`0.0.0.0`, `[::]`) still gets the generated certificate unless
  `-insecure-http` is given.

HTTPS responses carry `Strict-Transport-Security`, and session cookies are
`Secure` when the request came in over HTTPS.

## Reverse proxy

By default the controller believes only the connection itself: the client
address for the login rate limit and the audit log is the TCP peer, the
same-origin check for state-changing requests compares `Origin` with the
`Host` header, and `Secure` cookies and HSTS depend on whether the controller
itself terminates TLS. Behind a proxy that would all be wrong, so start the
controller with the proxy's address:

    ./bin/virticle -listen 127.0.0.1:8080 -data ./data -trusted-proxy 127.0.0.1

Rules:

- Only requests whose TCP peer is inside a `-trusted-proxy` range are looked at.
  From every other peer the `X-Forwarded-*` headers are ignored; a client that
  sends its own `X-Forwarded-For` does not change its rate-limit key or audit
  address.
- Client address: `X-Forwarded-For` is read from the right, skipping addresses
  that are themselves trusted proxies; the first other address is the client.
  An unparsable entry stops the walk at the address to its right. Without the
  header the proxy's own address is used.
- Scheme: the right-most `X-Forwarded-Proto` (`http` or `https`) decides the
  `Secure` cookie flag and HSTS.
- Host: the right-most `X-Forwarded-Host` is used for the `Origin` check, for
  the `connect-src` entry of the Content-Security-Policy (`wss://host` or
  `ws://host` according to the scheme) and for the WebSocket origin check of
  the consoles and the event stream. If your proxy passes the original `Host`
  header on, `X-Forwarded-Host` is not needed.
- The `Forwarded` header (RFC 7239) is not read.

Pass the headers from your proxy, and pass WebSocket upgrades. The consoles and
the live event stream use WebSockets. Volume and backup uploads stream
the request body, so remove body size limits and buffering for `/api/`.

Caddy (terminates TLS; passes `Host` and sets the `X-Forwarded-*` headers by default):

    vm.example.org {
        reverse_proxy 127.0.0.1:8080
    }

Caddy upgrades WebSockets and does not limit request bodies by default. Start
the controller with `-trusted-proxy 127.0.0.1`.

nginx:

    map $http_upgrade $connection_upgrade { default upgrade; '' close; }

    server {
        listen 443 ssl;
        server_name vm.example.org;
        ssl_certificate     /etc/ssl/vm.example.org.pem;
        ssl_certificate_key /etc/ssl/vm.example.org.key;

        client_max_body_size 0;          # volume and backup uploads
        proxy_request_buffering off;
        proxy_read_timeout 1h;           # long-lived console WebSockets

        location / {
            proxy_pass http://127.0.0.1:8080;
            proxy_http_version 1.1;
            proxy_set_header Host $host;
            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_set_header X-Forwarded-Host $host;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection $connection_upgrade;
        }
    }

The controller's `-listen` address must be reachable only by the proxy (loopback
or a firewalled private address). Anything else that can reach it and whose
address is inside `-trusted-proxy` can choose the client address that is
recorded. Do not trust a whole network such as `0.0.0.0/0` (the controller logs
a warning).

## Roles

Every API route has a required role (`routeLevels` in `internal/api/access.go`);
a route that is not listed requires `admin`.

| Role | Can |
| --- | --- |
| `viewer` | Read: VMs, stats, tasks, alerts, snapshots, backups listings, pools, networks. No console. |
| `operator` | Everything a viewer can, plus VM, storage, network, snapshot, backup, migration and console actions. |
| `admin` | Everything an operator can, plus nodes (add, remove, evacuate, maintenance), users, and the audit log. |

Console WebSockets count as control and need `operator`. Account settings
(password, tokens) need a web session, not a token. Failed sign-ins are rate
limited per user and address, per address and per user, with a lock that grows
with further failures; the counters are in memory.

## API tokens

Create a token in the UI under Account. The secret (`virticle_...`) is shown once.
A token has a name, a role (the user's role or lower) and an optional expiry
in days. Use it as a bearer token:

    curl -H "Authorization: Bearer virticle_xxxxxxxx" https://vm.example.org/api/vms

    curl -X POST -H "Authorization: Bearer virticle_xxxxxxxx" -H 'Content-Type: application/json' \
         -d '{"action":"start"}' https://vm.example.org/api/nodes/1/vms/<uuid>/action

Requests with a bearer token are not subject to the browser same-origin check.
They are written to the audit log like other state-changing requests.

## Where state lives

- **libvirt on the nodes**: domains, networks, storage pools and volumes,
  snapshots. App-specific per-VM data (template mark, tags, snapshot schedule,
  backup schedule) is stored as children of one `<metadata>` element in the
  domain XML, namespace `https://virticle.invalid/ns/1`, so `virsh dumpxml`
  shows it and clones do not inherit it. VMs configured by versions before the
  rename carry the same data in namespace `https://virt-proj.invalid/ns/1`
  (prefix `vp`); it is read as long as no new element exists, and replaced by
  the new one the next time the app writes metadata for that VM.
- **The data directory** (`-data`):
  - `virticle.db`: SQLite database with users, password hashes, sessions, API
    token hashes, the node list, the task log and the audit log.
  - `virticle.db.bak-<oldversion>-<timestamp>`: copies made before a schema
    upgrade (newest 3 kept); see `docs/install.md` for rollback.
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: trusted node host keys.
  - `tls-cert.pem`, `tls-key.pem`: only if a certificate was generated.
  - `instance-id`: a random id of this controller, put into the description of
    the snapshots it schedules.
- **In memory only**: live stats (about 10 minutes), alert state, rate-limit
  counters.

## Scheduled snapshots and backups

A schedule is stored in the VM's metadata and run by the controller; "last run"
is read from libvirt (the newest scheduled snapshot) or from the target pool
(the newest backup of that VM), so nothing is kept in the controller's
database. Times are UTC. The scheduler checks every 20 seconds, retries a
failed run after 5 minutes, and records failures in the task log under the user
`scheduler`; a failed scheduled job raises an alert until a later run succeeds.

- Snapshots: interval or daily, keep newest N, name `<prefix>-<timestamp>`.
  Shut-off VMs are skipped (nothing changes on them). Only snapshots with the
  scheduler's description and name pattern are ever pruned.
- Backups (Backups tab of a VM, `.../backup-schedule`): interval or daily,
  target node and pool, keep last N (1 to 1000). Shut-off VMs are backed up too.
  A VM with a backup already running is skipped. Retention is the same
  pruning a manual backup does.

Two controllers pointed at the same nodes would both run every schedule. The
snapshot guard is: each controller re-reads the newest scheduled snapshot right
before creating one, stamps its id into the snapshot description
(`Scheduled snapshot (virticle) [controller <id>]`; the old
`Scheduled snapshot (virt-proj)` text is still recognised), and raises the alert
"Another controller is running schedules on node X" when it sees recent
scheduled snapshots stamped by a different id. This narrows but does not remove
the race (two controllers can still act within the same few seconds), and a
controller from before this change does not recognise stamped snapshots as its
own, so run `-no-scheduler` on all controllers but one. Scheduled backups have
no such guard.

## Backup format

A backup is a directory `<vm>-<UTC timestamp>.vmbackup` in a directory-backed
pool (type `dir`, `fs` or `netfs`) of any node:

- `manifest.json`: format version, VM name and UUID, creation time, source node,
  whether the VM was running, the disks (target, file, format, sizes, optional
  SHA-256) and notes.
- `domain.xml`: the inactive domain definition.
- one `<target>.qcow2` per writable disk, flattened (no backing chain).
- `nvram.fd` for UEFI guests.

The manifest is written last and the directory renamed into place, so a backup
either exists completely or not at all. A running VM is backed up with libvirt's
backup API (full, push mode, no checkpoint or snapshot is left in the domain);
a shut-off VM with `qemu-img convert`. Optional: SHA-256 sums (checked on
restore) and freezing guest file systems through the guest agent. A backup can
be restored as a new VM or as the original, exported as one `.vmexport.tar` file
and imported again. Hidden work directories `.virticle-*` (`.virt-proj-*` from before the
rename) in a pool are leftovers of an interrupted run.

## Known limitations

- Snapshots are not part of a backup, and neither is TPM state; a restored VM
  starts with an empty TPM.
- Only file-based, non-shareable disks can be backed up.
- Internal snapshots (the default) pause the guest while memory is saved and
  block live migration. External snapshots keep the guest running but could not
  be deleted on the Debian/AppArmor lab nodes (an AppArmor profile problem).
- Windows guest sysprep, the guest-agent path for Windows, PCI/USB passthrough,
  NFS/LVM/iSCSI pool forms, multi-cell NUMA and BIOS-to-UEFI switching are
  untested or not offered; see `ROADMAP.md`.
- Migration streams are not encrypted by the controller (open item).
- Live stats are kept in memory for about 10 minutes only.
- One controller per set of nodes is the supported setup (see above).
- Alert state, rate-limit counters and the scheduler's retry pacing are in
  memory and start empty after a restart.
- No high availability or shared-storage features; no scheduled replication.

## Upgrading from the working name virt-proj

Replace the binary (`bin/virt-proj` is now `bin/virticle`) and start it with
the same `-data` directory; nothing else is required.

- `virt-proj.db` in the data directory is renamed to `virticle.db` at start
  (with its `-wal` and `-shm` files). If both exist, `virticle.db` is used and
  a warning is logged.
- Rename `VIRT_JUMP_PASSWORD` to `VIRTICLE_JUMP_PASSWORD` when convenient; the
  old name keeps working and is logged as deprecated.
- Per-VM data is read from the old libvirt metadata namespace and moved to the
  new one when the app next writes it for that VM. No VM definition is
  rewritten at start. Backups made by the old name restore as before.
- Encrypted-migration marker files (`.virt-proj-migration.json`) on nodes are
  read and replaced by `.virticle-migration.json` on the next setup or removal.
- Browser settings (theme, console click, tree state) are carried over on
  the first visit. The session cookie keeps its name, so nobody is signed out.
