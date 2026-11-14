package api

// Replication and shared storage.
//
// A disk on an NFS share that both nodes mount is already on the standby: it is one file that both nodes see. A copy of
// it on the standby would be a second file under the same name, the replica could never be put in place at failover
// without overwriting the real disk, and with one disk shared and another local the two would be from different moments
// (the local copy up to one interval old, the shared disk current), a state no guest ever had. So a VM with a disk on
// shared storage is not replicated. A migration moves such a VM without copying the shared disks.
//
// The replica itself is written to a local pool of the standby, never to a share: a replica on shared storage would be
// seen by the source node too, and failover would then leave the VM running from the share.

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"virticle/internal/hostconn"
)

// sharedDisksOf names the writable disks of the VM whose files are on an NFS share the standby mounts at the same
// path ("vda on the share srv:/export"). The pools are by directory, as migration reads them.
func sharedDisksOf(md migDomain, srcName, dstName string, srcPools, dstPools map[string]poolDesc) []string {
	var out []string
	for _, d := range md.Disks {
		if d.Path == "" || d.Device != "disk" || d.ReadOnly {
			continue
		}
		pl := placeDir(path.Dir(d.Path), srcName, dstName, srcPools, dstPools)
		if pl.Shared {
			out = append(out, d.Target+" on the shared storage "+pl.Pool.Name+" ("+pl.Pool.shareText()+")")
		}
	}
	sort.Strings(out)
	return out
}

// sharedRefusal is the message when sharedDisksOf found any; empty otherwise.
func sharedRefusal(vm, dstName string, shared []string) string {
	if len(shared) == 0 {
		return ""
	}
	return fmt.Sprintf("%s has a disk on storage that %s mounts too: %s. A disk on shared storage is already on both nodes, so it cannot be replicated, and a VM with such a disk is not replicated at all (a copy of only its other disks would be from another moment). Move the disk to local storage to replicate the VM, or leave it as it is: Migrate moves it to the other node without copying the shared disk",
		vm, dstName, strings.Join(shared, ", "))
}

// refuseSharedDisks reads the pools of both nodes and returns the refusal for a VM with a shared disk, or nil. When the
// pools cannot be read nothing is refused here: the later steps (inspection, space, the copy itself) say what is wrong.
func refuseSharedDisks(src, dst *hostconn.Host, md migDomain) error {
	srcPools, err1 := poolDescs(src)
	dstPools, err2 := poolDescs(dst)
	if err1 != nil || err2 != nil {
		return nil
	}
	unifyServers(src, dst, srcPools, dstPools)
	if msg := sharedRefusal(md.Name, dst.Name, sharedDisksOf(md, src.Name, dst.Name, srcPools, dstPools)); msg != "" {
		return badRequest(msg)
	}
	return nil
}
