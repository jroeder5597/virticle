package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The web page reads missing.length: a share that is on every node must give "missing":[], never null.
func TestSharedGroupJSONHasNoNulls(t *testing.T) {
	g := groupShared([]nodeNFS{{Node: 1, Pool: "s", Server: "a", Export: "/e", Target: "/mnt/s", State: "running"}}, []sharedMember{{Node: 1}})
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Errorf("%s", b)
	}
}
