package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidatePrefs(t *testing.T) {
	parse := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	good := []string{`{"tasksDock":"open"}`, `{"tasksDock":"closed","consoleClick":"inline"}`}
	for _, g := range good {
		if _, err := validatePrefs(parse(g)); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
	bad := []string{
		`{}`,
		`{"tasksDock":"sideways"}`,
		`{"tasksDock":true}`,
		`{"tasksDock":null}`,
		`{"nope":"open"}`,
		`{"tasksDock":"open","nope":"x"}`,
		`{"` + strings.Repeat("k", 5000) + `":"x"}`,
		`{"tasksDock":"` + strings.Repeat("o", 5000) + `"}`,
	}
	for _, b := range bad {
		if _, err := validatePrefs(parse(b)); err == nil {
			t.Errorf("%.60s: accepted", b)
		}
	}
	many := map[string]json.RawMessage{}
	for i := 0; i < prefMaxKeys+1; i++ {
		many[string(rune('a'+i))] = json.RawMessage(`"x"`)
	}
	if _, err := validatePrefs(many); err == nil {
		t.Error("too many keys accepted")
	}
}

func TestValidatePrefsSizes(t *testing.T) {
	ok := map[string]string{"sidebarW": `"320"`, "dockH": `""`}
	in := map[string]json.RawMessage{}
	for k, v := range ok {
		in[k] = json.RawMessage(v)
	}
	out, err := validatePrefs(in)
	if err != nil || out["sidebarW"] != "320" || out["dockH"] != "" {
		t.Fatalf("got %v, %v", out, err)
	}
	for _, bad := range []string{`"12"`, `"99999"`, `"3e2"`, `"0320"`, `"wide"`, `320`} {
		if _, err := validatePrefs(map[string]json.RawMessage{"sidebarW": json.RawMessage(bad)}); err == nil {
			t.Errorf("sidebarW=%s was accepted", bad)
		}
	}
}
