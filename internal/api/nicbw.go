package api

import (
	"fmt"

	"virticle/internal/domxml"
)

// Speed limit of a VM network card. The user thinks in Mbit/s; libvirt's
// <bandwidth> wants kilobytes per second (kilo = 1000), which the host turns
// into tc rules. Direction is from the guest's side: "down" is traffic to the
// guest (libvirt inbound), "up" is traffic from the guest (libvirt outbound).

const maxLimitMbit = 100000

// nicLimit is a request to set the limit of a card: Mbit/s per direction, 0 for
// no limit in that direction. Absent from a request means "leave it as it is".
type nicLimit struct {
	Down int `json:"down"`
	Up   int `json:"up"`
}

// mbitToKBps is the only place Mbit/s becomes libvirt's kilobytes per second:
// 1 Mbit/s = 1000000 bit/s = 125000 byte/s = 125 kB/s.
func mbitToKBps(mbit int) uint64 { return uint64(mbit) * 125 }

// kbpsToMbit is the reverse, for display. A hand-made limit need not be a
// whole number of Mbit/s.
func kbpsToMbit(kbps uint64) float64 { return float64(kbps) / 125 }

// limitBurstKB is the burst written next to an average of kbps kilobytes per
// second. Peak is left unset on purpose (it then equals the average).
//
// Burst = 200 ms worth of the average, at least 128 kB and at most 65536 kB.
//   - A one-second burst would let a short transfer (a test download of a few
//     megabytes) pass at full speed and look unlimited; 200 ms keeps the limit
//     visible within a second or two while still absorbing normal bursts.
//   - The floor keeps tc's policer and token bucket working at low rates, where
//     200 ms is smaller than a handful of full-size packets.
//   - The cap keeps the bucket sane on very fast limits.
func limitBurstKB(kbps uint64) uint64 {
	b := kbps / 5
	if b < 128 {
		b = 128
	}
	if b > 65536 {
		b = 65536
	}
	return b
}

// rateOf is the libvirt rate for a limit in Mbit/s; 0 gives no limit.
func rateOf(mbit int) domxml.Rate {
	if mbit == 0 {
		return domxml.Rate{}
	}
	avg := mbitToKBps(mbit)
	return domxml.Rate{Average: avg, Burst: limitBurstKB(avg)}
}

// check says why a limit cannot be applied.
func (l nicLimit) check() error {
	for _, v := range []int{l.Down, l.Up} {
		if v < 0 || v > maxLimitMbit {
			return badRequest(fmt.Sprintf("a speed limit is a whole number from 1 to %d Mbit/s; leave it empty for no limit", maxLimitMbit))
		}
	}
	return nil
}

// xml is the <bandwidth> element for a new card, "" when there is no limit.
func (l nicLimit) xml() string { return domxml.BandwidthXML(rateOf(l.Down), rateOf(l.Up)) }
