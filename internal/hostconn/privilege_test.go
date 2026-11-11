package hostconn

import (
	"errors"
	"os/exec"
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

func TestWrapRoot(t *testing.T) {
	if got, err := WrapRoot(PrivRoot, "id -u"); err != nil || got != "id -u" {
		t.Errorf("root: %q %v", got, err)
	}
	if got, err := WrapRoot("", "id -u"); err != nil || got != "id -u" {
		t.Errorf("undetected: %q %v", got, err)
	}
	if _, err := WrapRoot(PrivNone, "id -u"); !errors.Is(err, ErrNoPrivilege) {
		t.Errorf("none: %v", err)
	}
	got, err := WrapRoot(PrivSudo, "echo 'x y' | tr a-z A-Z")
	if err != nil || got != `sudo -n sh -c 'echo '\''x y'\'' | tr a-z A-Z'` {
		t.Fatalf("sudo: %q %v", got, err)
	}
	if !strings.HasPrefix(got, "sudo -n sh -c ") {
		t.Fatal("prefix")
	}
}

// The wrapped line, with sudo replaced by a function that just runs its
// arguments, must behave like the plain command line: output and exit code.
func TestWrapRootRoundTrip(t *testing.T) {
	cmds := []string{
		`echo 'single quoted' "double $((1+2))" | tr a-z A-Z`,
		`FOO=bar sh -c 'echo $FOO'; echo "rc=$?"`,
		`printf '%s\n' "it's" 'a"b' | wc -l; exit 3`,
		"cat <<EOF\nhere doc $((2*3))\nEOF",
		`x=$(echo hi); [ "$x" = hi ] && echo yes || echo no; false`,
		`cat; echo done`,
	}
	for _, c := range cmds {
		w, err := WrapRoot(PrivSudo, c)
		if err != nil {
			t.Fatal(err)
		}
		fake := `sudo() { shift; "$@"; }; ` + w
		pl := exec.Command("sh", "-c", c)
		pl.Stdin = strings.NewReader("stdin text\n")
		want, werr := pl.CombinedOutput()
		wr := exec.Command("sh", "-c", fake)
		wr.Stdin = strings.NewReader("stdin text\n")
		got, gerr := wr.CombinedOutput()
		if string(got) != string(want) {
			t.Errorf("%q: wrapped output %q, plain %q", c, got, want)
		}
		code := func(e error) int {
			var x *exec.ExitError
			if errors.As(e, &x) {
				return x.ExitCode()
			}
			return 0
		}
		if code(werr) != code(gerr) {
			t.Errorf("%q: exit codes differ: %v vs %v", c, werr, gerr)
		}
	}
}

func TestDropHost(t *testing.T) {
	in := "10.0.0.1 ssh-ed25519 AAAA\n10.0.0.2,box ssh-ed25519 BBBB\n|1|salt|hash ssh-ed25519 CCCC\n# c\n[10.0.0.1]:2222 ssh-ed25519 DDDD\n"
	out, n := dropHost(in, "10.0.0.1")
	if n != 1 || strings.Contains(out, "AAAA") || !strings.Contains(out, "BBBB") || !strings.Contains(out, "DDDD") {
		t.Errorf("n=%d out=%q", n, out)
	}
	out, n = dropHost(in, "10.0.0.2")
	if n != 1 || !strings.Contains(out, "box ssh-ed25519 BBBB") || strings.Contains(out, "10.0.0.2") {
		t.Errorf("n=%d out=%q", n, out)
	}
}

func TestParseDetect(t *testing.T) {
	cases := []struct {
		in   string
		want Privilege
	}{
		{"uid=0\nsudo=no\nuser=root", PrivRoot},
		{"uid=0\nsudo=yes\nuser=root", PrivRoot},
		{"uid=1000\nsudo=yes\nuser=ne-user", PrivSudo},
		{"uid=1000\nsudo=no\nuser=ne-user", PrivNone},
		{"", PrivNone},
	}
	for _, c := range cases {
		if got, _ := ParseDetect(c.in); got != c.want {
			t.Errorf("%q: %s, want %s", c.in, got, c.want)
		}
	}
}
