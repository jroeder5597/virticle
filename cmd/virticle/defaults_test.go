package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"virticle/internal/store"
	"virticle/internal/webaccess"
)

// -data: the packages' directory when it holds a database, else "data".
func TestPickDataDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if got := pickDataDir(missing); got != "data" {
		t.Errorf("missing directory: %q", got)
	}
	empty := t.TempDir()
	if got := pickDataDir(empty); got != "data" {
		t.Errorf("directory without a database: %q", got)
	}
	with := t.TempDir()
	if err := os.WriteFile(filepath.Join(with, store.DBName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pickDataDir(with); got != with {
		t.Errorf("directory with a database: %q", got)
	}
}

// The listener without flags: stored settings win; an existing installation
// without stored settings keeps loopback HTTP and is told; a new one gets
// HTTPS with a self-signed certificate on all interfaces.
func TestChooseSettings(t *testing.T) {
	stored := webaccess.Settings{Mode: "own", HTTPSListen: "10.0.0.5:443"}
	c := chooseSettings(stored, true, true)
	if c.settings.Mode != "own" || c.source != "stored" || c.notice != "" || c.persist {
		t.Errorf("stored: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, true)
	if c.settings.Mode != webaccess.ModeOff || c.settings.Listen != "127.0.0.1:8080" || c.notice == "" || c.persist {
		t.Errorf("upgrade without stored settings: %+v", c)
	}
	c = chooseSettings(webaccess.Settings{}, false, false)
	if c.settings.Mode != webaccess.ModeSelfSigned || c.settings.HTTPSListen != ":8443" || c.notice != "" || !c.persist {
		t.Errorf("fresh install: %+v", c)
	}
}

func TestSetupHint(t *testing.T) {
	ws := webaccess.FirstRunDefaults()
	if got := setupHint(false, ws, []string{"10.0.0.5", "vm1"}); got != "open https://10.0.0.5:8443 to create the administrator" {
		t.Errorf("%q", got)
	}
	if got := setupHint(true, ws, nil); got != "" {
		t.Errorf("with an administrator: %q", got)
	}
	d := webaccess.Defaults()
	if got := setupHint(false, d, nil); !strings.HasPrefix(got, "open http://127.0.0.1:8080 ") {
		t.Errorf("%q", got)
	}
}
