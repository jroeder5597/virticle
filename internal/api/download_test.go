package api

import "testing"

func TestShQuote(t *testing.T) {
	if got := shQuote(`a'b; rm -rf /`); got != `'a'\''b; rm -rf /'` {
		t.Error(got)
	}
}

func TestValidDownloadURL(t *testing.T) {
	for _, ok := range []string{"https://example.com/a.qcow2", "http://h:8080/x?y=1"} {
		if validDownloadURL(ok) != nil {
			t.Errorf("rejected %s", ok)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y", "https://", "-o /etc/x", "https://a/b c", "https://a/b\nc", "//a/b"} {
		if validDownloadURL(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, n := range []string{"a/b", "../x", ".hidden", "", "a b", "a;b"} {
		if volNameRE.MatchString(n) {
			t.Errorf("name %q accepted", n)
		}
	}
}
