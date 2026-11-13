package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 seed "12345678901234567890"; the RFC lists 8
// digits, a 6-digit code is the last six of those.
func TestRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for _, c := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"}} {
		got, err := Code(secret, Step(time.Unix(c.unix, 0)))
		if err != nil || got != c.want {
			t.Errorf("t=%d: got %s want %s (%v)", c.unix, got, c.want, err)
		}
	}
}

func TestDriftAndReplay(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	cur := Step(now)
	at := func(s int64) string { c, _ := Code(secret, s); return c }
	for _, d := range []int64{-1, 0, 1} {
		if s, ok := Verify(secret, at(cur+d), now, 0); !ok || s != cur+d {
			t.Errorf("step offset %d must be accepted", d)
		}
	}
	for _, d := range []int64{-2, 2, 5} {
		if _, ok := Verify(secret, at(cur+d), now, 0); ok {
			t.Errorf("step offset %d must be refused", d)
		}
	}
	// Replay: once step cur was used, neither it nor any earlier step works.
	if _, ok := Verify(secret, at(cur), now, cur); ok {
		t.Error("the same code twice must be refused")
	}
	if _, ok := Verify(secret, at(cur-1), now, cur); ok {
		t.Error("an older code after a newer one must be refused")
	}
	if _, ok := Verify(secret, at(cur+1), now, cur); !ok {
		t.Error("the next code must still work")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	spaced := at(cur)[:3] + " " + at(cur)[3:]
	if _, ok := Verify(secret, spaced, now, 0); !ok {
		t.Error("a code typed with a space must work")
	}
}

func TestURI(t *testing.T) {
	u := URI("Virticle", "alice@example.org", "ABCDEFGH")
	for _, w := range []string{"otpauth://totp/Virticle:alice@example.org?", "secret=ABCDEFGH", "issuer=Virticle", "digits=6", "period=30"} {
		if !strings.Contains(u, w) {
			t.Errorf("%s lacks %s", u, w)
		}
	}
}
