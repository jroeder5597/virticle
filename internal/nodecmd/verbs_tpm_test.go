package nodecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tpmTestUUID = "011a9fe8-ad36-45cc-8f45-ef934f875a1c"

func TestTPMSaveAndPlace(t *testing.T) {
	n := newNode(t)
	work := n.workDir(".virticle-rp.TTTTTT")
	state := n.abs("swtpm/" + tpmTestUUID + "/tpm2/tpm2-00.permall")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("tpm state one"), 0o600); err != nil {
		t.Fatal(err)
	}
	// save: a copy appears in the work directory
	if out := n.ok("", "tpm-save", tpmTestUUID, work+"/t1.tpm"); out != "" {
		t.Errorf("save said %q", out)
	}
	if b, err := os.ReadFile(work + "/t1.tpm"); err != nil || string(b) != "tpm state one" {
		t.Fatalf("copy %q %v", b, err)
	}
	if st, _ := os.Stat(work + "/t1.tpm"); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	// a VM that never ran has no state: NONE, nothing written
	if out := n.ok("", "tpm-save", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", work+"/t2.tpm"); out != "NONE" {
		t.Errorf("no state gave %q", out)
	}
	if _, err := os.Stat(work + "/t2.tpm"); err == nil {
		t.Error("a file was written for a VM without state")
	}
	// what is refused
	n.denied("a path as the uuid", "", "tpm-save", "../x", work+"/t3.tpm")
	n.denied("upper case uuid", "", "tpm-save", strings.ToUpper(tpmTestUUID), work+"/t3.tpm")
	n.denied("destination outside the roots", "", "tpm-save", tpmTestUUID, n.abs("outside/t.tpm"))
	n.denied("a path as the uuid (place)", "", "tpm-place", work+"/t1.tpm", "../../x")
	// a link in place of the state file is never followed
	other := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if err := os.MkdirAll(n.abs("swtpm/"+other+"/tpm2"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.abs("outside/secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(n.abs("outside/secret"), n.abs("swtpm/"+other+"/tpm2/tpm2-00.permall")); err != nil {
		t.Fatal(err)
	}
	if out, code := n.run("", "tpm-save", other, work+"/t4.tpm"); code == 0 {
		t.Errorf("followed a link: %s", out)
	}

	// place: a new VM directory is made, then the state is replaced in one step
	third := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	n.ok("", "tpm-place", work+"/t1.tpm", third)
	got := n.abs("swtpm/" + third + "/tpm2/tpm2-00.permall")
	if b, err := os.ReadFile(got); err != nil || string(b) != "tpm state one" {
		t.Fatalf("placed %q %v", b, err)
	}
	if st, _ := os.Stat(got); st.Mode().Perm() != 0o600 {
		t.Errorf("placed mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(n.abs("swtpm/" + third)); st.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", st.Mode().Perm())
	}
	if err := os.WriteFile(work+"/t5.tpm", []byte("tpm state two"), 0o600); err != nil {
		t.Fatal(err)
	}
	n.ok("", "tpm-place", work+"/t5.tpm", third)
	if b, _ := os.ReadFile(got); string(b) != "tpm state two" {
		t.Errorf("not replaced: %q", b)
	}
	ents, _ := filepath.Glob(n.abs("swtpm/" + third + "/tpm2/*"))
	if len(ents) != 1 {
		t.Errorf("leftovers: %v", ents)
	}
	// too large, or not in a root
	big := make([]byte, tpmStateMax+1)
	if err := os.WriteFile(work+"/big.tpm", big, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := n.run("", "tpm-place", work+"/big.tpm", third); code == 0 {
		t.Errorf("placed a huge file: %s", out)
	}
	n.denied("source outside the roots", "", "tpm-place", n.abs("outside/secret"), third)
}
