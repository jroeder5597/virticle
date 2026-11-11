package prepare

import (
	"virticle/internal/cluster"
	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// Admin is the administrator's credential for one run. Nothing here is stored.
type Admin struct {
	Addr, User, Jump string
	Password         string // also used for "sudo -S" when the user is not root
	KeyPEM           []byte
	Agent            bool
}

// Dial opens the administrator's SSH connection. The jump host (if any) is
// reached the way the controller reaches jump hosts; only the final hop uses
// the administrator's credential, never the controller's own key.
func Dial(m *cluster.Manager, a Admin) (*hostconn.Conn, error) {
	t, err := m.Target(store.Node{Addr: a.Addr, User: a.User, Jump: a.Jump})
	if err != nil {
		return nil, err
	}
	t.KeyFile = ""
	t.Password, t.KeyPEM, t.Agent = a.Password, a.KeyPEM, a.Agent
	return hostconn.DialConn(t, m.KnownHostsFile())
}
