package nodecmd

import (
	"fmt"
	"strings"

	"virticle/internal/nodeproto"
)

// describeUplinkChange fills the summary and warnings of a set-uplinks plan.
func describeUplinkChange(c *Ctx, pl *netPlanned, in *planInput, d cardDiff) {
	old, n := in.Old, in.New
	for _, x := range d.Removed {
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s leaves %s and gets its own configuration back (%s).", x.Name, n.Bridge, strings.Join(x.Expect, ", ")))
	}
	for _, x := range in.Added {
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s joins %s as a member of the bond %s.", x.Link.Name, n.Bridge, n.Bond))
		if w := cardsDropSummary(x); w != "" {
			pl.Summary = append(pl.Summary, w)
		}
	}
	if d.BondCreated {
		pl.Summary = append(pl.Summary, fmt.Sprintf("A bond %s is created. It becomes the only port of %s and holds %s. Mode: %s. For a moment %s has no uplink.", n.Bond, n.Bridge, strings.Join(n.cardNames(), ", "), bondModeWords(n.BondMode), n.Bridge))
		if n.MoveAddresses {
			pl.Summary = append(pl.Summary, fmt.Sprintf("The bond gets the MAC address %s, the one the bridge already shows.", n.MAC))
		}
	}
	if d.ModeChanged {
		pl.Summary = append(pl.Summary, fmt.Sprintf("The mode of the bond %s changes from %s to %s. The bond is made again, so the uplink is down for a moment.", n.Bond, bondModeWords(old.BondMode), bondModeWords(n.BondMode)))
	}
	if n.Bond != "" && len(n.cards()) == 1 {
		pl.Summary = append(pl.Summary, fmt.Sprintf("%s stays a bond with one member; it works like a plain uplink and takes more cards later.", n.Bond))
	}
	if n.BondMode == nodeproto.BondLACP {
		pl.Warn = append(pl.Warn, c.lacpWarnings(n.cardNames())...)
	}
}

// vlanAddrPlanSummary is the summary of an add-vlan-address plan.
func vlanAddrPlanSummary(old, n *bridgeSpec, v vlanAddrSpec) []string {
	sum := []string{vlanAddrSummary(n, v)}
	if n.VLANs != old.VLANs {
		sum = append(sum, fmt.Sprintf("VLAN %d was not in the list of allowed ids of %s; it is added (now %s).", v.ID, n.Bridge, n.VLANs))
	}
	sum = append(sum, fmt.Sprintf("%s becomes a member of VLAN %d itself (bridge vlan ... self), and the port carries the id tagged.", n.Bridge, v.ID))
	if v.Method == nodeproto.AddrDHCP {
		sum = append(sum, "The change does not wait for a lease: the interface keeps asking the DHCP server of that VLAN.")
	}
	return sum
}
