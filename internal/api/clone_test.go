package api

import (
	"strings"
	"testing"
)

func TestChooseSysprep(t *testing.T) {
	cases := []struct {
		guest   string
		mark    templateMark
		have    bool
		want    string
		wantErr string
	}{
		{"linux", templateMark{Template: true, Sealed: "linux"}, false, mechSealed, ""},
		{"linux", templateMark{Template: true, Sealed: "linux"}, true, mechSealed, ""},
		{"linux", templateMark{}, true, mechVirtSysprep, ""},
		{"linux", templateMark{}, false, "", "Seal and convert"},
		{"windows", templateMark{Template: true, Sealed: "windows"}, true, mechSealed, ""},
		{"windows", templateMark{}, true, "", "Windows itself"},
		{"linux", templateMark{Template: true, Sealed: "windows"}, true, mechVirtSysprep, ""},
	}
	for i, c := range cases {
		got, err := chooseSysprep(c.guest, c.mark, c.have)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("case %d: err = %v, want %q", i, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("case %d: got %q, %v want %q", i, got, err, c.want)
		}
	}
}

func TestGuestFamily(t *testing.T) {
	if g := guestFamily("", templateMark{}, "<domain/>"); g != "linux" {
		t.Error(g)
	}
	if g := guestFamily("", templateMark{Sealed: "windows"}, ""); g != "windows" {
		t.Error(g)
	}
	if g := guestFamily("", templateMark{}, `<libosinfo:os id="http://microsoft.com/win/11"/>`); g != "windows" {
		t.Error(g)
	}
	if g := guestFamily("linux", templateMark{Sealed: "windows"}, ""); g != "linux" {
		t.Error(g)
	}
}

func TestBuildSeed(t *testing.T) {
	img, err := buildSeed("linux", "c1", cloneCustomize{User: "bob", Password: "x", SSHKeys: []string{"ssh-ed25519 AAAA k"}})
	if err != nil || !strings.Contains(strings.ToLower(string(img)), "cidata") {
		t.Fatalf("linux seed: %v", err)
	}
	img, err = buildSeed("windows", "WIN1", cloneCustomize{User: "adm", Password: "pw"})
	if err != nil || !strings.Contains(string(img), "UNATTEND") {
		t.Fatalf("windows seed: %v", err)
	}
	if _, err = buildSeed("windows", "WIN1", cloneCustomize{User: "adm", Password: "pw", SSHKeys: []string{"ssh-ed25519 AAAA k"}}); err == nil {
		t.Error("ssh keys should be refused for Windows")
	}
	if _, err = buildSeed("linux", "c1", cloneCustomize{User: "Bad User"}); err == nil {
		t.Error("bad user should be refused")
	}
}

func TestShellQuote(t *testing.T) {
	if g := shellQuote("a'b c"); g != `'a'\''b c'` {
		t.Error(g)
	}
}

func TestMetaSetChild(t *testing.T) {
	const stored = `<vp:meta xmlns:vp="https://virt-proj.invalid/ns/1"><vp:snapshot-schedule enabled="yes"/></vp:meta>`
	got, err := metaSetChild(stored, "template", `<template sealed="linux"/>`)
	if err != nil || got != `<meta><snapshot-schedule enabled="yes"/><template sealed="linux"/></meta>` {
		t.Fatalf("add: %q %v", got, err)
	}
	got, _ = metaSetChild(`<vp:meta xmlns:vp="x"><vp:template/><vp:snapshot-schedule enabled="yes"/></vp:meta>`, "template", "")
	if got != `<meta><snapshot-schedule enabled="yes"/></meta>` {
		t.Fatalf("remove: %q", got)
	}
	if got, _ = metaSetChild(`<vp:meta xmlns:vp="x"><vp:template/></vp:meta>`, "template", ""); got != "" {
		t.Fatalf("last child removed: %q", got)
	}
	if got, _ = metaSetChild("", "template", "<template/>"); got != "<meta><template/></meta>" {
		t.Fatalf("empty: %q", got)
	}
}
