# Preparing a node

A node is a Linux host with libvirt and QEMU that the controller reaches over SSH. Nothing of virticle runs on it.
`virticle node prepare` (or **Prepare this node** in the Add node dialog) makes a fresh host ready, and this page says
exactly what it does so you can do the same by hand.

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
3. **Creates the service account** (default `virticle`, a system user, home `/var/lib/virticle`, shell `/bin/sh`, no
   password, groups `libvirt` and `kvm`).
4. **Installs the controller's public key** for that account with restrictions, the **helper**, and the **sudoers rule**.
5. **Creates the node key** of the account (for copies between nodes, see below). **Tests the connection** as the service account, with the same steps as Test connection.

The sudoers file names one account, so a host has one service account: preparing it for another account replaces the
rule. Running it again changes nothing and says so. `virticle node unprepare admin@host` removes the account, key, helper and
sudoers file and leaves packages, VMs, pools and networks alone. It removes an account only when prepare created it
(the account carries the comment "virticle service account"); for an existing account it removes just the key and the
rule, and the group memberships stay. It ends the account's processes first, so remove the node in the controller
before you run it.

```
virticle node prepare [--check] [--user virticle] [--use-existing-user[=NAME]] [--confirm] [--root]
                      [--from ADDR] [--extras guestfs,virtiofs] [--jump user@host]
                      [--identity FILE] [--password-stdin] [--data DIR] [user@]host[:port]
```

- `--use-existing-user NAME`: the account exists. Prepare reports what is missing (groups, login shell, sudo rule) and
  changes nothing about the account except adding the controller's key; with `--confirm` it also adds the groups and the
  sudoers rule.
- `--root`: the controller logs in as root; no service account is created. The node command is installed all the same and the
  controller runs it directly, without sudo. Shown as a warning ("Uses root; a dedicated account is safer"). `--allow-root` is the
  old name of this option and still works.
- `--from ADDR`: adds `from="ADDR"` to the key, so it works only from the controller's address.
- `--data DIR`: the controller's data directory (its key, `known_hosts`, instance id); default `data`.
- The host key of the node is recorded in the controller's `known_hosts` on first sight.

## Tested matrix

| Distribution | Status |
|---|---|
| Debian 13, Rocky Linux 10 | tested |
| Debian 12, Ubuntu 22.04 and 24.04, RHEL, AlmaLinux and Rocky 9 and 10, Fedora | untested, prepare warns and goes on |
| anything else | error; install by hand (below, including the node command) and add the node |

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
install -m 755 -o root -g root virticle-helper /usr/local/libexec/virticle-helper   # the file from this binary, see below
printf '%s\n' 'virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-helper' > /etc/sudoers.d/virticle.new
visudo -cf /etc/sudoers.d/virticle.new && install -m 440 -o root -g root /etc/sudoers.d/virticle.new /etc/sudoers.d/virticle
rm -f /etc/sudoers.d/virticle.new
```

On SELinux hosts run `restorecon -R /var/lib/virticle/.ssh` and `restorecon /usr/local/libexec/virticle-helper`
afterwards. The `virticle-helper` file is embedded in the controller binary; `virticle node prepare` installs it, and its
text is in `internal/hostconn/virticle-helper.sh` in the source.

## The SSH key restriction

The key line is `restrict,pty,port-forwarding[,from="ADDR"] ssh-ed25519 AAAA... virticle-controller`.

- `restrict` turns off everything not named: X11 and agent forwarding, `~/.ssh/rc`, and the terminal.
- `port-forwarding` is required: the controller reaches libvirt by forwarding a connection to its UNIX socket, and the
  VNC console to the VM's display on the node's loopback address.
- `pty` is required: the event stream (`virsh event --loop`) runs with a terminal so that closing the session ends
  the remote process.
- `permitopen=` was tried and dropped: OpenSSH applies it to the UNIX socket forward too, so libvirt becomes unreachable.
- Shell commands, `scp` and `sftp` keep working (they are not restricted by `restrict`).

## Scoped root access: the helper

The service account has **no general sudo**. Its sudoers rule allows exactly one program:

```
# /etc/sudoers.d/virticle
virticle ALL=(root) NOPASSWD: /usr/local/libexec/virticle-helper
```

`/usr/local/libexec/virticle-helper` is a POSIX shell script, `root:root 0755`, embedded in the controller binary and
versioned (`virticle-helper version`). Its subcommands are fixed, validate every argument (absolute paths from an
allow-list, no `..`, no `//`, no shell characters, no symbolic links in written paths, root-owned target directory),
never evaluate an argument, and there is no subcommand that runs a given command:

| Subcommand | Does |
|---|---|
| `version` | prints the helper version |
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
| `read PATH MAX` | prints a file, at most `MAX` bytes (up to 64 MiB) |
| `write PATH MAX [600\|644]` | stdin to a new file, at most `MAX` bytes |
| `mkworkdir DIR TAG` / `chmod-work 700\|711 DIR` | private work directory `DIR/.virticle-TAG.XXXXXX` and its mode |
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
| `peer-hostkey-set HOST PORT`, `peer-hostkey-unset`, `peer-list` | host keys the node accepts for its peers; what is on record |
| `peer-send HOST PORT USER SRC DST MODE`, `peer-ping`, `peer-recv` | send a file to a peer node; test a peer; receive (the peer key's forced command) |

**Allowed roots.** The file verbs work only inside the target directories of the libvirt storage pools of type `dir`,
`fs` and `netfs`, and the UEFI NVRAM directory `/var/lib/libvirt/qemu/nvram`. The helper computes them on the node on
every call. A path must be absolute and plain (`A-Za-z0-9._+=,@/-`, at most 200 characters, no `..`, `//` or trailing
`/`); its parent directory is resolved with `realpath` and must be inside a root; verbs that change or remove refuse a
final component that is a symbolic link; verbs that create refuse to replace anything. A pool whose directory is a
system place (`/etc`, `/usr`, `/root`, `/var/lib/libvirt/qemu`, ...) is never a root.

**Downloads and imports.** `download` runs `curl` as root with `-q`, `--proto =http,https` and
`--proto-redir =http,https`, so neither the URL nor a redirect can name a local file or another scheme. The URL must start with
`http://` or `https://`, be at most 2000 characters and contain only `A-Za-z0-9._~:/?#@!$&()*+,;=%-`. The destination must be
a new name (starting with a letter or digit) inside a root. The data goes to `.NAME.part` next to it, which is created
exclusively and never written through a link; with a digest the file is moved to `NAME` only if it matches; any failure,
cancel or signal removes the part file. Import reads an image or OVA that lies inside a root. `image-info` and
`convert-start` choose the format themselves (raw, qcow2, vmdk, vpc, vhdx or vdi) and refuse a qcow2 whose backing chain
leaves the roots and a VMDK whose descriptor names another file. `tar-extract` refuses an archive that contains anything
but regular files with plain names. `sysprep` takes the first-boot script from the helper itself; the caller supplies only
image paths, each inside a root, not a link, qcow2 (backing chain inside the roots) or raw.

**Node to node copies.** Each helper node has its own SSH key (`~/.ssh/virticle_peer_ed25519`, made by
`virticle node prepare`; the private half stays on the node). When a migration needs files copied, the controller
installs the sending node's public key on the receiving node with `restrict,command="sudo -n <helper> peer-recv"` (without `sudo -n` when the receiving
account is root), so
that key can deliver a new file into a storage pool and nothing else, and records the receiving node's SSH host key
(taken from the controller's `known_hosts`) on the sending node, which checks it strictly. Then `peer-send` on the
source reads the file as root and streams it over `ssh`, run as the service account, to `peer-recv` on the target. The
data does not pass through the controller. This is done by the pre-flight check, idempotently; nothing is entered by
hand. Removing a node from the controller removes its key from the other helper nodes, and theirs from it. `virticle node unprepare`
removes the node's key and the peer keys installed on it.

**Live migration** is coordinated by the controller for every node, root or service account (it calls the phases of
libvirt's migration protocol on both nodes), so no SSH between the nodes is needed for it. The memory stream and the disk mirror
still go from node to node.

**There is no self-update.** A helper that replaced itself from the service account would let anyone who holds that
account install any code as root. When the controller finds a helper whose version
differs from its own, the connection test says so and the node counts as not prepared (privileged features are off) until
an administrator runs `virticle node prepare` again.

### What the helper covers

With the helper the account can run libvirt operations (it is in the `libvirt` group), upload and manage volumes through
libvirt, take screenshots, set up and remove **encrypted migration** certificates, **migrate VMs** (live and
offline), **back up and restore VMs** (live and offline, same node or another node, export and import included),
**download** images and the image catalogue into a pool, **import** disks and OVAs, **move a disk** to another pool,
and **clone with guest preparation** (virt-sysprep, if libguestfs is installed).

- Files that move between two nodes go node to node by `peer-send`, never through the controller, for every pair: two
  service accounts, two root logins, or one of each.
- Backup export and import stream through the controller to and from the browser.

There is no other way to give the controller root on a node: a login with passwordless full sudo but without the node
command is simply not prepared, and the connection test says `this node has no node command; run "virticle node
prepare" for it`. A root login runs the node command directly (installed by `virticle node prepare --root`).

Privilege detection on each connection: **root** (uid 0 and the node command of this version answers), **helper** (the
service account; the node command is present, its version matches, and `sudo -n <node command> version` works) or
**none** (anything else). A node stored as `sudo` by an older version is detected again.
