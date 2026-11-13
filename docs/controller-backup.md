# Backing up and recovering the controller

The VMs, storage and networks live on the nodes, in libvirt. The controller
itself holds the things that make it the controller. Losing them is not the loss
of any VM, but every node must then be set up again and every user created
again. A backup of the controller avoids that.

- [What is in a backup](#what-is-in-a-backup)
- [Make a backup](#make-a-backup)
- [Restore on the same machine](#a-restore-on-the-same-machine)
- [Move the controller to a new machine](#b-move-the-controller-to-a-new-machine)
- [There is no backup and the controller is lost](#c-there-is-no-backup-and-the-controller-is-lost)
- [Roll back an upgrade](#d-roll-back-an-upgrade)

## What is in a backup

One file, `virticle-backup-<host>-<UTC time>.tar.gz`, with:

| Content | Why it matters |
| --- | --- |
| The database (`virticle.db`), copied while the controller runs | Users, password hashes, API token hashes, the node list, web access settings, task and audit logs |
| `id_ed25519`, `id_ed25519.pub` | The SSH key. Every node trusts it, so losing it means preparing each node again |
| `known_hosts` | The nodes' host keys |
| `migration-ca/` | The certificate authority of encrypted migration. Nodes hold certificates it signed |
| `tls-cert.pem`, `tls-key.pem`, `web-own/`, `acme/` | The web certificates and the ACME account |
| `instance-id` | Marks the scheduled snapshots of this controller |
| `secret.key`, the key that encrypts the stored secrets (two-step seeds, sign-in passwords, notification addresses), if it exists | |
| `manifest.json` | Format version, program version, time, host name, the database schema version, every file with its SHA-256 |

Not included: `metrics.db` (rebuilt by itself), logs, the copies made before an
upgrade, earlier backups.

**The file contains private keys. Keep it as safe as a root password.** Anyone
who has it can sign in to your nodes. Encrypt it if it leaves the machine.

## Make a backup

On the controller's machine:

    sudo virticle backup -o /root/backups/

It works while the controller runs, writes one file with mode 0600 and prints
one line with its name and size. `-o` takes a directory or a file name; without
it the file goes into the current directory. The data directory is
`/var/lib/virticle` on a package install; use `-data DIR` for another one.

To encrypt it, add `--encrypt`. The command asks for a passphrase twice (at
least 10 characters). In a script use `--passphrase-file FILE`, which holds the
passphrase on its first line. The result is an [age](https://age-encryption.org)
file named `...tar.gz.age`; the passphrase cannot be recovered, so write it down
somewhere other than the backup.

In the web UI: **Settings > Backup > Download backup**. It asks for your
password again, and for a passphrase if you want the file encrypted.

For a copy that survives losing the machine, run the command from cron and copy
the file elsewhere, for example:

    30 2 * * *  root  virticle backup -o /var/backups/virticle/ --passphrase-file /root/.virticle-backup-pass

Remove old files yourself; this command keeps all of them.

### A daily copy on the same server

**Settings > Backup > Keep a daily backup on this server** (off by default)
writes a backup to `<data directory>/backups/` at 03:30 server time (plus up to
20 minutes) and keeps the newest 7. These files are not encrypted, because a
passphrase kept next to the data would protect nothing; only the controller's
account can read them. They protect against a bad upgrade or a mistake, **not
against losing the machine**. A copy that failed raises an alert on the Alerts
page.

## (a) Restore on the same machine

Use this when the data directory is damaged or changed by mistake.

1. Stop the controller: `sudo systemctl stop virticle`.
2. Restore: `sudo virticle restore /root/backups/virticle-backup-....tar.gz`
   (an encrypted file asks for the passphrase, or use `--passphrase-file`).
3. Start it: `sudo systemctl start virticle`.

`restore` checks the whole file first, and changes nothing if anything is wrong:

- It refuses while the controller runs on that directory.
- It reads the manifest and checks every SHA-256 before it touches the data
  directory. It rejects entries that are not plain files, paths that leave the
  directory and files the manifest does not list.
- It refuses a backup made by a newer Virticle (a newer database schema). A
  backup from an older one is fine; the normal upgrade of the database runs at
  the next start.
- It moves an existing data directory aside to
  `<dir>.before-restore-<time>` and never overwrites it. Delete it when you are
  sure you do not need it.
- It gives the files the owner of the existing data directory, or the account
  `virticle`, and modes 0600 (0644 for public keys and certificates).

The nodes need nothing: they trust the restored SSH key and their migration
certificates were signed by the restored authority. Changes made after the
backup (users, nodes, tokens, history) are gone; add them again.

## (b) Move the controller to a new machine

1. On the old machine: `sudo virticle backup --encrypt -o /root/` and copy the
   file to the new machine. (If the old machine is gone, use your latest backup.)
2. On the new machine, install the package ([install.md](install.md)). Do not
   create an administrator.
3. Stop the service: `sudo systemctl stop virticle`.
4. Restore: `sudo virticle restore virticle-backup-....tar.gz.age`.
5. Set up the firewall and the web address: `sudo virticle setup`.
6. Start it: `sudo systemctl start virticle`. Shut the old controller down, or
   start it with `-no-scheduler`: two controllers on the same nodes would both
   run the schedules.

What changes when the address changes:

- **The browser address** is the new machine's. Bookmarks and reverse proxy
  entries must change.
- **Web certificates** are the old machine's. A self-signed certificate covers
  the old names; make a new one on **Settings > Web access** (or use an own or
  ACME certificate for the new name). Browsers will ask you to trust it again.
- **The nodes keep working.** They trust the key, not the address. The one
  exception is a node whose key was restricted to the old address with
  `virticle node prepare --from ADDRESS`: it refuses the new address. Prepare
  that node again with `--from NEW-ADDRESS`.
- API tokens, users and node list are the same.

## (c) There is no backup and the controller is lost

What survives, because it lives on the nodes: **all VMs, their disks, storage
pools, networks and snapshots**. Also **schedules and tags**: they are stored in
the VMs' metadata and come back when the new controller sees the VMs.

What is gone: users and password hashes, API tokens, the node list, task history
and the audit log, web access settings and the migration authority.

To adopt the nodes with a fresh controller:

1. Install the package on a machine that can reach the nodes
   ([install.md](install.md)) and open the web address it prints.
2. Create the administrator. Set the web address on **Settings > Web access** if
   you want something other than the default.
3. Recreate the other users on **Settings > Users**.
4. For each node, open **Nodes > Add node** and enter its address and the
   administrator login (root, or a user that may use sudo). Run **Check** first
   if you want to see what would happen. Preparing a node that is already
   prepared is safe: it changes only what differs.
5. The check stops with **Other controller: This node was managed by another
   Virticle controller. Take it over?** Press **Take over node**. This:
   - installs the new controller's SSH key and **removes the old controller's
     key** from the node, so a copy of the old controller, if it still exists,
     can no longer manage it;
   - installs the node command again and re-checks the sudo rule;
   - replaces the old controller's migration certificates with the new
     controller's (the old files stay next to them as `*.virticle-orig`).

   On the command line the same answer is `virticle node prepare --take-over
   admin@host`. VMs are not touched.
6. Wait for the node to come online. The new controller trusts the node's host
   key on first use.
7. Take **Settings > Backup** into use now, so this does not happen twice.

Things to expect:

- Scheduled snapshots of the old controller carry its instance id. Until they
  are old enough (26 hours for daily schedules, three intervals for the others)
  the new controller sees them as made by another controller, shows an alert
  "Another controller is running schedules on node ...", and leaves those VMs'
  schedules to it. After that window it takes over by itself. Nothing is lost
  in between; the schedules simply pause.
- Snapshots made by the old controller's schedules are recognised as scheduler
  snapshots, so the new controller counts and prunes them like its own.

## (d) Roll back an upgrade

Before a start applies a database schema change, the controller copies the
database to `virticle.db.bak-<old version>-<time>` in the data directory (the
newest 3 are kept). To go back to the previous version, follow
[Upgrade and rollback](install.md#upgrade-and-rollback): stop the service, put
that copy back as `virticle.db`, install the old version, start it.

If you have a backup made before the upgrade (the daily copy on the server
counts), `virticle restore` does the same for the whole data directory in one
step, as in (a), and checks the schema for you. Changes made after the copy or
backup are lost.
