package api

import (
	"encoding/json"
	"testing"
)

func TestIsVirtioWinISO(t *testing.T) {
	const pat = "virtio-win*.iso"
	for name, want := range map[string]bool{
		"virtio-win.iso":             true,
		"virtio-win-0.1.266.iso":     true,
		"VirtIO-Win-0.1.240.ISO":     true,
		"virtio-win.qcow2":           false,
		"my-virtio-win.iso":          false,
		"virtio-win-guest-tools.exe": false,
		"win10.iso":                  false,
		"":                           false,
	} {
		if got := isVirtioWinISO(name, pat); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
	if !isVirtioWinISO("wg-virtio-win.iso", "wg-virtio-win*.iso") {
		t.Error("custom pattern")
	}
	if isVirtioWinISO("x", "[") {
		t.Error("a malformed pattern must not match")
	}
}

func TestGuessOS(t *testing.T) {
	const ns = `<metadata><libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"><libosinfo:os id="%s"/></libosinfo:libosinfo></metadata>`
	win := `<domain>` + `<metadata><libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"><libosinfo:os id="http://microsoft.com/win/11"/></libosinfo:libosinfo></metadata></domain>`
	if f, id, src := guessOS(win); f != "windows" || id != "http://microsoft.com/win/11" || src != "libosinfo" {
		t.Errorf("win11: %q %q %q", f, id, src)
	}
	deb := `<domain><metadata><libosinfo:libosinfo xmlns:libosinfo="http://libosinfo.org/xmlns/libvirt/domain/1.0"><libosinfo:os id="http://debian.org/debian/12"/></libosinfo:libosinfo></metadata></domain>`
	if f, _, src := guessOS(deb); f != "linux" || src != "libosinfo" {
		t.Errorf("debian: %q %q", f, src)
	}
	if f, _, src := guessOS(`<domain><features><acpi/><hyperv><relaxed state='on'/></hyperv></features></domain>`); f != "windows" || src != "guess" {
		t.Errorf("hyperv: %q %q", f, src)
	}
	if f, _, _ := guessOS(`<domain><clock offset='localtime'/></domain>`); f != "windows" {
		t.Errorf("localtime: %q", f)
	}
	if f, _, _ := guessOS(`<domain><clock offset='utc'/></domain>`); f != "" {
		t.Errorf("plain domain should be undetermined, got %q", f)
	}
	_ = ns
}

func TestParseFSInfo(t *testing.T) {
	raw := json.RawMessage(`[
	 {"name":"vda1","mountpoint":"/","type":"ext4","used-bytes":1000,"total-bytes":4000,"disk":[]},
	 {"name":"vda15","mountpoint":"/boot/efi","type":"vfat","used-bytes":10,"total-bytes":100,"disk":[]},
	 {"name":"vda1","mountpoint":"/","type":"ext4","used-bytes":1000,"total-bytes":4000},
	 {"name":"sr0","mountpoint":"/mnt/cd","type":"iso9660"},
	 {"name":"vdb","mountpoint":"/data","type":"xfs","used-bytes":0,"total-bytes":0}]`)
	fs, err := parseFSInfo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[0].Mountpoint != "/" || fs[1].Mountpoint != "/boot/efi" || fs[0].UsedBytes != 1000 || fs[0].TotalBytes != 4000 {
		t.Errorf("got %+v", fs)
	}
	if _, err := parseFSInfo(json.RawMessage(`{}`)); err == nil {
		t.Error("an object is not a filesystem list")
	}
}

func TestParseOSInfo(t *testing.T) {
	o, err := parseOSInfo(json.RawMessage(`{"id":"debian","name":"Debian GNU/Linux","pretty-name":"Debian GNU/Linux 13 (trixie)","version":"13 (trixie)","version-id":"13","kernel-release":"6.12.48","kernel-version":"#1 SMP","machine":"x86_64"}`))
	if err != nil || o.PrettyName != "Debian GNU/Linux 13 (trixie)" || o.Kernel != "6.12.48" || o.Version != "13 (trixie)" {
		t.Errorf("got %+v, %v", o, err)
	}
	o, _ = parseOSInfo(json.RawMessage(`{"id":"mswindows","version-id":"10.0"}`))
	if o.Version != "10.0" {
		t.Errorf("version-id fallback: %+v", o)
	}
}

func TestParseAgentInterfaces(t *testing.T) {
	m, err := parseAgentInterfaces(json.RawMessage(`[
	 {"name":"lo","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4"}]},
	 {"name":"eth0","hardware-address":"52:54:00:AA:BB:CC","ip-addresses":[{"ip-address":"192.168.1.5"},{"ip-address":"fe80::1"}]}]`))
	if err != nil || len(m) != 1 || len(m["52:54:00:aa:bb:cc"]) != 1 || m["52:54:00:aa:bb:cc"][0] != "192.168.1.5" {
		t.Errorf("got %v, %v", m, err)
	}
}
