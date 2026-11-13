package api

import (
	"sync"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// replicaGuard refuses a change to a replica. The standby's copy is inert: it is not a VM that can be started,
// edited, deleted, backed up or moved, because its disks are not in place (they are the base and overlays
// of the replica directory) and starting it by any other way would damage the only copy.
func replicaGuard(h *hostconn.Host, dom libvirt.Domain) error {
	meta, err := domainMeta(h, dom)
	if err != nil || meta == "" {
		return nil
	}
	if rm := parseReplicaMeta(meta); rm != nil {
		return badRequest("This VM is the replica of " + dom.Name + " on " + rm.SourceName + ": an inert copy for failover, not a VM that can be changed. " +
			"Use \"Start here from the copy\" (or Planned failover) on this page to run it here, or \"Delete replica\" to remove it.")
	}
	return nil
}

// staleGuard refuses to start the old VM after another node took over: starting it would run the VM twice.
func staleGuard(h *hostconn.Host, dom libvirt.Domain) error {
	meta, err := domainMeta(h, dom)
	if err != nil {
		return nil
	}
	if sm := parseStale(meta); sm != nil {
		return badRequest("This is the old copy of " + dom.Name + " from before it was started on " + sm.ToName + ". Starting it here would run the VM twice with the same addresses. " +
			"Delete it, or use \"Replicate back\" on the running VM to make it the new replica.")
	}
	return nil
}

// isReplicaOrStale is true for the domains that the scheduler, bulk work, evacuation and the checks must skip.
func isReplicaOrStale(h *hostconn.Host, dom libvirt.Domain) bool {
	meta, err := domainMeta(h, dom)
	if err != nil || meta == "" {
		return false
	}
	return parseReplicaMeta(meta) != nil || parseStale(meta) != nil
}

// The replicas each node holds, as the last alert round found them. The statistics round uses it to leave them out
// of the node's figures without asking libvirt for the metadata of every VM every 15 seconds.
var knownReplicas = struct {
	sync.Mutex
	m map[int64]map[string]bool
}{m: map[int64]map[string]bool{}}

func noteReplicas(node int64, reps []alertReplica) {
	set := map[string]bool{}
	for _, r := range reps {
		set[r.UUID] = true
	}
	knownReplicas.Lock()
	knownReplicas.m[node] = set
	knownReplicas.Unlock()
}

// forgetReplica takes a VM off the list at once, when its replica became the VM (a failover).
func forgetReplica(node int64, uuid string) {
	knownReplicas.Lock()
	delete(knownReplicas.m[node], uuid)
	knownReplicas.Unlock()
}

func isKnownReplica(node int64, uuid string) bool {
	knownReplicas.Lock()
	defer knownReplicas.Unlock()
	return knownReplicas.m[node][uuid]
}
