package tlscert

import "testing"

func TestIsPrivate(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:80": true, "[::1]:80": true, "localhost:80": true, "10.1.2.3:80": true, "192.168.1.5:80": true,
		"172.16.0.1:80": true, "[fd00::1]:80": true, "[fe80::1]:80": true,
		"0.0.0.0:80": false, ":80": false, "[::]:80": false, "8.8.8.8:80": false, "example.org:80": false, "172.32.0.1:80": false, "bad": false,
	} {
		if got := IsPrivate(addr); got != want {
			t.Errorf("%s: got %v want %v", addr, got, want)
		}
	}
}
