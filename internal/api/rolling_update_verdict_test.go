package api

import (
	"testing"

	"virticle/internal/nodeproto"
)

// What the run does with the update state of the node. A failed result of this run's own update is reported, and
// pUpdate then forgets that it started the update, so that Continue starts it again (before, Continue read the
// same failed result again and the update could never be retried).
func TestUpdateVerdict(t *testing.T) {
	const at = 1000
	cases := []struct {
		name string
		st   nodeproto.UpdateStatus
		want string
	}{
		{"nothing on the node", nodeproto.UpdateStatus{State: nodeproto.UpdateNone}, updateStartAgain},
		{"running", nodeproto.UpdateStatus{State: nodeproto.UpdateRunning, Started: at}, updateWait},
		{"failed just now", nodeproto.UpdateStatus{State: nodeproto.UpdateFailed, Started: at}, updateFailed},
		{"failed a little before the start was recorded", nodeproto.UpdateStatus{State: nodeproto.UpdateFailed, Started: at - 20}, updateFailed},
		{"an old result of an earlier run", nodeproto.UpdateStatus{State: nodeproto.UpdateOK, Started: at - 3600}, updateStartAgain},
		{"done", nodeproto.UpdateStatus{State: nodeproto.UpdateOK, Started: at}, updateDone},
	}
	for _, c := range cases {
		if got := updateVerdict(c.st, at); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
