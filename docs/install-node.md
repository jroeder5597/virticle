# Preparing a node

A node is a Linux host with libvirt and QEMU that the controller reaches over SSH. No virticle service runs on it.
A node must be prepared before it is added: `virticle node prepare` (or **Prepare this node** in the Add node dialog)
makes a host ready, and this page says exactly what it does so you can do the same by hand.

## The two ways to reach a node

Both use the same program, **the node command** (`virticle-node`), installed by prepare at
`/usr/local/libexec/virticle-node`. The controller starts it over SSH when something must be done as root; it exits when
it is done.

| Access | The controller signs in as | The node command runs | Use |
| --- | --- | --- | --- |
| **Service account** | `virticle`, a user without a password | as root, through one sudo rule that allows this program and nothing else | Recommended. |
| **Root** | `root` | directly | When you do not want an extra account: `virticle node prepare --root`. |

There is no other way. A login that has general passwordless sudo but no node command is not prepared.

## What prepare does

![Prepare node dialog](img/prepare-node.png)

It signs in with an **administrator credential that you give for this one run** (your SSH agent or a key file with
the CLI; a password or pasted private key in the web dialog). The credential is used in memory only. It is not stored, not
logged, not written to the audit log or the task record. It is separate from the controller's own key.

1. **Checks** the host (read-only; `--check` stops here): CPU virtualisation flags (vmx or svm), `/dev/kvm`, nested
   virtualisation (information, when the host is itself a VM), IOMMU on or off (information, for device passthrough),
   distribution and version against the tested matrix, architecture, free space under `/var/lib/libvirt/images`,
   SELinux or AppArmor, time sync, whether libvirt is installed (and its version), whether another controller runs
   snapshot schedules there, and how many VMs exist ("Existing VMs are left alone"). It never reboots the host; if a
   reboot or kernel parameter is needed (IOMMU), a row says so.
2. **Installs packages** (the lists below), enables libvirt, starts the `default` network and defines the `default`
   pool when they are missing. Existing networks and pools are not changed.
3. **Service account only: creates the account** (default `virticle`, a system user, home `/var/lib/virticle`, shell
   `/bin/sh`, no password, groups `libvirt` and `kvm`).
4. **Installs the controller's public key** for that account (or for root) with restrictions, **installs the node
   command**, and for a service account **the sudo rule**.
5. **Creates the node key** of the account (for copies between nodes, see below), then **tests the connection** with
   the same steps as Test connection.

What is on the node afterwards:

| Item | Where | Service account | Root |
| --- | --- | --- | --- |
| Account | `virticle`, comment "virticle service account" | created | none (root) |
| Controller's key | `~/.ssh/authorized_keys` of the account, with restrictions (see below) | yes | yes |
| Sudo rule | `/etc/sudoers.d/virticle`: the account may run the node command as root | yes | no |
| Node command | `/usr/local/libexec/virticle-node`, `root:root 0755` | yes | yes |
| Node key | `~/.ssh/virticle_peer_ed25519`, the account's own key for copies between nodes | yes | yes |

The sudoers file names one account, so a host has one service account: preparing it for another account replaces the
rule. Running prepare again changes nothing and says so.

- **Upgrading.** When the controller says that a node was prepared by an older version (the node command there reports
  another protocol version, or is still the old shell script), run prepare again. It replaces the node command and
  removes the old script.
- **Unprepare.** `virticle node unprepare admin@host` removes the account, key, node command and sudo rule and leaves
  packages, VMs, pools and networks alone. It removes an account only when prepare created it (the account carries the
  comment "virticle service account"); for an existing account it removes just the key and the rule, and the group
  memberships stay. It ends the account's processes first, so remove the node in the controller before you run it.

```
virticle node prepare [--check] [--user virticle] [--use-existing-user[=NAME]] [--confirm] [--root]
                      [--from ADDR] [--extras guestfs,virtiofs] [--jump user@host]
                      [--identity FILE] [--password-stdin] [--data DIR] [user@]host[:port]
```

- `--use-existing-user NAME`: the account exists. Prepare reports what is missing (groups, login shell, sudo rule) and
  changes nothing about the account except adding the controller's key; with `--confirm` it also adds the groups and the
  sudoers rule.
- `--root`: Root access. The controller signs in as root and no service account is created. The node command is
  installed all the same and the controller runs it directly, without sudo. Prepare shows a warning ("Uses root; a
  dedicated account is safer"). `--allow-root` is the old name of this option and still works.
- `--from ADDR`: adds `from="ADDR"` to the key, so it works only from the controller's address.
- `--data DIR`: the controller's data directory (its key, `known_hosts`, instance id); default `data`.
- The host key of the node is recorded in the controller's `known_hosts` on first sight.

## Tested matrix

| Distribution | Status |
|---|---|
| Debian 13, Rocky Linux 10 | tested |
| Debian 12, Ubuntu 22.04 and 24.04, RHEL, AlmaLinux and Rocky 9 and 10, Fedora | untested, prepare warns and goes on |
| anything else | error; install by hand (below, including the node command), then add the node |

## Packages

The names were checked on Debian 13 and Rocky Linux 10.

| | Debian family | RHEL family |
|---|---|---|
| base | `qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients dnsmasq-base ovmf swtpm swtpm-tools sudo curl ca-certificates openssh-client` | `qemu-kvm libvirt virt-install edk2-ovmf swtpm swtpm-tools sudo curl openssh-clients` |
| extra `guestfs` | `libguestfs-tools` | `guestfs-tools` |
| extra `virtiofs` | `virtiofsd` | `virtiofsd` |

## The manual equivalent

As root on the node. Debian family:

```sh
apt-get update
apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients \
    dnsmasq-base ovmf swtpm swtpm-tools sudo curl ca-certificates openssh-client
systemctl enable --now libvirtd
```

RHEL family (libvirt runs as modular daemons there; prepare enables the sockets):

```sh
dnf install -y qemu-kvm libvirt virt-install edk2-ovmf swtpm swtpm-tools sudo curl openssh-clients
systemctl enable --now virtqemud.socket virtnetworkd.socket virtstoraged.socket virtnodedevd.socket \
    virtsecretd.socket virtproxyd.socket virtlogd.socket
```

Then, on both:

```sh
virsh -c qemu:///system net-start default || true         # define it first if the host has none
virsh -c qemu:///system pool-define-as default dir --target /var/lib/libvirt/images
virsh -c qemu:///system pool-build default; virsh -c qemu:///system pool-autostart default; virsh -c qemu:///system pool-start default

useradd --system --create-home --home-dir /var/lib/virticle --shell /bin/sh --user-group \
    --comment 'virticle service account' -p '*' virticle
usermod -aG libvirt,kvm virticle
install -d -m 700 -o virticle -g virticle /var/lib/virticle/.ssh
echo 'restrict,pty,port-forwarding ssh-ed25519 AAAA... virticle-controller' > /var/lib/virticle/.ssh/authorized_keys
chown virticle:virticle /var/lib/virticle/.ssh/authorized_keys; chmod 600 /var/lib/virticle/.ssh/authorized_keys
runuser -u virticle -- ssh-keygen -q -t ed25519 -N '' -C virticle-peer@$(hostname) -f /var/lib/virticle/.ssh/virticle_peer_ed25519

install -d -m 755 /usr/local/libexec
install -m 755 -o root -g root virticle-node /usr/local/libexec/virticle-node   # see below
printf '%s\n' 'virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-node' > /etc/sudoers.d/virticle.new
visudo -cf /etc/sudoers.d/virticle.new && install -m 440 -o root -g root /etc/sudoers.d/virticle.new /etc/sudoers.d/virticle
rm -f /etc/sudoers.d/virticle.new
```

For Root access leave out the account and the sudo rule: put the controller's key into root's `authorized_keys`, install
the node command the same way, and create the node key as root.

On SELinux hosts run `restorecon -R /var/lib/virticle/.ssh` and `restorecon /usr/local/libexec/virticle-node`
afterwards. The `virticle-node` file is embedded in the controller binary (linux/amd64 and linux/arm64) and
`virticle node prepare` installs it. To get it by hand, build it from the source with `make nodebin`; the files are
`internal/nodebin/dist/virticle-node_linux_amd64` and `_arm64`.

## The SSH key restriction

The key line is `restrict,pty,port-forwarding[,from="ADDR"] ssh-ed25519 AAAA... virticle-controller`.

- `restrict` turns off everything not named: X11 and agent forwarding, `~/.ssh/rc`, and the terminal.
- `port-forwarding` is required: the controller reaches libvirt by forwarding a connection to its UNIX socket, and the
  VNC console to the VM's display on the node's loopback address.
- `pty` is required: the event stream (`virsh event --loop`) runs with a terminal so that closing the session ends
  the remote process.
- `permitopen=` was tried and dropped: OpenSSH applies it to the UNIX socket forward too, so libvirt becomes unreachable.
- Shell commands, `scp` and `sftp` keep working (they are not restricted by `restrict`).

## The node command

`/usr/local/libexec/virticle-node` is a Go program, `root:root 0755`, embedded in the controller and versioned
(`virticle-node version` prints the protocol number, which the controller compares with its own). The sudo rule of the
service account allows exactly this program:

```
# /etc/sudoers.d/virticle
virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-node
```

sudoers cannot pin the arguments of a program, so the node command checks them itself.

**What it may do.** Run one of the fixed verbs below. Every verb validates all its arguments first (absolute paths
inside the allowed roots, no `..`, no `//`, no shell characters, no symbolic links in written paths), never passes an
argument to a shell, and works on open directory descriptors, so a link swapped in later is not followed.

**What it may not do.** There is no verb that runs a given command, none that reads or writes outside the allowed
roots, and none that replaces an existing file. It has no listener and no daemon. It cannot update itself: that would
let anyone who holds the service account install any code as root.

| Verb | Does |
|---|---|
| `version` | prints the protocol version |
| `migtls-state DIR` | prints the migration certificate directory (`DIR` must be the one `qemu.conf` names, default `/etc/pki/qemu`) |
| `migtls-mkdir DIR` | creates that directory |
| `migtls-put DIR NAME OWNER MODE` | stdin to `DIR/NAME.virticle-tmp`; `NAME` from the five certificate files and the marker, `MODE` 0400 for keys and 0644 otherwise, `OWNER` `user:0`; at most 64 KiB |
| `migtls-discard DIR` | removes the temporary files |
| `migtls-commit DIR [backup:NAME]... [rm-legacy] [restorecon]` | keeps replaced files as `*.virticle-orig`, moves the temporary files into place |
| `migtls-remove DIR (rm:NAME\|restore:NAME)...` | removes the setup's files, puts backups back |
| `migtls-rmdir DIR TARGET` | removes an empty directory the setup created (`TARGET` is `DIR` or one of its parents below `/etc`) |
| `gpu-users PCIADDR` | names the processes that hold a graphics card open |
| `roots` | prints the allowed roots (see below) |
| `stat PATH...` | `KIND\|BLOCKS\|BLOCKSIZE\|MODE\|SIZE` per path, or `MISSING` |
| `remove PATH...` | removes files (not directories, not symbolic links; a missing file is fine) |
| `rmtree DIR` | removes a work directory (`.virticle-*`) or a directory that holds `manifest.json` |
| `rmdir DIR` | removes an empty directory |
| `move SRC DST` / `copy SRC DST` | rename or copy; `DST` must not exist and is never replaced |
| `read PATH MAXBYTES` | prints a file, at most `MAXBYTES` bytes (up to 64 MiB) |
| `write PATH MAXBYTES [MODE]` | stdin to a new file, at most `MAXBYTES` bytes; `MODE` 600 or 644 |
| `mkworkdir DIR TAG` / `chmod-work MODE DIR` | private work directory `DIR/.virticle-TAG.XXXXXX`; its mode (700 or 711) |
| `qemu-img-info PATH [chain] [FORMAT]` | `qemu-img info` as JSON; every backing file must also be inside a root |
| `sha256 PATH` | SHA-256 of a file |
| `tools-missing NAME...` | which of `qemu-img ssh scp tar sha256sum` are missing |
| `df DIR` | free bytes of the file system that holds `DIR` |
| `files-size DIR NAME...` | size of each regular file `DIR/NAME` (plain names, no symbolic link), or `MISSING` |
| `backup-scan DIR MARKER` | the backups in a pool directory with their manifests (at most 500; manifests at most 64 KiB; `MARKER` is `@@@` and 16 hex digits) |
| `tar-pack DIR NAME...` | tar of the named plain files of a backup directory (one that holds `manifest.json`) on stdout |
| `tar-unpack WORKDIR` | stdin tar into the empty work directory; names with a slash are skipped; if anything but plain single-link files results, the directory is emptied and the call fails |
| `convert-start WORKDIR SRC FORMAT` | detached `qemu-img convert` of `SRC` to `WORKDIR/out.part`; `FORMAT` is `raw`, `qcow2`, `vpc`, `vhdx` or `vdi`; a qcow2 backing chain must be inside the roots and may not use an external data file |
| `convert-poll WORKDIR` / `convert-kill WORKDIR` | exit code and progress of that conversion; stop it |
| `convert-finish WORKDIR DEST` | moves `out.part` to the new file `DEST` (never replaces) and drops the state files |
| `download URL DEST [sha256\|sha512 HEX]` | the node fetches an `http://` or `https://` URL into the new file `DEST` (see "Downloads and imports") |
| `download-cancel DEST` | stops the running download of `DEST` |
| `image-info PATH` | `qemu-img info` as JSON for an image to import; the format is recognised from the content |
| `realpath file\|dir PATH`, `free-space DIR` | canonical path of an existing file or directory inside a root; free bytes of its file system |
| `tar-list FILE`, `tar-extract FILE WORKDIR NAME...` | list an OVA; extract plain-named members of an archive that holds regular files only into a work directory |
| `import-convert-start WORKDIR SRC`, `import-convert-status WORKDIR`, `import-convert-kill WORKDIR` | run `qemu-img convert` to qcow2 in the background from a work directory, read its progress, stop it |
| `sysprep DISK...` | `virt-sysprep` with a fixed option set on qcow2 or raw images |
| `peer-key`, `peer-allow`, `peer-disallow` | the account's own node key; install or remove a peer's key (restricted to `peer-recv`) |
| `peer-hostkey-set HOST PORT`, `peer-hostkey-unset HOST PORT`, `peer-list` | host keys the node accepts for its peers; what is on record |
| `peer-send HOST PORT USER SRC DST MODE`, `peer-ping HOST PORT USER`, `peer-recv` | send a file to a peer node; test a peer; receive (the forced command of a peer's key) |
| `svc-allow`, `svc-hosts`, `svc-list`, `svc-peerkey` | internal: the node command starts itself again as the service account to edit that account's `~/.ssh` files, so root never follows a link planted there. Not meant to be called by hand. |
| `convert-run`, `import-convert-run` | internal: the detached part of `convert-start` and `import-convert-start` |

**Allowed roots.** The file verbs work only inside the target directories of the libvirt storage pools of type `dir`,
`fs` and `netfs`, and the UEFI NVRAM directory `/var/lib/libvirt/qemu/nvram`. The node command computes them on the node
on every call. A path must be absolute and plain (`A-Za-z0-9._+=,@/-`, at most 200 characters, no `..`, `//` or trailing
`/`); its parent directory is resolved and must be inside a root; verbs that change or remove refuse a final component
that is a symbolic link; verbs that create refuse to replace anything. A pool whose directory is a system place (`/etc`,
`/usr`, `/root`, `/var/lib/libvirt/qemu`, ...) is never a root.

**Downloads and imports.** `download` runs `curl` as root with `-q`, `--proto =http,https` and
`--proto-redir =http,https`, so neither the URL nor a redirect can name a local file or another scheme. The URL must start with
`http://` or `https://`, be at most 2000 characters and contain only `A-Za-z0-9._~:/?#@!$&()*+,;=%-`. The destination must be
a new name (starting with a letter or digit) inside a root. The data goes to `.NAME.part` next to it, which is created
exclusively and never written through a link; with a digest the file is moved to `NAME` only if it matches; any failure,
cancel or signal removes the part file. Import reads an image or OVA that lies inside a root. `image-info` and
`convert-start` choose the format themselves (raw, qcow2, vmdk, vpc, vhdx or vdi) and refuse a qcow2 whose backing chain
leaves the roots and a VMDK whose descriptor names another file. `tar-extract` refuses an archive that contains anything
but regular files with plain names. `sysprep` takes the first-boot script from the node command itself; the caller
supplies only image paths, each inside a root, not a link, qcow2 (backing chain inside the roots) or raw.

### How nodes exchange files

Each node has its own SSH key, the **node key** (`~/.ssh/virticle_peer_ed25519`, made by `virticle node prepare`; the
private half stays on the node). When a migration or a restore needs a file copied, the controller:

1. installs the sending node's public key on the receiving node as
   `restrict,command="sudo -n /usr/local/libexec/virticle-node peer-recv"` (without `sudo -n` when the receiving
   account is root), so that key can deliver one new file into a storage pool and nothing else;
2. records the receiving node's SSH host key, taken from the controller's `known_hosts`, on the sending node, which
   checks it strictly;
3. tests the path with `peer-ping`, then runs `peer-send` on the source, which reads the file as root and streams it
   over `ssh` to `peer-recv` on the target.

The file data goes from node to node and does not pass through the controller. The migration pre-flight does this,
idempotently, in its **SSH between nodes** row; nothing is entered by hand. It works for any pair of nodes, service
account or root. Removing a node from the controller removes its key from the other nodes, and theirs from it.
`virticle node unprepare` removes the node's key and the keys of peers installed on it.

**Live migration** is coordinated by the controller for every node (it calls the phases of libvirt's migration protocol
on both nodes), so it needs no SSH between the nodes. The memory stream and the disk mirror still go from node to node.

### What the node command is used for

The account is in the `libvirt` group, so libvirt operations and volume upload need no root. The node command is used for
**encrypted migration** certificates, **migrating VMs** (live and offline), **backing up and restoring VMs** (same node
or another node, export and import included), **downloading** images and the image catalogue into a pool, **importing**
disks and OVAs, **moving a disk** to another pool, and **cloning with guest preparation** (virt-sysprep, if libguestfs is
installed). Backup export and import stream through the controller to and from the browser.

### Versions and detection

- The controller never replaces the node command by itself. When the version on a node differs from the controller's,
  Test connection says "prepared by an older version; run `virticle node prepare` again" (or names the two versions);
  the node counts as not prepared, and the features that need root are off, until an administrator runs prepare again.
- On each connection the controller detects the access: **Root** (uid 0 and the node command of this version answers),
  **Service account** (the node command is present, its version matches, and `sudo -n virticle-node version` works) or
  **Not prepared** (anything else). A node stored as `sudo` by an older version is detected again.
