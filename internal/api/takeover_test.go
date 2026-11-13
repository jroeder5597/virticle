package api

import (
	"testing"

	"virticle/internal/migpki"
)

// The decision for the certificate files of a node that may have been taken over from another controller.
func TestMigAutoAction(t *testing.T) {
	cases := []struct {
		name                            string
		state                           string
		addrOnly, takenOver             bool
		wantInstall, wantForce, wantErr bool
	}{
		{"fine", migpki.StateOK, false, false, false, false, false},
		{"not set up", migpki.StateNotSetUp, false, false, true, false, false},
		{"expiring", migpki.StateExpiring, false, false, true, false, false},
		{"foreign files are left alone and reported", migpki.StateMismatch, false, false, false, false, true},
		{"only the address changed", migpki.StateMismatch, true, false, true, false, false},
		{"taken over from another controller: replace, keep the old files", migpki.StateMismatch, false, true, true, true, false},
	}
	for _, c := range cases {
		install, force, err := migAutoAction(c.state, c.addrOnly, c.takenOver)
		if install != c.wantInstall || force != c.wantForce || (err != nil) != c.wantErr {
			t.Errorf("%s: install=%v force=%v err=%v", c.name, install, force, err)
		}
	}
}

func TestFromOtherCA(t *testing.T) {
	other := migpki.Status{State: migpki.StateMismatch, Problems: []string{"set up by a different controller or CA (fingerprint ab12, this controller's is cd34)"}}
	files := migpki.Status{State: migpki.StateMismatch, Problems: []string{"/etc/pki/qemu already contains server-key.pem, which this controller did not create"}}
	if !fromOtherCA(other) || fromOtherCA(files) {
		t.Error("only a marker from another CA counts as another controller's setup")
	}
}

func TestAddNodeTakeOverFlag(t *testing.T) {
	e := newEnv(t)
	e.user("ann", admin)
	w := e.do("POST", "/api/nodes", map[string]any{"addr": "127.0.0.1:1", "user": "root", "takeOver": true}, cookieOf(e, "ann"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var n struct{ ID int64 }
	decode(t, w, &n)
	if !e.srv.tookOver(n.ID) {
		t.Error("the take-over was not remembered")
	}
	e.srv.takeOverDone(n.ID)
	if e.srv.tookOver(n.ID) {
		t.Error("still remembered")
	}
	w = e.do("POST", "/api/nodes", map[string]any{"addr": "127.0.0.1:2", "user": "root"}, cookieOf(e, "ann"))
	decode(t, w, &n)
	if w.Code != 200 || e.srv.tookOver(n.ID) {
		t.Errorf("a plain add is not a take-over: %d", w.Code)
	}
}
