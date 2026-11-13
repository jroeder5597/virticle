package api

// The automatic limit on a shared link.
//
// Migration data (memory, the live disk copy, copied files, cross-node backups) must not starve the
// management traffic of a node. When the data of a node travels over the same interface as that node's
// management address, the transfer is capped; when it has its own interface it is not. This file holds the
// decision, which is pure: miglink.go collects the facts it needs from the nodes.

import (
	"fmt"
	"strconv"
	"strings"

	"virticle/internal/hostconn"
)

const (
	limitAuto   = "auto"   // 80% of the link speed when it is known (the default)
	limitNone   = "none"   // never limit
	limitCustom = "custom" // a number of Mbit/s
)

// autoPercent is the share of a shared link the transfer may use.
const autoPercent = 80

// limitSetting is the per-node setting "Limit on a shared link". It is stored as "" (auto), "none" or
// "custom:<Mbit/s>".
type limitSetting struct {
	Mode string
	Mbit int
}

func parseLimit(s string) (limitSetting, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == limitAuto:
		return limitSetting{Mode: limitAuto}, nil
	case s == limitNone:
		return limitSetting{Mode: limitNone}, nil
	case strings.HasPrefix(s, limitCustom+":"):
		n, err := strconv.Atoi(strings.TrimPrefix(s, limitCustom+":"))
		if err != nil || n < 1 || n > 1_000_000 {
			return limitSetting{}, badRequest("the custom limit must be a number of Mbit/s between 1 and 1000000")
		}
		return limitSetting{Mode: limitCustom, Mbit: n}, nil
	}
	return limitSetting{}, badRequest(`the limit must be "auto", "none" or "custom:<Mbit/s>"`)
}

// String is the stored form; the default is stored as the empty string.
func (l limitSetting) String() string {
	switch l.Mode {
	case limitNone:
		return limitNone
	case limitCustom:
		return fmt.Sprintf("%s:%d", limitCustom, l.Mbit)
	}
	return ""
}

// storedLimit is parseLimit for a value already stored: anything unreadable counts as the default.
func storedLimit(s string) limitSetting {
	l, err := parseLimit(s)
	if err != nil {
		return limitSetting{Mode: limitAuto}
	}
	return l
}

// linkSide is what the decision needs to know about one end of a transfer.
type linkSide struct {
	Name      string // node name, for the sentences
	Iface     string // the interface the migration data of this node uses ("" unknown)
	Addr      string // its address on that interface, for the sentences
	Shared    bool   // that interface also carries the node's management address
	Own       string // the node's migration address as its Settings show it (the management address when none is chosen)
	SpeedMbps int    // link speed of the interface, 0 when unknown
	Limit     limitSetting
}

// limitDecision is the outcome for one transfer.
type limitDecision struct {
	Mbit    int    // the cap in Mbit/s, 0: not limited
	Applies bool   // at least one end shares its link with management (so a row is worth showing)
	Text    string // the pre-flight sentence ("" when Applies is false)
}

// sideCap is the cap one end asks for and the words for it.
type sideCap struct {
	mbit   int
	kind   string // auto | custom | none | unknown
	detail string // "80% of 1 Gb/s" or "set for n1"
}

func capOf(s linkSide) (sideCap, bool) {
	if !s.Shared {
		return sideCap{}, false
	}
	switch s.Limit.Mode {
	case limitNone:
		return sideCap{kind: limitNone}, true
	case limitCustom:
		return sideCap{mbit: s.Limit.Mbit, kind: limitCustom, detail: "custom limit"}, true
	}
	if s.SpeedMbps <= 0 {
		return sideCap{kind: "unknown"}, true
	}
	m := s.SpeedMbps * autoPercent / 100
	if m < 1 {
		m = 1
	}
	return sideCap{mbit: m, kind: limitAuto, detail: fmt.Sprintf("%d%% of %s", autoPercent, fmtLinkSpeed(s.SpeedMbps))}, true
}

// decideLimit combines the two ends of a transfer: each end that shares its link with management asks for
// a cap by its setting, and the lower cap wins. An end with its own interface asks for nothing.
func decideLimit(a, b linkSide) limitDecision {
	ca, sa := capOf(a)
	cb, sb := capOf(b)
	if !sa && !sb {
		return limitDecision{}
	}
	d := limitDecision{Applies: true}
	// The side that sets the cap; when only one end is shared, or both are alike, the node needs no name.
	var win sideCap
	var winSide linkSide
	switch {
	case sa && sb:
		switch {
		case ca.mbit > 0 && (cb.mbit == 0 || ca.mbit <= cb.mbit):
			win, winSide = ca, a
		case cb.mbit > 0:
			win, winSide = cb, b
		default:
			win, winSide = ca, a
			if ca.kind != cb.kind && ca.kind == limitNone {
				win, winSide = cb, b // "unknown" says more than "no limit" does
			}
		}
	case sa:
		win, winSide = ca, a
	default:
		win, winSide = cb, b
	}
	d.Mbit = win.mbit
	on := ""
	if sa && sb && (ca != cb) {
		on = " on " + winSide.Name
	}
	switch {
	case win.mbit > 0:
		d.Text = fmt.Sprintf("Shared with management%s: limited to %s (%s).", on, fmtMbit(win.mbit), win.detail)
	case win.kind == "unknown":
		d.Text = "Link speed unknown, so the transfer is not limited."
	default:
		d.Text = "Shared with management, but the limit is turned off: the transfer is not limited."
	}
	return d
}

func fmtMbit(m int) string { return fmt.Sprintf("%d Mbit/s", m) }

// fmtLinkSpeed writes a link speed the way network cards are sold: 100 Mb/s, 1 Gb/s, 2.5 Gb/s.
func fmtLinkSpeed(mbps int) string {
	if mbps < 1000 {
		return fmt.Sprintf("%d Mb/s", mbps)
	}
	g := float64(mbps) / 1000
	if mbps%1000 == 0 {
		return fmt.Sprintf("%d Gb/s", mbps/1000)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", g), "0"), ".") + " Gb/s"
}

// mbitToBytes turns Mbit/s into bytes per second (decimal megabits, as link speeds are).
func mbitToBytes(m int) uint64 { return uint64(m) * 125_000 }

// mbitToMiB turns Mbit/s into the whole MiB/s libvirt's bandwidth parameter takes, at least 1.
func mbitToMiB(m int) uint64 {
	v := uint64(m) * 125_000 / (1 << 20)
	if v < 1 {
		v = 1
	}
	return v
}

// bandwidthMiB is the limit for libvirt's migration bandwidth parameter in MiB/s: the request's own value
// when it has one, else the automatic cap; 0 means not limited.
func (p *migPlan) bandwidthMiB(req migReq) uint64 {
	if req.BandwidthMiB > 0 {
		return req.BandwidthMiB
	}
	if p.LimitMbit > 0 {
		return mbitToMiB(p.LimitMbit)
	}
	return 0
}

// rateBps is the same limit in bytes per second for the file copies.
func (p *migPlan) rateBps(req migReq) uint64 {
	if req.BandwidthMiB > 0 {
		return req.BandwidthMiB << 20
	}
	return mbitToBytes(p.LimitMbit)
}

// checkMigrationNetwork adds the pre-flight rows "Migration network" and, when a link is shared with
// management, "Transfer limit", and records the cap in the plan. Failing to read the interfaces of a node
// is a warning: the migration then goes ahead unlimited.
func (s *Server) checkMigrationNetwork(src, dst *hostconn.Host, req migReq, plan *migPlan, c *checkList) {
	t, err := s.transferBetween(src, dst)
	if err != nil {
		c.warn("Migration network", "could not read the network interfaces ("+firstLine(err.Error())+"), so the network and the transfer limit are not known; the transfer is not limited")
		return
	}
	c.pass("Migration network", t.Row)
	switch {
	case req.BandwidthMiB > 0:
		c.pass("Transfer limit", fmt.Sprintf("Limited to %d MiB/s by the request.", req.BandwidthMiB))
	case t.Limit.Applies:
		plan.LimitMbit = t.Limit.Mbit
		c.pass("Transfer limit", t.Limit.Text)
	}
}
