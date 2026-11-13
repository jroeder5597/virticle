package nodecmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"virticle/internal/nodeproto"
)

// The host network verbs: Linux bridges with optional VLAN filtering, with a
// rollback that the node itself owns. See net_engine.go for the state and
// the steps, net_networkd.go and net_nm.go for the configuration systems.
//
//	net-info                    JSON: the configuration system, the bridges, the status
//	net-plan                    JSON request on stdin; prints the plan as JSON, changes nothing
//	net-apply                   JSON request on stdin; saves the state, arms the rollback timer, starts the apply
//	                            in a transient systemd unit and prints {"id":..,"timeoutSeconds":..}
//	net-run ID                  the detached half of net-apply (run by the unit)
//	net-confirm ID              make the change permanent and cancel the rollback timer
//	net-rollback [ID] [timer]   restore the saved state; without ID the pending change; safe to repeat
//	net-status                  JSON: the pending change and the last ones
//	net-boot                    run by a unit at boot: a change still unconfirmed gets its rollback timer again
//
// By hand, from the console of a node whose network does not answer:
//
//	/usr/local/libexec/virticle-node net-rollback

func init() {
	Register("net-info", verbNetInfo)
	Register("net-plan", verbNetPlan)
	Register("net-apply", verbNetApply)
	Register("net-run", verbNetRun)
	Register("net-confirm", verbNetConfirm)
	Register("net-rollback", verbNetRollback)
	Register("net-status", verbNetStatus)
	Register("net-boot", verbNetBoot)
}

func (c *Ctx) printJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return Fail("%v", err)
	}
	fmt.Fprintln(c.Stdout, string(b))
	return nil
}

func verbNetInfo(c *Ctx) error {
	if err := c.Need(0, 0, "net-info"); err != nil {
		return err
	}
	st := c.detectStack()
	info := nodeproto.NetInfo{Stack: st.Name, StackTested: st.Tested, Supported: st.Supported, StackNote: st.Note,
		Bridges: []nodeproto.NetBridge{}, Status: c.netStatus()}
	snap, err := c.readSnap()
	if err != nil {
		return err
	}
	managed := c.loadManaged()
	for _, l := range snap.Links {
		if l.Kind != "bridge" {
			continue
		}
		b := nodeproto.NetBridge{Name: l.Name, VLANFiltering: l.VLANFiltering, Ports: c.bridgePorts(l.Name)}
		if b.Ports == nil {
			b.Ports = []string{}
		}
		if sp := managed[l.Name]; sp != nil {
			b.Managed, b.Uplink, b.MovedAddresses = true, sp.Uplink, sp.MoveAddresses
			b.Uplinks, b.Bond, b.BondMode, b.MTU = sp.cardNames(), sp.Bond, sp.BondMode, sp.MTUSet
			for _, v := range sp.VLANAddrs {
				b.VLANAddresses = append(b.VLANAddresses, nodeproto.NetVLANAddr{ID: v.ID, Interface: v.iface(sp), Method: v.Method,
					Address: v.Address, Gateway: v.Gateway, DefaultRoute: v.DefaultRoute})
			}
			if sp.VLANAware {
				b.VLANs = sp.VLANs
			}
		} else if l.VLANFiltering {
			b.VLANs = foreignVLANs(snap, b.Ports)
		}
		info.Bridges = append(info.Bridges, b)
	}
	sort.Slice(info.Bridges, func(i, j int) bool { return info.Bridges[i].Name < info.Bridges[j].Name })
	return c.printJSON(info)
}

// foreignVLANs is what the ports of a bridge made by hand allow: the union of the ids of the ports
// libvirt does not manage (without the untagged default 1).
func foreignVLANs(snap *netSnap, ports []string) string {
	var parts []string
	for _, p := range ports {
		if libvirtOwned(p) || strings.HasPrefix(p, "tap") {
			continue
		}
		if v := snap.PortVLANs[p]; v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	canon, err := nodeproto.ParseVLANs(strings.Join(parts, ","))
	if err != nil {
		return ""
	}
	return taggedVLANs(canon)
}

func verbNetPlan(c *Ctx) error {
	if err := c.Need(0, 0, "net-plan  (JSON request on stdin)"); err != nil {
		return err
	}
	req, err := c.readRequest()
	if err != nil {
		return err
	}
	in, pl, st, err := c.buildPlan(req)
	if err != nil {
		return err
	}
	return c.printJSON(c.publicPlan(in, pl, st))
}

func verbNetApply(c *Ctx) error {
	if err := c.Need(0, 0, "net-apply  (JSON request on stdin)"); err != nil {
		return err
	}
	req, err := c.readRequest()
	if err != nil {
		return err
	}
	unlock, err := c.netLock()
	if err != nil {
		return err
	}
	defer unlock()
	in, pl, st, err := c.buildPlan(req)
	if err != nil {
		return err
	}
	now := c.netNow()
	m := &netMeta{ID: newChangeID(now), Action: req.Action, Bridge: req.Bridge, Stack: st.Name, State: nodeproto.NetArmed,
		Started: now, Deadline: now.Add(time.Duration(req.TimeoutSeconds) * time.Second), Timeout: req.TimeoutSeconds,
		Steps: pl.Steps, Restore: pl.Restore, Marker: pl.Marker, Spec: pl.Spec}
	if err := c.saveState(m, pl, in.Snap); err != nil {
		_ = os.RemoveAll(c.changeDir(m.ID))
		return Fail("cannot save the current network state, nothing was changed: %v", err)
	}
	pending := filepath.Join(c.netDir(), "pending")
	if err := c.atomicWrite(pending, []byte(m.ID+"\n"), 0o600); err != nil {
		_ = os.RemoveAll(c.changeDir(m.ID))
		return Fail("cannot record the change, nothing was changed: %v", err)
	}
	c.netLog(m.ID, "%s of bridge %s planned with %s; the previous state is saved", m.Action, m.Bridge, st.Name)
	// The unit that arms the rollback again if the node restarts before the confirmation.
	if err := c.ensureBootUnit(); err != nil {
		_ = os.Remove(pending)
		_ = os.RemoveAll(c.changeDir(m.ID))
		return Fail("cannot install the boot-time rollback unit, nothing was changed: %v", err)
	}
	// The rollback is armed before anything changes.
	if err := c.netTimer().Arm(m.ID, req.TimeoutSeconds); err != nil {
		_ = os.Remove(pending)
		_ = os.RemoveAll(c.changeDir(m.ID))
		return Fail("cannot arm the rollback timer, nothing was changed: %v", err)
	}
	c.netLog(m.ID, "rollback armed for %d seconds", req.TimeoutSeconds)
	if err := c.netTimer().Detach(m.ID); err != nil {
		_ = c.netTimer().Cancel(m.ID)
		m.State, m.Reason, m.Message, m.Finished = nodeproto.NetRolledBack, "apply failed", "the apply could not be started: "+err.Error(), c.netNow()
		_ = c.saveMeta(m)
		_ = os.Remove(pending)
		return Fail("cannot start the change, nothing was changed: %v", err)
	}
	return c.printJSON(nodeproto.NetApplyResult{ID: m.ID, TimeoutSeconds: req.TimeoutSeconds})
}

func verbNetRun(c *Ctx) error {
	if err := c.Need(1, 1, "net-run ID"); err != nil {
		return err
	}
	return c.runChange(c.Args[0])
}

func verbNetConfirm(c *Ctx) error {
	if err := c.Need(1, 1, "net-confirm ID"); err != nil {
		return err
	}
	unlock, err := c.netLock()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := c.loadMeta(c.Args[0])
	if err != nil {
		return err
	}
	if err := c.confirmLocked(m); err != nil {
		return err
	}
	fmt.Fprintln(c.Stdout, "confirmed")
	return nil
}

func verbNetRollback(c *Ctx) error {
	if err := c.Need(0, 2, "net-rollback [ID] [timer]"); err != nil {
		return err
	}
	unlock, err := c.netLock()
	if err != nil {
		return err
	}
	defer unlock()
	var m *netMeta
	fromTimer := false
	args := c.Args
	if len(args) == 2 && args[1] == "timer" {
		fromTimer = true
	} else if len(args) == 2 {
		return Die("usage: net-rollback [ID] [timer]")
	}
	if len(args) >= 1 {
		if m, err = c.loadMeta(args[0]); err != nil {
			return err
		}
	} else if m = c.pendingMeta(); m == nil {
		return Fail("no network change is pending")
	}
	switch m.State {
	case nodeproto.NetConfirmed:
		return Fail("the change %s is confirmed (permanent); delete the bridge to undo it", m.ID)
	case nodeproto.NetRolledBack, nodeproto.NetFailed:
		fmt.Fprintf(c.Stdout, "%s was rolled back already\n", m.ID)
		return nil
	}
	reason, msg := "manual", "Rolled back by hand."
	if fromTimer {
		reason, msg = "timer", "The change was not confirmed in time, so the node restored its previous network settings by itself."
	}
	c.rollbackLocked(m, reason, msg, fromTimer)
	fmt.Fprintf(c.Stdout, "%s: %s\n", m.ID, m.State)
	if m.State == nodeproto.NetFailed {
		return Fail("%s", m.Message)
	}
	return nil
}

func verbNetStatus(c *Ctx) error {
	if err := c.Need(0, 0, "net-status"); err != nil {
		return err
	}
	return c.printJSON(c.netStatus())
}

// verbNetBoot runs at every boot (virticle-net-boot.service, only when the pending marker exists). A change that
// is still unconfirmed now was interrupted by a restart: the transient timer is gone and the controller's
// task has most likely given up. The change gets the grace period it had at the start, counted from now, so that
// a controller that is still waiting can confirm, and is rolled back when nobody does. If the timer cannot be armed the
// change is rolled back at once: a node must never stay on a configuration nobody confirmed.
func verbNetBoot(c *Ctx) error {
	if err := c.Need(0, 0, "net-boot"); err != nil {
		return err
	}
	unlock, err := c.netLock()
	if err != nil {
		return err
	}
	defer unlock()
	m := c.pendingMeta()
	if m == nil {
		return nil
	}
	if out, err := c.netRun("systemctl", "is-active", rollbackUnit(m.ID)+".timer"); err == nil || strings.TrimSpace(out) == "active" {
		return nil // the timer is armed already (the unit was started by hand)
	}
	grace := m.Timeout
	if grace < 30 {
		grace = 30
	}
	if grace > 600 {
		grace = 600
	}
	c.netLog(m.ID, "the node restarted while the change was %s; arming the rollback again for %d seconds", m.State, grace)
	if err := c.netTimer().Arm(m.ID, grace); err != nil {
		c.netLog(m.ID, "cannot arm the rollback timer: %v", err)
		c.rollbackLocked(m, "restart", "The node restarted before the change was confirmed, so it restored its previous network settings.", true)
		return nil
	}
	m.Deadline = c.netNow().Add(time.Duration(grace) * time.Second)
	_ = c.saveMeta(m)
	return nil
}
