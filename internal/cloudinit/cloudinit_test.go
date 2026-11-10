package cloudinit

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestSHA512CryptVectors(t *testing.T) {
	cases := []struct {
		pw, salt string
		rounds   int
		want     string
	}{
		{"Hello world!", "saltstring", 5000, "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"Hello world!", "saltstringsaltstring", 10000, "$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"This is just a test", "toolongsaltstring", 5000, "$6$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
	}
	for _, c := range cases {
		if got := sha512Crypt(c.pw, c.salt, c.rounds); got != c.want {
			t.Errorf("sha512Crypt(%q,%q,%d)\n got %s\nwant %s", c.pw, c.salt, c.rounds, got, c.want)
		}
	}
	h, err := HashPassword("secret")
	if err != nil || !strings.HasPrefix(h, "$6$") || strings.Contains(h, "secret") {
		t.Fatalf("bad hash %q %v", h, err)
	}
	salt := strings.Split(h, "$")[2]
	if sha512Crypt("secret", salt, 5000) != h {
		t.Error("hash does not verify")
	}
}

// readDir lists the root directory of the tree whose root record starts at
// off in the descriptor, returning name -> contents. rr selects Rock Ridge
// names (primary tree) instead of UCS-2 (Joliet).
func readDir(t *testing.T, img []byte, desc int, joliet bool) map[string]string {
	t.Helper()
	d := img[desc*sector : (desc+1)*sector]
	if string(d[1:6]) != "CD001" {
		t.Fatal("bad descriptor")
	}
	root := d[156:190]
	lba := int(binary.LittleEndian.Uint32(root[2:]))
	dir := img[lba*sector : (lba+1)*sector]
	out := map[string]string{}
	for off := 0; dir[off] != 0; {
		n := int(dir[off])
		rec := dir[off : off+n]
		off += n
		idLen := int(rec[32])
		id := rec[33 : 33+idLen]
		if idLen == 1 && id[0] <= 1 {
			continue
		}
		name := ""
		if joliet {
			u := make([]uint16, idLen/2)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(id[2*i:])
			}
			name = string(utf16.Decode(u))
		} else {
			su := rec[33+idLen+(idLen+1)%2:]
			for len(su) >= 4 {
				if su[0] == 'N' && su[1] == 'M' {
					name = string(su[5:su[2]])
				}
				su = su[su[2]:]
			}
		}
		fl := int(binary.LittleEndian.Uint32(rec[2:]))
		size := int(binary.LittleEndian.Uint32(rec[10:]))
		out[name] = string(img[fl*sector : fl*sector+size])
	}
	return out
}

func TestBuildImage(t *testing.T) {
	files := map[string][]byte{
		"user-data":        []byte("#cloud-config\n"),
		"meta-data":        []byte("instance-id: x\n"),
		"big":              bytes.Repeat([]byte("z"), 5000),
		"autounattend.xml": []byte("<x/>"),
	}
	img, err := BuildImage("cidata", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(img)%sector != 0 {
		t.Fatal("image not sector aligned")
	}
	if string(img[16*sector+1:16*sector+6]) != "CD001" || !bytes.Equal(img[16*sector+40:16*sector+46], []byte("CIDATA")) {
		t.Error("primary volume descriptor / label wrong")
	}
	jl := img[17*sector+40 : 17*sector+52]
	if !bytes.Equal(jl, []byte{0, 'c', 0, 'i', 0, 'd', 0, 'a', 0, 't', 0, 'a'}) {
		t.Errorf("joliet label wrong: %v", jl)
	}
	for i, joliet := range []bool{false, true} {
		got := readDir(t, img, 16+i, joliet)
		if len(got) != len(files) {
			t.Fatalf("joliet=%v: got %d entries %v", joliet, len(got), got)
		}
		for n, want := range files {
			if got[n] != string(want) {
				t.Errorf("joliet=%v: file %s mismatch", joliet, n)
			}
		}
	}
	if _, err := BuildImage("x", map[string][]byte{"a/b": nil}); err == nil {
		t.Error("slash in name accepted")
	}
}

func TestSeed(t *testing.T) {
	c := Config{
		Hostname: "web1", User: "ops", Password: "pa\"ss: #word\n",
		SSHKeys:  []string{"ssh-ed25519 AAAAC3Nz test@host"},
		Packages: []string{"qemu-guest-agent", "htop"},
	}
	ud, err := c.UserData()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config\n", `hostname: "web1"`, `- name: "ops"`, `passwd: "$6$`, `- "ssh-ed25519 AAAAC3Nz test@host"`, `- "qemu-guest-agent"`, "ssh_pwauth: true", "systemctl, start, qemu-guest-agent"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user-data lacks %q:\n%s", want, ud)
		}
	}
	if strings.Contains(ud, "pa\\\"ss") || strings.Contains(ud, "#word") {
		t.Errorf("clear text password leaked:\n%s", ud)
	}
	img, err := c.Seed()
	if err != nil {
		t.Fatal(err)
	}
	got := readDir(t, img, 16, false)
	if !strings.HasPrefix(got["user-data"], "#cloud-config\nhostname: \"web1\"") || !strings.Contains(got["meta-data"], "instance-id: \"iid-") || !strings.Contains(got["meta-data"], `local-hostname: "web1"`) {
		t.Errorf("seed contents wrong: %v", got)
	}
	m2, _ := c.MetaData()
	m1, _ := c.MetaData()
	if m1 == m2 {
		t.Error("instance ids are not fresh")
	}

	raw := Config{RawUserData: "#!/bin/sh\necho hi\n"}
	if ud, _ := raw.UserData(); ud != raw.RawUserData {
		t.Error("raw user-data not used verbatim")
	}
	for _, bad := range []Config{{Hostname: "a b"}, {User: "Root!"}, {SSHKeys: []string{"ssh-rsa AAA"}}, {User: "u", SSHKeys: []string{"junk"}}, {Packages: []string{"a;b"}}, {Password: "x"}} {
		if _, err := bad.UserData(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
