package prepare

import (
	"time"

	"virticle/internal/cluster"
	"virticle/internal/store"
)

// Verify runs the connection test as the service account (the controller's
// own key, restrictions and all) and returns its steps as rows.
func Verify(m *cluster.Manager, addr, user, jump string) []Row {
	res, err := m.TestConnect(store.Node{Addr: addr, User: user, Jump: jump}, 30*time.Second)
	var rows []Row
	for _, s := range res.Steps {
		st := s.Status
		if st == cluster.StepSkipped {
			st = Skipped
		}
		d := s.Detail
		if st == Skipped {
			d = "Not run"
		}
		rows = append(rows, Row{ID: "test-" + s.ID, Title: s.Title, Status: st, Detail: d})
	}
	if len(rows) == 0 && err != nil {
		rows = append(rows, Row{ID: "test", Title: "Test connection", Status: Error, Detail: err.Error()})
	}
	return rows
}
