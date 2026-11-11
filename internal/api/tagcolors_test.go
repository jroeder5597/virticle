package api

import (
	"reflect"
	"testing"
)

func TestColorRE(t *testing.T) {
	for _, ok := range []string{"#000000", "#ffffff", "#A1b2C3"} {
		if !colorRE.MatchString(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "000000", "#fff", "#12345", "#1234567", "#gggggg", "red", "#ffffff\n", " #ffffff", "#ffffff;", "javascript:1", "url(x)"} {
		if colorRE.MatchString(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestTagList(t *testing.T) {
	vms := []vmSummary{{Tags: []string{"web", "Prod"}}, {Tags: []string{"prod", "web"}}, {Tags: nil}}
	got := tagList(vms, map[string]string{"web": "#112233", "gone": "#000000"})
	want := []tagInfo{{"Prod", "", 1}, {"prod", "", 1}, {"web", "#112233", 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}
