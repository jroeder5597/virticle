package cluster

import "testing"

func TestUUIDRoundTrip(t *testing.T) {
	const s = "0f3a9c1e-12ab-4cde-8f01-23456789abcd"
	u, err := ParseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatUUID(u); got != s {
		t.Fatalf("got %s, want %s", got, s)
	}
	if _, err := ParseUUID("nope"); err == nil {
		t.Fatal("expected error for malformed UUID")
	}
}
