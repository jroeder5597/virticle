# Reference

## Commands

| Command | Does |
| --- | --- |
| `virticle [flags]` | Runs the controller. |
| `virticle version` | Prints version, commit and build date. |
| `virticle node prepare\|unprepare` | Prepares a host as a node, or undoes it. See [install-node.md](install-node.md). |
| `virticle web show\|set\|reset` | Reads or changes the stored web access settings. See [web-tls.md](web-tls.md). |

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | Address of the web UI and API. |
| `-data` | `data` | Directory for the database and the controller's SSH key. |
| `-tls-cert`, `-tls-key` | empty | PEM certificate and key for HTTPS. Both or neither. |
| `-insecure-http` | off | Plain HTTP on a non-loopback address. Sign-ins travel unencrypted. |
| `-trusted-proxy` | empty | IPs or CIDRs of reverse proxies whose `X-Forwarded-*` headers are believed. |
| `-no-scheduler` | off | Do not run scheduled snapshots and backups from this controller. |
| `-alert-pool-warn` | `85` | Pool usage in percent that raises a warning. |
| `-alert-pool-critical` | `95` | Pool usage in percent that raises a critical alert. |
| `-alert-memory` | `90` | Host memory use in percent that raises an alert. |
| `-alert-cert-days` | `30` | Alert when the web certificate expires within this many days. |
| `-alert-webhook` | empty | URL that receives a JSON POST when an alert is raised or cleared. |

The web flags (`-listen`, `-tls-cert`, `-tls-key`, `-insecure-http`,
`-trusted-proxy`) override what the **Web access** page stores and make that
page read-only. Leave them out to manage web access from the UI.

## Environment

| Variable | Meaning |
| --- | --- |
| `VIRTICLE_LISTEN`, `VIRTICLE_TLS_CERT`, `VIRTICLE_TLS_KEY`, `VIRTICLE_INSECURE_HTTP`, `VIRTICLE_TRUSTED_PROXY` | Defaults for the flags of the same name. |
| `VIRTICLE_JUMP_PASSWORD` | Password for node jump hosts. |
| `VIRTICLE_VIRTIO_WIN_PATTERN` | Volume name pattern that counts as the virtio-win ISO. |

The package reads these from `/etc/virticle/virticle.env`.

## Roles

| Role | Can |
| --- | --- |
| `viewer` | Read everything except the audit log. No console. |
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

## Where state lives

- **libvirt on the nodes**: VMs, networks, pools, volumes, snapshots. Per-VM
  app data (template mark, tags, schedules, CPU model marker) sits in one
  `<metadata>` element of the domain XML, namespace
  `https://virticle.invalid/ns/1`.
- **The data directory** (`-data`):
  - `virticle.db`: users, password hashes, sessions, token hashes, node list,
    web access settings, task log, audit log.
  - `virticle.db.bak-<version>-<time>`: copy made before a schema upgrade
    (newest 3 kept).
  - `id_ed25519`, `id_ed25519.pub`: the controller's SSH key.
  - `known_hosts`: node host keys, trusted on first use.
  - `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/`: certificates.
  - `instance-id`: stamped into scheduled snapshots.
- **Memory only**: live stats (about 10 minutes), alert state, rate-limit
  counters.

## New VM defaults

- **CPU**: the newest model every online node can run (`custom`, exact match),
  so the VM can live-migrate anywhere in the cluster. `host-model` and
  `host-passthrough` can be chosen instead.
- **Clock** (Linux): `rtc` catch-up, `pit` delay, `hpet` off, `kvmclock` on.
  The guest clock is synced after resume, snapshot revert and live migration
  when the guest agent runs.
- **Serial console**: optional at creation; add or remove later under Hardware.

## Schedules

- Stored in the VM's metadata, run by the controller, times in UTC.
- **Snapshots**: interval or daily, keep newest N. Shut-off VMs are skipped.
  Only snapshots the scheduler made are pruned.
- **Backups**: interval or daily, target node and pool, keep last N.
- A failed run is retried after 5 minutes and raises an alert.
- Run one controller per set of nodes. With several, start all but one with
  `-no-scheduler`.

## Backup format

A directory `<vm>-<UTC timestamp>.vmbackup` in a directory-backed pool:

- `manifest.json`: VM name and UUID, time, source node, disks, optional SHA-256.
- `domain.xml`: the VM definition.
- `<target>.qcow2`: one flattened image per writable disk.
- `nvram.fd`: for UEFI guests.

A running VM is backed up with libvirt's backup API and keeps running. The
directory is renamed into place last, so a backup is complete or absent. A
backup restores as a new VM or as the original, and exports to one
`.vmexport.tar`.

## Node privilege levels

| Level | How | Limits |
| --- | --- | --- |
| Helper | `virticle node prepare` (default) | Backups are disabled. Image download and import, disk move and clone-with-sysprep work through typed helper verbs (paths inside the storage pools only). Migration works between two helper nodes (files go node to node, live migration is coordinated by the controller); a helper node and a root/sudo node can migrate only VMs that need no file copy. |
| Broad sudo | `virticle node prepare --broad-sudo`, or an account with passwordless sudo | None. |
| Root | SSH as root | None. Discouraged. |

## Known limits

- Debian and Rocky nodes cannot migrate VMs to each other (emulator path and
  machine type differ).
- Snapshots and TPM state are not part of a backup.
- Only file-based, non-shareable disks can be backed up.
- Internal snapshots pause the guest while memory is saved and block live
  migration.
- External snapshots could not be deleted on Debian nodes (AppArmor).
- No high availability, no shared-storage features, no replication.
- Not verified end to end: ACME issuance, the systemd service, arm64. See
  `ROADMAP.md`.

## Upgrading from the working name virt-proj

Replace the binary and start it with the same `-data` directory.

- `virt-proj.db` is renamed to `virticle.db` at start.
- `VIRT_JUMP_PASSWORD` and `VIRT_VIRTIO_WIN_PATTERN` still work and log a
  deprecation line.
- Per-VM metadata in the old namespace is read and moved on the next write.
