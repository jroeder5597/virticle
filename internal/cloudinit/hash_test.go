package cloudinit

import (
	"strings"
	"testing"
)

func TestPasswordHashIsWrittenVerbatim(t *testing.T) {
	hash, _ := HashPassword("pw")
	if !HashRE.MatchString(hash) {
		t.Fatalf("HashPassword output %q does not match HashRE", hash)
	}
	ud, err := Config{User: "u", PasswordHash: hash}.UserData()
	if err != nil || !strings.Contains(ud, "passwd: \""+hash+"\"") || !strings.Contains(ud, "ssh_pwauth: true") {
		t.Errorf("%v\n%s", err, ud)
	}
	// A plaintext password wins over a hash.
	ud, _ = Config{User: "u", Password: "pw", PasswordHash: hash}.UserData()
	if strings.Contains(ud, hash) {
		t.Error("hash used although a plaintext password was given")
	}
	for _, bad := range []string{"plaintext", "$1$abc$def", "$6$salt$short"} {
		if _, err := (Config{User: "u", PasswordHash: bad}).UserData(); err == nil {
			t.Errorf("hash %q accepted", bad)
		}
	}
	if _, err := (Config{PasswordHash: hash}).UserData(); err == nil {
		t.Error("hash without user accepted")
	}
}
