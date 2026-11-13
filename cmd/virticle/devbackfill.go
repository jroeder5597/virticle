package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"virticle/internal/metrics"
)

// backfillCommand is a developer tool, not part of the documented command
// line: it writes synthetic history into metrics.db so that the longer
// ranges of the Performance tab can be looked at without waiting for days.
//
//	virticle dev-backfill-metrics -data DIR -node 1 -node-name n1 -vms uuid=name,uuid=name -days 7
func backfillCommand(args []string) int {
	fs := flag.NewFlagSet("dev-backfill-metrics", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory")
	node := fs.Int64("node", 1, "node number")
	nodeName := fs.String("node-name", "node", "node name")
	vms := fs.String("vms", "", "uuid=name,uuid=name")
	days := fs.Int("days", 7, "days to write")
	untilFlag := fs.String("until", "", "RFC 3339 time where the real data starts (default: now)")
	fs.Parse(args)
	list := map[string]string{}
	for _, kv := range strings.Split(*vms, ",") {
		if u, n, ok := strings.Cut(kv, "="); ok {
			list[u] = n
		}
	}
	d, err := metrics.Open(filepath.Join(*data, "metrics.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer d.Close()
	until := time.Now()
	if *untilFlag != "" {
		t, err := time.Parse(time.RFC3339, *untilFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		until = t
	}
	if err := (metrics.Backfill{VMs: list, Node: *node, NodeN: *nodeName, Days: *days, Until: until}).Run(d); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("wrote synthetic history up to", until.Format(time.RFC3339))
	return 0
}
