package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every route that changes a VM by its UUID must reach the VM through the guard that refuses a replica
// (Server.domain, directly or through hwContext and snapshot, which call it). The few routes that act on replicas
// on purpose use domainAny and are listed here; a new route that reaches a domain another way fails this test.
func TestEveryVMChangeGoesThroughTheReplicaGuard(t *testing.T) {
	srcs := map[string]string{}
	files, _ := filepath.Glob("*.go")
	var all strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs[f] = string(b)
		all.WriteString(string(b))
		all.WriteString("\n")
	}
	server := srcs["server.go"]
	route := regexp.MustCompile(`auth\("(POST|PUT|DELETE|PATCH) (/api/nodes/\{node\}/vms/\{uuid\}[^"]*)", (?:s\.needs\("[A-Za-z]+", )?s\.([A-Za-z0-9]+)`)
	// Handlers that are meant to act on a replica (they use domainAny), and why.
	onReplicas := map[string]string{
		"replicaFailover":      "starts the VM from its copy",
		"deleteReplicaHandler": "deletes the copy",
		"replicationCancel":    "cancels a run that writes the copy",
	}
	guarded := regexp.MustCompile(`s\.domain\(r\)|s\.hwContext\(r\)|s\.snapshot\(r\)|s\.hwDomain\(r\)`)
	helperCall := regexp.MustCompile(`s\.([A-Za-z0-9]+)\(w, r[,)]`)
	n := 0
	for _, m := range route.FindAllStringSubmatch(server, -1) {
		method, path, h := m[1], m[2], m[3]
		n++
		if _, ok := onReplicas[h]; ok {
			continue
		}
		body := funcBody(all.String(), h)
		if body == "" {
			t.Errorf("%s %s: handler %s not found", method, path, h)
			continue
		}
		ok := reaches(all.String(), body, guarded, helperCall, 3)
		if strings.Contains(body, "domainAny(") {
			t.Errorf("%s %s: handler %s uses domainAny", method, path, h)
		}
		if !ok {
			t.Errorf("%s %s: handler %s does not reach the VM through the replica guard (s.domain)", method, path, h)
		}
		if strings.Contains(body, "domainAny(") {
			t.Errorf("%s %s: handler %s uses domainAny and bypasses the replica guard", method, path, h)
		}
	}
	if n < 40 {
		t.Errorf("only %d routes found: the pattern no longer matches server.go", n)
	}
	// The helpers the others rely on must themselves go through the guard.
	for _, h := range []string{"hwContext", "snapshot"} {
		if b := funcBody(all.String(), h); !strings.Contains(b, "s.domain(r)") {
			t.Errorf("helper %s does not call s.domain(r)", h)
		}
	}
}

// funcBody returns the text of method name of *Server up to the closing brace at column 0.
func funcBody(src, name string) string {
	i := strings.Index(src, "func (s *Server) "+name+"(")
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

// reaches says whether the guard is called by body or by a helper it hands the request to (up to depth levels).
func reaches(src, body string, guarded, helperCall *regexp.Regexp, depth int) bool {
	if guarded.MatchString(body) {
		return true
	}
	if depth == 0 {
		return false
	}
	for _, hm := range helperCall.FindAllStringSubmatch(body, -1) {
		hb := funcBody(src, hm[1])
		if strings.Contains(hb, "domainAny(") {
			return false
		}
		if reaches(src, hb, guarded, helperCall, depth-1) {
			return true
		}
	}
	return false
}
