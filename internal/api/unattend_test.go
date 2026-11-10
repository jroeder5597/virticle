package api

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

func TestUnattendXML(t *testing.T) {
	c := unattendConfig{ComputerName: "WIN-01", User: "adm in", Password: `p<a>&"s'`}
	x, err := c.unattendXML()
	if err != nil {
		t.Fatal(err)
	}
	// Well-formed, and the password round-trips through the escaping.
	var doc struct {
		Settings []struct {
			Pass     string   `xml:"pass,attr"`
			Name     string   `xml:"component>ComputerName"`
			Password []string `xml:"component>UserAccounts>LocalAccounts>LocalAccount>Password>Value"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal([]byte(x), &doc); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, x)
	}
	if len(doc.Settings) != 2 || doc.Settings[1].Pass != "oobeSystem" || doc.Settings[0].Name != "WIN-01" ||
		len(doc.Settings[1].Password) != 1 || doc.Settings[1].Password[0] != `p<a>&"s'` {
		t.Errorf("unexpected content: %+v\n%s", doc, x)
	}
	if strings.Contains(x, "p<a>") {
		t.Error("password not escaped")
	}
	img, err := c.image()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(img, []byte("UNATTEND")) || !bytes.Contains(img, []byte("<unattend")) {
		t.Error("image lacks label or answer file")
	}
}

func TestUnattendValidate(t *testing.T) {
	bad := []unattendConfig{
		{"", "a", "p"}, {"TOOLONGCOMPUTERNAME1", "a", "p"}, {"123", "a", "p"}, {"a_b", "a", "p"},
		{"ok", "", "p"}, {"ok", "a/b", "p"}, {"ok", "a", ""}, {"ok", "<x>", "p"},
	}
	for _, c := range bad {
		if _, err := c.unattendXML(); err == nil {
			t.Errorf("%+v should be rejected", c)
		}
	}
}
