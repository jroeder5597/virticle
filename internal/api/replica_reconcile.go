package api

// Which of two VMs of one identity is the old copy. The decision is a pure function of what the two VMs'
// metadata says, so it can be tested without hosts; replica_failover.go reads the facts and acts on the answer.
//
// The VM that took over carries a failed-over record with a generation: the generation of the VM it came from
// plus one (replicas remember it, see replicaMeta.SrcGen). Only the generation orders two failovers; the
// controller's clock (At) is a tie-break that is never used to power anything off. Nothing on a node's own
// clock is ever compared.

type oldCopyAction int

const (
	oldNothing  oldCopyAction = iota // leave both alone
	oldMark                          // mark the old VM as the old copy (it is shut off already)
	oldPowerOff                      // mark it and power it off: it runs beside the VM that took over
)

// oldVMFacts is what is known about the VM on the node the failover came from.
type oldVMFacts struct {
	Meta    string // its application metadata
	Running bool   // not shut off
}

// decideOldCopy says what to do with the VM at the node that fo (the record of the VM that took over) came from.
// It never marks or powers off a VM that may be the newer one: when the facts do not say clearly, it does nothing.
func decideOldCopy(fo failedOverMeta, old oldVMFacts) oldCopyAction {
	if parseStale(old.Meta) != nil {
		return oldNothing // handled already
	}
	if parseReplicaMeta(old.Meta) != nil {
		return oldNothing // it is the replica of the running VM now
	}
	if ofo := parseFailedOver(old.Meta); ofo != nil {
		// Both took over at some time. The later failover (the higher generation) is the live VM, and this record
		// is the one that is out of date. Equal generations cannot be told apart: do nothing, the alerts and the
		// guard on Start stay as they are.
		if ofo.Gen >= fo.Gen {
			return oldNothing
		}
	}
	if old.Running {
		return oldPowerOff
	}
	return oldMark
}
