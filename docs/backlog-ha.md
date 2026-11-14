# Backlog: automatic failover

Replication with a one-click failover exists (see the user guide, "Replication and failover"). What follows is what
automatic failover would need. None of it is built.

## Why a person confirms today

When a node stops answering, Virticle cannot tell a dead node from a node that is only cut off. Starting the VM on the
standby while the old node still runs it gives two copies with the same MAC and IP addresses. The disks are separate,
so nothing is destroyed, but the two copies diverge. A person who knows the node is down is the only fence there is.

## What automatic failover needs

| Need | Why | Notes |
| --- | --- | --- |
| Fencing | Make sure the old copy is stopped before the new one starts. | Power control by BMC (IPMI, Redfish) or a smart plug; one fence method per node, tested on setup. Without a confirmed fence the failover must not start. |
| A witness or quorum | A node that is alone must not decide. | Three voters (two nodes and a small witness on a third machine) or a shared device. The controller itself is a single point: it would have to run on more than one machine, or each node would need a small agent that holds its own VMs down when it loses the quorum. |
| A per-VM policy | Not every VM should restart by itself. | Off by default. A VM opts in; the failed-over VM goes back only by a person's click. |
| A start order and capacity check | The standby may not hold all failed-over VMs. | Memory and CPU on the standby against the VMs that would start; start the important VMs first. |
| A way back | After the old node returns. | Exists for the manual path (old copy marked and shut off, **Replicate back**, then **Planned failover**); the automatic path would reuse it. |
| A shorter interval | The copy is at most one interval old. | Replication every 5 minutes is the floor today; shorter needs a continuous scheme (a live disk mirror) and is a different feature. |

## Not part of it

Shared storage (see `backlog-storage.md`), which has its own failover plan, and live memory replication (COLO), which
needs matching hardware and a fast link.
