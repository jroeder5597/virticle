package api

import (
	"fmt"
	"path"

	"virticle/internal/hostconn"
)

// markShared decides, for every file of the VM, whether it lives on shared storage that both nodes mount (an
// NFS pool with the same server, export and mount path). Such a file is not copied, not mirrored and not
// removed from the source: the target opens the same file. The result is written to the plan (migFile.Shared)
// and explained in plain words in the pre-flight list.
func (s *Server) markShared(src, dst *hostconn.Host, plan *migPlan, c *checkList) {
	srcPools, err1 := poolDescs(src)
	dstPools, err2 := poolDescs(dst)
	if err1 != nil || err2 != nil {
		// Without the pool lists nothing is treated as shared, so files are copied (or refused by the later
		// checks); that is the safe direction.
		c.warn("Shared storage", "cannot read the storage pools of the nodes, so no disk is treated as shared")
		return
	}
	places := map[string]diskPlace{}
	for _, f := range plan.Files.list {
		dir := path.Dir(f.Path)
		pl, ok := places[dir]
		if !ok {
			pl = placeDir(dir, src.Name, dst.Name, srcPools, dstPools)
			places[dir] = pl
			switch {
			case pl.Problem != "":
				c.bad("Storage "+dir, pl.Problem)
			case pl.Note != "":
				c.pass("Storage "+dir, pl.Note)
			case pl.Shared:
				c.pass("Storage "+dir, fmt.Sprintf("pool %s is the NFS share %s on both nodes: the files are not copied", pl.Pool.Name, pl.Pool.shareText()))
			}
		}
		if pl.Shared && pl.Problem == "" {
			f.Shared, f.Skip, f.SharedPool = true, true, pl.Pool.Name
		}
	}

	// A shared file must be visible on the target; otherwise the two nodes do not see the same share (a wrong
	// export, a stale mount) and the VM would start without its disk.
	var shared []string
	for _, f := range plan.Files.list {
		if f.Shared {
			shared = append(shared, f.Path)
		}
	}
	if len(shared) > 0 {
		ex, err := dst.Exists(shared...)
		for _, p := range shared {
			switch {
			case err != nil:
				c.bad("Shared files", "cannot check the shared storage on "+dst.Name+": "+err.Error())
			case !ex[p]:
				c.bad("Shared file "+path.Base(p), p+" is not visible on "+dst.Name+" although both nodes mount the same share. Check that the share is mounted there (Storage) and that both nodes use the same server and export")
			}
			if err != nil {
				break
			}
		}
	}

	var sharedDisks, localDisks []string
	for _, d := range plan.Disks {
		if d.ReadOnly {
			continue
		}
		if plan.sharedDisk(d) {
			sharedDisks = append(sharedDisks, d.Target+" on "+plan.Files.get(d.Chain[0].Path).SharedPool)
		} else {
			localDisks = append(localDisks, d.Target)
		}
	}
	if len(sharedDisks)+len(localDisks) > 0 {
		c.pass("Disks", diskSummary(sharedDisks, localDisks))
	}
}
