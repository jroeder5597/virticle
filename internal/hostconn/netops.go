package hostconn

// Typed calls of the host network verbs of the node command (net-info,
// net-plan, net-apply, net-confirm, net-rollback, net-status). A node that was
// prepared by an older version has no such verbs and no node command access at
// all (the protocol version differs): ErrNoPrivilege comes back, which the
// caller shows as "prepare the node again".

import (
	"encoding/json"
	"errors"
	"fmt"

	"virticle/internal/nodeproto"
)

func (h *Host) netJSON(stdin, verb string, v any, args ...string) error {
	out, err := h.RunNodeInput(stdin, verb, args...)
	if err != nil {
		return nodeErr(out, err)
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("unexpected answer of %s: %s", verb, firstLineOf(out, errors.New("empty")))
	}
	return nil
}

func netRequestJSON(req nodeproto.NetRequest) string {
	b, _ := json.Marshal(req)
	return string(b)
}

// NetInfo describes the node's network configuration system, its bridges and the status of changes.
func (h *Host) NetInfo() (nodeproto.NetInfo, error) {
	var v nodeproto.NetInfo
	err := h.netJSON("", "net-info", &v)
	return v, err
}

// NetPlan asks what a change would do. Nothing is changed.
func (h *Host) NetPlan(req nodeproto.NetRequest) (nodeproto.NetPlan, error) {
	var v nodeproto.NetPlan
	err := h.netJSON(netRequestJSON(req), "net-plan", &v)
	return v, err
}

// NetApply saves the node's network state, arms its rollback timer and starts the change. The node may be
// unreachable for a while after this returns.
func (h *Host) NetApply(req nodeproto.NetRequest) (nodeproto.NetApplyResult, error) {
	var v nodeproto.NetApplyResult
	err := h.netJSON(netRequestJSON(req), "net-apply", &v)
	return v, err
}

// NetStatus returns the pending change and the last ones.
func (h *Host) NetStatus() (nodeproto.NetStatus, error) {
	var v nodeproto.NetStatus
	err := h.netJSON("", "net-status", &v)
	return v, err
}

// NetConfirm makes a change permanent and cancels its rollback timer.
func (h *Host) NetConfirm(id string) error {
	out, err := h.RunNode("net-confirm", id)
	if err != nil {
		return nodeErr(out, err)
	}
	return nil
}

// NetRollback restores the saved state of a change.
func (h *Host) NetRollback(id string) error {
	out, err := h.RunNode("net-rollback", id)
	if err != nil {
		return nodeErr(out, err)
	}
	return nil
}
