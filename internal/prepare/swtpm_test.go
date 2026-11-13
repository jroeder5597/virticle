package prepare

import (
	"strings"
	"testing"
)

func TestPackagesStepFixesTheSwtpmCAOwnerAndReportsIt(t *testing.T) {
	asked := false
	n := hostOnDebian(false, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "/var/lib/swtpm-localca") {
			asked = true
			return "fixed the owner of the swtpm local CA files (TPM guests could not start)\n@@changed", true, nil
		}
		return "", false, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	r := find(rows, "do-packages")
	if !asked || r.Status != OK || !strings.Contains(r.Detail, "installed; fixed the owner of the swtpm local CA files") {
		t.Errorf("packages row: asked=%v %+v", asked, r)
	}
	if s := find(rows, "summary"); strings.Contains(s.Detail, "Nothing changed") {
		t.Errorf("the fix must count as a change: %+v", s)
	}
}

func TestPackagesStepSwtpmCAQuietWhenFine(t *testing.T) {
	n := hostOnDebian(false, func(cmd, stdin string) (string, bool, error) {
		if strings.Contains(cmd, "/var/lib/swtpm-localca") {
			return "", true, nil // the script exits silently
		}
		return "", false, nil
	})
	rows := (&Runner{N: n}).Prepare(Options{ControllerKey: testKey}, nil, nil)
	r := find(rows, "do-packages")
	if strings.Contains(r.Detail, "swtpm") || !strings.HasPrefix(r.Detail, "Already in place") {
		t.Errorf("packages row: %+v", r)
	}
	if s := find(rows, "summary"); !strings.Contains(s.Detail, "Nothing changed") {
		t.Errorf("summary: %+v", s)
	}
}

func TestSwtpmCAScriptOnlyChownsToTss(t *testing.T) {
	for _, bad := range []string{"rm ", "chmod", "chown -R"} {
		if strings.Contains(swtpmCAScript, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	if !strings.Contains(swtpmCAScript, "id -u tss") {
		t.Error("the script must skip hosts without a tss account")
	}
}
