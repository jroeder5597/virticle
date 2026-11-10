// Package snapsched holds the pure logic of scheduled snapshots: the schedule
// stored in a domain's <metadata>, when a snapshot is due, and which old
// scheduled snapshots to prune. Nothing here talks to libvirt.
package snapsched

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// NS is the XML namespace for application data in domain metadata.
const NS = "https://virt-proj.invalid/ns/1"

// Marker is the description given to every scheduled snapshot. Together with
// the name pattern it tells scheduled snapshots from user-made ones.
const Marker = "Scheduled snapshot (virt-proj)"

const (
	rootName  = "meta"
	childName = "snapshot-schedule"
)

type Schedule struct {
	Enabled bool `json:"enabled"`
	// Mode is "interval" (every EveryMinutes) or "daily" (at At, UTC).
	Mode         string `json:"mode"`
	EveryMinutes int    `json:"everyMinutes,omitempty"`
	At           string `json:"at,omitempty"` // HH:MM, UTC
	Keep         int    `json:"keep"`
	Prefix       string `json:"prefix"`
	Memory       bool   `json:"memory"`
	Quiesce      bool   `json:"quiesce"`
}

var (
	prefixRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,30}$`)
	atRE     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	stampRE  = `[0-9]{8}-[0-9]{6}`
)

func (s Schedule) Validate() error {
	switch {
	case !prefixRE.MatchString(s.Prefix):
		return fmt.Errorf("prefix must be 1-31 characters: letters, digits, dot, dash, underscore, starting with a letter or digit")
	case s.Keep < 1 || s.Keep > 1000:
		return fmt.Errorf("keep must be between 1 and 1000")
	case s.Mode != "interval" && s.Mode != "daily":
		return fmt.Errorf("mode must be \"interval\" or \"daily\"")
	case s.Mode == "interval" && (s.EveryMinutes < 1 || s.EveryMinutes > 60*24*366):
		return fmt.Errorf("interval must be between 1 minute and one year")
	case s.Mode == "daily" && !atRE.MatchString(s.At):
		return fmt.Errorf("daily time must look like 03:30 (UTC)")
	}
	return nil
}

// Name is the snapshot name for a scheduled snapshot taken at t.
func Name(prefix string, t time.Time) string {
	return prefix + "-" + t.UTC().Format("20060102-150405")
}

// Due reports whether a snapshot should be taken now, given the creation time
// of the newest scheduled snapshot (zero if there is none).
func (s Schedule) Due(now, last time.Time) bool {
	if !s.Enabled {
		return false
	}
	switch s.Mode {
	case "interval":
		return last.IsZero() || !now.Before(last.Add(time.Duration(s.EveryMinutes)*time.Minute))
	case "daily":
		h, _ := strconv.Atoi(s.At[:2])
		m, _ := strconv.Atoi(s.At[3:])
		n := now.UTC()
		slot := time.Date(n.Year(), n.Month(), n.Day(), h, m, 0, 0, time.UTC)
		if slot.After(n) {
			slot = slot.AddDate(0, 0, -1)
		}
		return last.Before(slot)
	}
	return false
}

// Snap is the part of a snapshot the pruning logic needs.
type Snap struct {
	Name        string
	Description string
	Created     time.Time
}

// stampPrefix starts the suffix that names the controller that took a
// scheduled snapshot: "Scheduled snapshot (virt-proj) [controller 1a2b3c4d]".
// Snapshots from before the suffix existed carry the bare Marker.
const stampPrefix = Marker + " [controller "

// Stamp is the description for a scheduled snapshot taken by the controller
// with the given instance id (empty: the bare Marker).
func Stamp(id string) string {
	if id == "" {
		return Marker
	}
	return stampPrefix + id + "]"
}

// Controller returns the instance id in a scheduled snapshot's description,
// or "" for a bare Marker or anything that is not a scheduler description.
func Controller(desc string) string {
	if rest, ok := strings.CutPrefix(desc, stampPrefix); ok {
		if id, ok := strings.CutSuffix(rest, "]"); ok && id != "" && !strings.ContainsAny(id, "[] ") {
			return id
		}
	}
	return ""
}

func isMarker(desc string) bool { return desc == Marker || Controller(desc) != "" }

// Foreign returns the instance id of a controller other than self that took
// a scheduled snapshot of this prefix at or after since; "" if there is none.
// Snapshots without a stamp are not attributed to anyone.
func Foreign(snaps []Snap, prefix, self string, since time.Time) string {
	var id string
	var newest time.Time
	for _, sn := range snaps {
		if !Ours(sn, prefix) || sn.Created.Before(since) {
			continue
		}
		if c := Controller(sn.Description); c != "" && c != self && sn.Created.After(newest) {
			id, newest = c, sn.Created
		}
	}
	return id
}

// ForeignWindow is how long a snapshot by another controller counts as
// recent for a schedule: three intervals, at least ten minutes.
func (s Schedule) ForeignWindow() time.Duration {
	if s.Mode == "daily" {
		return 26 * time.Hour
	}
	d := 3 * time.Duration(s.EveryMinutes) * time.Minute
	if d < 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

// Ours reports whether sn was made by the scheduler for this prefix, by any
// controller (old bare-marker descriptions included).
func Ours(sn Snap, prefix string) bool {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "-" + stampRE + "$")
	return isMarker(sn.Description) && re.MatchString(sn.Name)
}

// Newest returns the creation time of the newest scheduled snapshot, or zero.
func Newest(snaps []Snap, prefix string) time.Time {
	var last time.Time
	for _, sn := range snaps {
		if Ours(sn, prefix) && sn.Created.After(last) {
			last = sn.Created
		}
	}
	return last
}

// Prune returns the names to delete, oldest first, so that only the newest
// keep scheduled snapshots with this prefix remain. User-made snapshots, and
// scheduled ones with another prefix, are never returned.
func Prune(snaps []Snap, prefix string, keep int) []string {
	var mine []Snap
	for _, sn := range snaps {
		if Ours(sn, prefix) {
			mine = append(mine, sn)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Created.Before(mine[j].Created) })
	var out []string
	for i := 0; i < len(mine)-keep; i++ {
		out = append(out, mine[i].Name)
	}
	return out
}

// --- domain metadata ---

type scheduleXML struct {
	XMLName      xml.Name `xml:"snapshot-schedule"`
	Enabled      string   `xml:"enabled,attr"`
	Mode         string   `xml:"mode,attr"`
	EveryMinutes int      `xml:"every-minutes,attr,omitempty"`
	At           string   `xml:"at,attr,omitempty"`
	Keep         int      `xml:"keep,attr"`
	Prefix       string   `xml:"prefix,attr"`
	Memory       string   `xml:"memory,attr"`
	Quiesce      string   `xml:"quiesce,attr"`
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (s Schedule) element() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<%s enabled=%q mode=%q`, childName, yn(s.Enabled), s.Mode)
	if s.Mode == "interval" {
		fmt.Fprintf(&b, ` every-minutes="%d"`, s.EveryMinutes)
	} else {
		fmt.Fprintf(&b, ` at=%q`, s.At)
	}
	fmt.Fprintf(&b, ` keep="%d" prefix=%q memory=%q quiesce=%q/>`, s.Keep, s.Prefix, yn(s.Memory), yn(s.Quiesce))
	return b.String()
}

// layout describes the metadata root: where its content starts and ends and
// where the schedule child sits inside it (child is -1 when absent).
type layout struct {
	inner, innerEnd  int
	child, childEnd  int
	hasOtherChildren bool
}

// scan parses the metadata libvirt returned for NS. libvirt hands back one
// root element carrying the namespace declaration; its children are plain
// (unprefixed) elements, which is also how they have to be written back.
func scan(meta string) (layout, error) {
	l := layout{child: -1, childEnd: -1, innerEnd: -1}
	d := xml.NewDecoder(strings.NewReader(meta))
	depth := 0
	for {
		before := int(d.InputOffset())
		tok, err := d.Token()
		if err != nil {
			if depth == 0 && l.innerEnd >= 0 {
				return l, nil
			}
			return l, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				l.inner = int(d.InputOffset())
			}
			if depth == 2 {
				if t.Name.Local == childName {
					l.child = before
					d.Skip()
					depth--
					l.childEnd = int(d.InputOffset())
				} else {
					l.hasOtherChildren = true
				}
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				l.innerEnd = before
			}
		}
	}
}

// Parse reads the schedule from the domain's metadata element for NS. An
// empty or foreign metadata string means "no schedule" (nil, nil).
func Parse(meta string) (*Schedule, error) {
	if strings.TrimSpace(meta) == "" {
		return nil, nil
	}
	l, err := scan(meta)
	if err != nil {
		return nil, fmt.Errorf("metadata is not well-formed XML: %w", err)
	}
	if l.child < 0 {
		return nil, nil
	}
	var x scheduleXML
	if err := xml.Unmarshal([]byte(meta[l.child:l.childEnd]), &x); err != nil {
		return nil, err
	}
	s := &Schedule{
		Enabled: x.Enabled == "yes", Mode: x.Mode, EveryMinutes: x.EveryMinutes, At: x.At,
		Keep: x.Keep, Prefix: x.Prefix, Memory: x.Memory == "yes", Quiesce: x.Quiesce == "yes",
	}
	return s, s.Validate()
}

// Empty reports whether the metadata holds nothing but an empty root, so the
// whole element can be dropped from the domain.
func Empty(meta string) bool {
	if strings.TrimSpace(meta) == "" {
		return true
	}
	l, err := scan(meta)
	return err == nil && l.child < 0 && !l.hasOtherChildren && strings.TrimSpace(meta[l.inner:l.innerEnd]) == ""
}

// Update returns new metadata for DomainSetMetadata with the schedule set (or
// removed when s is nil). It is a single root element WITHOUT namespace
// declaration, because libvirt adds the namespace itself and rejects one that
// is already declared. Other content of the root, which belongs to other
// features, is preserved byte for byte.
func Update(meta string, s *Schedule) (string, error) {
	elem := ""
	if s != nil {
		elem = s.element()
	}
	wrap := func(inner string) string { return "<" + rootName + ">" + inner + "</" + rootName + ">" }
	if strings.TrimSpace(meta) == "" {
		return wrap(elem), nil
	}
	l, err := scan(meta)
	if err != nil {
		return "", fmt.Errorf("existing metadata is not well-formed XML: %w", err)
	}
	inner := meta[l.inner:l.innerEnd]
	if l.child >= 0 {
		inner = meta[l.inner:l.child] + elem + meta[l.childEnd:l.innerEnd]
	} else {
		inner += elem
	}
	// libvirt stores the children with the "vp:" prefix; written back they
	// must be plain again, since the prefix is only declared by the root.
	inner = strings.NewReplacer("<vp:", "<", "</vp:", "</").Replace(inner)
	return wrap(inner), nil
}
