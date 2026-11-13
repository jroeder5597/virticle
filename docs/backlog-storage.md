# Storage backlog

Storage features that were considered and are **not built**. Each entry says
what it is, why someone would want it, and what is known about building it.
Written on 2026-11-13 for a later session.

For what exists today, see [user-guide.md](user-guide.md#shared-storage-nfs)
and [reference.md](reference.md#shared-storage-nfs).

## What happens today when a node dies

Nothing automatic, on purpose. What exists:

- A node that does not answer is shown **offline** and raises the `node-down`
  alert (and a notification, if a destination is set).
- **Maintenance** and **Evacuate** move VMs away from a node that is still
  alive, by live migration. They need the node.
- Its VMs stay defined only on the dead node. Their disks may be safe (on shared
  storage) but the controller has no copy of the VM definition, so it cannot
  start them anywhere else. The way out today is by hand: define the VM on
  another node from a copy of its XML (`virsh dumpxml` from a backup of the
  definition, or a Virticle backup) and start it there.

## Start a VM of a dead node on another node, by hand

Not built. Specification, small enough for one session:

**What the user sees.** On the Nodes page, a node that is offline lists the VMs
the controller knew on it. A VM whose disks are all on shared storage that is
running (state `running`, `answers` in the pool table) has the action **Start on
another node**. Other VMs show why they cannot (a disk on the dead node's own
storage).

**The confirmation.** A dialog that says, in these words or plainer ones:

> NODE does not answer. Virticle cannot tell whether the VM is still running
> there. If it is, starting it here makes two copies of the VM write to the same
> disk, and the disk is destroyed. Only go on when you know the node is off:
> switched off, unplugged, or its network cable is out and the VM is not
> needed. Type the name of the VM to confirm.

The button stays disabled until the VM name is typed. The dialog offers the
target node (a node with the same shared storage, enough memory, and the same CPU
family) and nothing else.

**What it does.**

1. Needs a copy of the VM definition, kept by the controller while the node
   was up: a new store table `vm_definitions` (VM UUID, node id, XML, time),
   written when the controller sees a VM defined or changed and, at most once an
   hour, for every VM of every online node. Keep the last XML only. Remove the
   row when the VM is deleted through Virticle. Without a row the action is not
   offered.
2. Checks the target: every disk file of the definition is on an NFS pool that
   both nodes mount (the same rule as the migration check); the pools are up
   (the probe, not libvirt); no VM with this name or UUID exists there; UEFI
   firmware and TPM files are on shared storage too (else refuse: the VM would
   start with empty variables).
3. Tries to open the first disk read-write with `qemu-img info` (a lock
   attempt). With NFS 4 the old node's QEMU holds a lock on the file until its
   lease ends (about 90 seconds after the node died). If the lock is held, say
   so: "the disk is still locked by another computer. Wait two minutes if the
   node was just switched off, or check that the node is really off". This is
   the only protection against two running copies; it depends on the lock service on NFS 3
   (Add shared storage accepts NFS 3 but says so; see the table below).
4. Defines the VM on the target from the stored XML (names and paths are the
   same everywhere by design), starts it, and records the action in the audit
   log with the name of the person.
5. Later, when the old node comes back, it still has the VM defined, and if the VM is set to autostart the
   node would start it at boot: two copies again. So on reconnect, before anything else, the controller looks
   for VMs of this node that were started elsewhere (the action leaves a record), turns their autostart off on
   this node and shows a warning "VM X is defined here and runs on Y. Remove it here", with a button that
   undefines it without touching disks. The node may already have started the VM during boot; the lock in
   step 3 is what stops the second copy then. Never start the VM here automatically.

**Not part of it.** Automatic restart without a person, fencing (power control
through a BMC or smart plug), and a quorum. Without fencing the controller can
never know that the old VM is dead, which is why a person confirms.

**Open questions for the lab.** NFS 4 lease length on the server used (shorter
with `nfsv4leasetime` in `/etc/nfs.conf`); what a partitioned but running node
does to its VMs (hard mount: they wait; they do not write); the autostart race
in step 5 on both a Debian and a Rocky node.

## Other storage features

| Feature | Why | Notes |
| --- | --- | --- |
| NFS version 3 | Old NAS devices speak only version 3. | Add accepts it and shows one sentence: the cross-node locks need the server's lock service (rpc.statd, lockd) reachable from every node. In the lab, with those reachable, a second QEMU on the same image was refused ("Failed to get write lock"), so it is safe there; with a firewall in the way the locks fail and a VM may not start, or, where the lock call is simply dropped, two nodes could open one disk. Needs a lab test with a closed lock port before the note can be dropped or turned into a refusal. |
| Mount options (`soft`, `timeo`, `nconnect`) | Tuning for a slow network or many VMs. | libvirt's NFS pool passes none; a systemd mount unit would. A soft mount hands errors to the guest, so it is the wrong default. `nconnect` helps throughput on fast links. |
| Quota and free-space alert per share | A full share stops every VM on it at once. | The pool usage alert exists per node's pool; a share is one pool per node, so alerts repeat. Group them into one alert per share. |
| iSCSI and Fibre Channel shared storage | Block storage shared by nodes. | Needs a cluster lock manager (`sanlock` or `lvmlockd`); not simple. Not shared today. |
| Ceph / RBD | Replicated shared storage without a NAS. | A project of its own: monitors, OSDs, packages per distribution. |
| Server addresses by name and by address | One node mounts `nas`, another `192.168.1.5`: the files are seen as different shares and copied (the check refuses, since the file already exists on the target). | Resolve the name on the node and compare the address, or ask the user to use one spelling. Today the message says so. |
| Rename or move a share's mount point | The mount point is `/mnt/NAME` and VM definitions hold full paths. | Changing it means rewriting every VM that uses it. Remove and add again instead. |
