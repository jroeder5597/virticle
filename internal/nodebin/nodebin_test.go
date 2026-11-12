package nodebin

import "testing"

func TestGoArch(t *testing.T) {
	for in, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64", "amd64": "amd64"} {
		if got, ok := GoArch(in); !ok || got != want {
			t.Errorf("%s: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "ppc64le", "i686", "armv7l", "x86_64; id"} {
		if _, ok := GoArch(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
	if _, err := Binary("ppc64le"); err == nil {
		t.Error("an unsupported architecture gave a binary")
	}
}

// With "make nodebin" done the binaries are static ELF files; without it the
// error says what to do.
func TestBinaryIsELFOrExplainsItself(t *testing.T) {
	for _, u := range []string{"x86_64", "aarch64"} {
		b, err := Binary(u)
		if err != nil {
			t.Logf("%s: %v (run make nodebin to test the embedded binary)", u, err)
			continue
		}
		if len(b) < 1<<20 || string(b[:4]) != "\x7fELF" {
			t.Errorf("%s: %d bytes, not an ELF file", u, len(b))
		}
	}
}
