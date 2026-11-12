package api

import (
	"encoding/json"
	"testing"
)

// Fields the web UI no longer sends must mean the simple behaviour.
func TestRequestDefaults(t *testing.T) {
	br := defaultBackupReq()
	if err := json.Unmarshal([]byte(`{"pool":"p"}`), &br); err != nil {
		t.Fatal(err)
	}
	if br.KeepLast != 7 || br.SHA256 != nil || br.Quiesce != nil {
		t.Errorf("backup request: %+v (want keep 7, checksums and freeze automatic)", br)
	}
	if err := json.Unmarshal([]byte(`{"keepLast":0}`), &br); err != nil || br.KeepLast != 0 {
		t.Errorf("keepLast 0 must keep everything: %+v %v", br, err)
	}

	bs := defaultBackupSchedule()
	if err := json.Unmarshal([]byte(`{"mode":"daily","at":"03:00","pool":"p"}`), &bs); err != nil {
		t.Fatal(err)
	}
	if !bs.Enabled || bs.KeepLast != 7 {
		t.Errorf("backup schedule: %+v (want enabled, keep 7)", bs)
	}

	ss := defaultSnapSchedule()
	if err := json.Unmarshal([]byte(`{"mode":"interval","everyMinutes":360}`), &ss); err != nil {
		t.Fatal(err)
	}
	if !ss.Enabled || ss.Prefix != "auto" || ss.Keep != 7 || ss.Memory || ss.Quiesce {
		t.Errorf("snapshot schedule: %+v", ss)
	}
	if err := ss.Validate(); err != nil {
		t.Errorf("a schedule without the removed fields must validate: %v", err)
	}

	mv := defaultMoveReq()
	if err := json.Unmarshal([]byte(`{"pool":"p"}`), &mv); err != nil {
		t.Fatal(err)
	}
	if !mv.DeleteOld {
		t.Errorf("disk move must delete the old volume by default: %+v", mv)
	}
}
