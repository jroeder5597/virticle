package hostconn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"a":      "'a'",
		"it's":   `'it'\''s'`,
		"":       "''",
		"a b $x": "'a b $x'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestDropHost(t *testing.T) {
	in := "10.0.0.1 ssh-ed25519 AAAA\n10.0.0.2,box ssh-ed25519 BBBB\n|1|salt|hash ssh-ed25519 CCCC\n# c\n[10.0.0.1]:2222 ssh-ed25519 DDDD\n"
	out, n := dropHost(in, "10.0.0.1")
	if n != 1 || strings.Contains(out, "AAAA") || !strings.Contains(out, "BBBB") || !strings.Contains(out, "DDDD") {
		t.Errorf("n=%d out=%q", n, out)
	}
	// a hashed entry for 10.0.0.9
	salt := []byte("0123456789abcdefghij")
	m := hmac.New(sha1.New, salt)
	m.Write([]byte("10.0.0.9"))
	h := "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(m.Sum(nil)) + " ssh-ed25519 EEEE"
	if o, c := dropHost(h+"\n"+in, "10.0.0.9"); c != 1 || strings.Contains(o, "EEEE") || !strings.Contains(o, "BBBB") {
		t.Errorf("hashed: %d %q", c, o)
	}
	out, n = dropHost(in, "10.0.0.2")
	if n != 1 || !strings.Contains(out, "box ssh-ed25519 BBBB") || strings.Contains(out, "10.0.0.2") {
		t.Errorf("n=%d out=%q", n, out)
	}
}

func TestParseDetect(t *testing.T) {
	v := NodeCmdVersion
	cases := []struct {
		in   string
		want Privilege
	}{
		{"uid=0\nnode=" + v + "\nuser=root", PrivRoot},
		{"uid=0\nuser=root", PrivNone},
		{"uid=1000\nnode=" + v + "\nuser=virticle", PrivService},
		{"uid=1000\nuser=ne-user", PrivNone},
		{"", PrivNone},
	}
	for _, c := range cases {
		if got, _ := ParseDetect(c.in); got != c.want {
			t.Errorf("%q: %s, want %s", c.in, got, c.want)
		}
	}
}

func TestParsePrivilege(t *testing.T) {
	for in, want := range map[string]Privilege{
		"root": PrivRoot, "helper": PrivService, "none": PrivNone,
		"": "", "auto": "", "sudo": "", "bogus": "",
	} {
		if got := ParsePrivilege(in); got != want {
			t.Errorf("ParsePrivilege(%q) = %q, want %q", in, got, want)
		}
	}
	// The stored value of the service account did not change.
	if string(PrivService) != "helper" {
		t.Errorf("stored value of the service account: %q", PrivService)
	}
	if Privilege("sudo").Valid() {
		t.Error("the retired general sudo setting is still valid")
	}
}
