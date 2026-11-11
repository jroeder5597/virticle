package api

import (
	"reflect"
	"strings"
	"testing"

	"virt-proj/internal/cloudinit"
	"virt-proj/internal/store"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKtnt72pPWXOyE7vKx9bVhHeiDsdpEoi3OD1lfAd3vmi t@t"

func TestMergeProfile(t *testing.T) {
	hash, _ := cloudinit.HashPassword("secret")
	p := &store.CloudProfile{User: "ops", PwHash: hash, SSHKeys: []string{"ssh-ed25519 AAA p"}, Packages: []string{"qemu-guest-agent", "curl"}}

	// Nothing set: everything is inherited, the hostname stays per VM.
	c := mergeProfile(p, ciInput{Hostname: "vm1"})
	if c.Hostname != "vm1" || c.User != "ops" || c.PasswordHash != hash || c.Password != "" {
		t.Errorf("inherit: %+v", c)
	}
	// Dialog values override scalars and extend lists, without duplicates.
	c = mergeProfile(p, ciInput{User: "bob", Password: "new", SSHKeys: []string{" ssh-ed25519 BBB d ", "ssh-ed25519 AAA p"}, Packages: []string{"git", "curl"}})
	if c.User != "bob" || c.Password != "new" || c.PasswordHash != "" {
		t.Errorf("override: %+v", c)
	}
	if !reflect.DeepEqual(c.SSHKeys, []string{"ssh-ed25519 AAA p", "ssh-ed25519 BBB d"}) {
		t.Errorf("keys: %v", c.SSHKeys)
	}
	if !reflect.DeepEqual(c.Packages, []string{"qemu-guest-agent", "curl", "git"}) {
		t.Errorf("packages: %v", c.Packages)
	}
	// Raw user-data: the request's wins, whitespace counts as unset.
	p.RawUserData = "#cloud-config\nfoo: 1\n"
	if c = mergeProfile(p, ciInput{RawUserData: "  \n"}); c.RawUserData != p.RawUserData {
		t.Errorf("raw inherit: %q", c.RawUserData)
	}
	if c = mergeProfile(p, ciInput{RawUserData: "#!/bin/sh\n"}); c.RawUserData != "#!/bin/sh\n" {
		t.Errorf("raw override: %q", c.RawUserData)
	}
	// No profile: the input passes through, and the profile is never modified.
	if c = mergeProfile(nil, ciInput{User: "x", Packages: []string{"a"}}); c.User != "x" || len(c.Packages) != 1 {
		t.Errorf("nil: %+v", c)
	}
	if len(p.SSHKeys) != 1 || len(p.Packages) != 2 {
		t.Errorf("profile mutated: %+v", p)
	}
}

func TestMergedSeedCarriesHashNotPlaintext(t *testing.T) {
	hash, _ := cloudinit.HashPassword("secret")
	c := mergeProfile(&store.CloudProfile{User: "ops", PwHash: hash}, ciInput{Hostname: "h"})
	ud, err := c.UserData()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, "passwd: \""+hash+"\"") || !strings.Contains(ud, "lock_passwd: false") || !strings.Contains(ud, "ssh_pwauth: true") {
		t.Errorf("user-data:\n%s", ud)
	}
	if strings.Contains(ud, "secret") {
		t.Error("plaintext in user-data")
	}
}

func TestValidateProfile(t *testing.T) {
	ok := profileRequest{Name: " dev ", User: "ops", SSHKeys: []string{testKey, testKey}, Packages: []string{"curl"}}
	p, err := validateProfile(ok)
	if err != nil || p.Name != "dev" || len(p.SSHKeys) != 1 {
		t.Fatalf("valid: %+v %v", p, err)
	}
	many := make([]string, maxProfileKeys+1)
	for i := range many {
		many[i] = testKey
	}
	bad := map[string]profileRequest{
		"name":      {Name: " "},
		"long name": {Name: strings.Repeat("x", 65)},
		"user":      {Name: "a", User: "Bad User"},
		"key":       {Name: "a", User: "u", SSHKeys: []string{"ssh-ed25519 notbase64!"}},
		"multikey":  {Name: "a", User: "u", SSHKeys: []string{testKey + "\n" + testKey}},
		"package":   {Name: "a", Packages: []string{"x;rm"}},
		"raw":       {Name: "a", RawUserData: "hello: world"},
		"raw size":  {Name: "a", RawUserData: "#cloud-config\n" + strings.Repeat("a", maxProfileRaw)},
	}
	for name, q := range bad {
		if _, err := validateProfile(q); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Distinct keys beyond the limit: duplicates collapse, so make them differ.
	for i := range many {
		many[i] = testKey + " " + strings.Repeat("c", i+1)
	}
	if _, err := validateProfile(profileRequest{Name: "a", User: "u", SSHKeys: many}); err == nil {
		t.Error("too many keys accepted")
	}
}

func TestProfileViewHidesHash(t *testing.T) {
	v := view(store.CloudProfile{ID: 1, Name: "x", PwHash: "$6$salt$abc"})
	if !v.HasPassword {
		t.Error("hasPassword")
	}
}
