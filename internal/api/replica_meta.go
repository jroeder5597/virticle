package api

// What the application writes into a domain's metadata for replication. Four
// children of the one application element (see appmeta.go), none of which an
// older Virticle reads or drops:
//
//	replication   on the VM that is copied: where to, how often, and the last result
//	replica       on the standby's copy: it is inert (the state word "replica")
//	stale-copy    on the old VM after the standby took over: it must not run again
//	failed-over   on the VM that took over, to find the old one when its node returns

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"
)

const (
	replChild     = "replication"
	replicaChild  = "replica"
	staleChild    = "stale-copy"
	failedOverChd = "failed-over"
)

// replConfig is the replication of a VM, as stored on it.
type replConfig struct {
	Enabled      bool
	TargetNode   int64
	TargetName   string
	Pool         string
	EveryMinutes int
	Dir          string // the replica directory on the standby node
	// ReplaceStale says the standby holds an old copy of this very VM (after a failover) that the
	// first complete copy replaces.
	ReplaceStale bool
	// The last successful run.
	LastOK    int64 // unix time of the point
	LastBytes uint64
	LastKind  string
	LastSecs  int
}

func (c replConfig) element() string {
	el := fmt.Sprintf(`<%s enabled=%q target-node="%d" target-name=%q pool=%q every-minutes="%d" dir=%q`,
		replChild, yesNo(c.Enabled), c.TargetNode, c.TargetName, c.Pool, c.EveryMinutes, c.Dir)
	if c.ReplaceStale {
		el += ` replace-stale="yes"`
	}
	if c.LastOK > 0 {
		el += fmt.Sprintf(` last="%d" last-bytes="%d" last-kind=%q last-secs="%d"`, c.LastOK, c.LastBytes, c.LastKind, c.LastSecs)
	}
	return el + "/>"
}

func (c replConfig) validate() error {
	switch {
	case c.TargetNode <= 0:
		return fmt.Errorf("a standby node is required")
	case !poolNameRE.MatchString(c.Pool):
		return fmt.Errorf("a standby pool is required")
	}
	return replIntervalOK(c.EveryMinutes)
}

// parseReplication reads the replication of a VM from its application metadata; nil when there is none.
func parseReplication(meta string) *replConfig {
	if meta == "" {
		return nil
	}
	var root struct {
		R *struct {
			Enabled string `xml:"enabled,attr"`
			Node    int64  `xml:"target-node,attr"`
			Name    string `xml:"target-name,attr"`
			Pool    string `xml:"pool,attr"`
			Every   int    `xml:"every-minutes,attr"`
			Dir     string `xml:"dir,attr"`
			Replace string `xml:"replace-stale,attr"`
			Last    int64  `xml:"last,attr"`
			Bytes   uint64 `xml:"last-bytes,attr"`
			Kind    string `xml:"last-kind,attr"`
			Secs    int    `xml:"last-secs,attr"`
		} `xml:"replication"`
	}
	if xml.Unmarshal([]byte(meta), &root) != nil || root.R == nil {
		return nil
	}
	r := root.R
	return &replConfig{Enabled: r.Enabled == "yes", TargetNode: r.Node, TargetName: r.Name, Pool: r.Pool, EveryMinutes: r.Every,
		Dir: r.Dir, ReplaceStale: r.Replace == "yes", LastOK: r.Last, LastBytes: r.Bytes, LastKind: r.Kind, LastSecs: r.Secs}
}

// replicaMeta marks the standby's copy.
type replicaMeta struct {
	Dir        string
	Pool       string
	SourceNode int64
	SourceName string
	Every      int
	Last       int64 // time of the newest complete point, 0 = none yet
	Checked    int64 // the last time the copy was found to be current (a shut-off VM that did not change needs no new point)
	Points     int
	Bytes      uint64 // size of the replica on disk
}

func (m replicaMeta) element() string {
	return fmt.Sprintf(`<%s dir=%q pool=%q source-node="%d" source-name=%q every-minutes="%d" last="%d" checked="%d" points="%d" bytes="%d"/>`,
		replicaChild, m.Dir, m.Pool, m.SourceNode, m.SourceName, m.Every, m.Last, m.Checked, m.Points, m.Bytes)
}

func parseReplicaMeta(meta string) *replicaMeta {
	if meta == "" {
		return nil
	}
	var root struct {
		R *struct {
			Dir     string `xml:"dir,attr"`
			Pool    string `xml:"pool,attr"`
			Node    int64  `xml:"source-node,attr"`
			Name    string `xml:"source-name,attr"`
			Every   int    `xml:"every-minutes,attr"`
			Last    int64  `xml:"last,attr"`
			Checked int64  `xml:"checked,attr"`
			Points  int    `xml:"points,attr"`
			Bytes   uint64 `xml:"bytes,attr"`
		} `xml:"replica"`
	}
	if xml.Unmarshal([]byte(meta), &root) != nil || root.R == nil {
		return nil
	}
	r := root.R
	return &replicaMeta{Dir: r.Dir, Pool: r.Pool, SourceNode: r.Node, SourceName: r.Name, Every: r.Every, Last: r.Last, Checked: r.Checked, Points: r.Points, Bytes: r.Bytes}
}

// staleMeta marks the old VM after another node took over.
type staleMeta struct {
	ToNode int64
	ToName string
	At     int64
}

func (m staleMeta) element() string {
	return fmt.Sprintf(`<%s to-node="%d" to-name=%q at="%d"/>`, staleChild, m.ToNode, m.ToName, m.At)
}

func parseStale(meta string) *staleMeta {
	if meta == "" {
		return nil
	}
	var root struct {
		S *struct {
			Node int64  `xml:"to-node,attr"`
			Name string `xml:"to-name,attr"`
			At   int64  `xml:"at,attr"`
		} `xml:"stale-copy"`
	}
	if xml.Unmarshal([]byte(meta), &root) != nil || root.S == nil {
		return nil
	}
	return &staleMeta{ToNode: root.S.Node, ToName: root.S.Name, At: root.S.At}
}

// failedOverMeta is kept by the VM that took over.
type failedOverMeta struct {
	FromNode int64
	FromName string
	At       int64
	Planned  bool
}

func (m failedOverMeta) element() string {
	return fmt.Sprintf(`<%s from-node="%d" from-name=%q at="%d" planned=%q/>`, failedOverChd, m.FromNode, m.FromName, m.At, yesNo(m.Planned))
}

func parseFailedOver(meta string) *failedOverMeta {
	if meta == "" {
		return nil
	}
	var root struct {
		F *struct {
			Node    int64  `xml:"from-node,attr"`
			Name    string `xml:"from-name,attr"`
			At      int64  `xml:"at,attr"`
			Planned string `xml:"planned,attr"`
		} `xml:"failed-over"`
	}
	if xml.Unmarshal([]byte(meta), &root) != nil || root.F == nil {
		return nil
	}
	return &failedOverMeta{FromNode: root.F.Node, FromName: root.F.Name, At: root.F.At, Planned: root.F.Planned == "yes"}
}

// setMetaChildren replaces several children of the application metadata in one go: a child with an
// empty element is removed. It is the read, change, write of metaSetChild for more than one child.
func setMetaChildren(meta string, changes map[string]string) (string, error) {
	var err error
	// A stable order, so tests and the stored XML do not shuffle.
	for _, name := range []string{replChild, replicaChild, staleChild, failedOverChd} {
		el, ok := changes[name]
		if !ok {
			continue
		}
		if meta, err = metaSetChild(meta, name, el); err != nil {
			return "", err
		}
	}
	return meta, nil
}

// --- what the lists show ---

// replSummary is the replication of a VM as the VM list shows it.
type replSummary struct {
	TargetNode   int64  `json:"targetNode"`
	TargetName   string `json:"targetName"`
	Pool         string `json:"pool"`
	EveryMinutes int    `json:"everyMinutes"`
	Enabled      bool   `json:"enabled"`
	LastOK       int64  `json:"lastOk,omitempty"`
	LastBytes    uint64 `json:"lastBytes,omitempty"`
	LastKind     string `json:"lastKind,omitempty"`
	LastSecs     int    `json:"lastSecs,omitempty"`
	// Next is the unix time the next run is due (0 when the scheduler is off).
	Next int64 `json:"next,omitempty"`
}

// replicaSummary is the standby's copy as the VM list shows it.
type replicaSummary struct {
	SourceNode int64  `json:"sourceNode"`
	SourceName string `json:"sourceName"`
	Every      int    `json:"everyMinutes"`
	Last       int64  `json:"last,omitempty"`
	Checked    int64  `json:"checked,omitempty"`
	Points     int    `json:"points"`
	Bytes      uint64 `json:"bytes"`
	// Stale is the age in seconds when the copy is older than twice its interval (0 = fresh).
	Late bool `json:"late,omitempty"`
}

// replicaLate says a copy is older than twice its interval (plus a minute, for the run itself).
func replicaLate(last int64, every int, now time.Time) bool {
	if every <= 0 {
		return false
	}
	if last == 0 {
		return false // nothing yet: the first copy is on its way
	}
	return now.Sub(time.Unix(last, 0)) > 2*time.Duration(every)*time.Minute+time.Minute
}

func unixStr(t int64) string { return strconv.FormatInt(t, 10) }
