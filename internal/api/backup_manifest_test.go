package api

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func goodManifest() backupManifest {
	return backupManifest{
		Format: 1, VM: "web-1", UUID: "0dc0f2ac-2086-4f51-9633-9d6952479016", Created: "2026-11-10T19:30:00Z",
		SourceNode: "n1", AppVersion: "x", Notes: []string{noteSnapshots, noteTPM},
		Disks: []backupDiskRec{{Target: "vda", File: "vda.qcow2", Format: "qcow2", VirtualSize: 1 << 30, Bytes: 1000}},
	}
}

func TestParseManifest(t *testing.T) {
	ok := goodManifest()
	raw, _ := json.Marshal(ok)
	if _, err := parseBackupManifest(raw); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(m *backupManifest){
		"format":     func(m *backupManifest) { m.Format = 2 },
		"vm name":    func(m *backupManifest) { m.VM = "../x" },
		"uuid":       func(m *backupManifest) { m.UUID = "nope" },
		"time":       func(m *backupManifest) { m.Created = "yesterday" },
		"no disks":   func(m *backupManifest) { m.Disks = nil },
		"file slash": func(m *backupManifest) { m.Disks[0].File = "../vda.qcow2" },
		"reserved":   func(m *backupManifest) { m.Disks[0].File = "domain.xml" },
		"format raw": func(m *backupManifest) { m.Disks[0].Format = "raw" },
		"target":     func(m *backupManifest) { m.Disks[0].Target = "v;da" },
		"dup file": func(m *backupManifest) {
			m.Disks = append(m.Disks, backupDiskRec{Target: "vdb", File: "vda.qcow2", Format: "qcow2"})
		},
		"bad sha":    func(m *backupManifest) { m.Disks[0].SHA256 = "abc" },
		"nvram name": func(m *backupManifest) { m.NVRAM = "/etc/passwd" },
		"nvram fmt":  func(m *backupManifest) { m.NVRAM, m.NVRAMFormat = "nvram.fd", "vmdk" },
	}
	for name, mut := range cases {
		m := goodManifest()
		mut(&m)
		raw, _ := json.Marshal(m)
		if _, err := parseBackupManifest(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseBackupManifest(bytes.Repeat([]byte(" "), backupMaxManifest+1)); err == nil {
		t.Error("oversized manifest accepted")
	}
}

func TestBackupDirName(t *testing.T) {
	n := backupDirName("web-1", time.Date(2026, 11, 10, 21, 30, 5, 0, time.FixedZone("x", 7200)))
	if n != "web-1-20261110T193005Z.vmbackup" || !backupDirRE.MatchString(n) {
		t.Fatal(n)
	}
}

func TestStripBackingStore(t *testing.T) {
	in := `<disk type='file'>
      <driver name='qemu' type='qcow2'/>
      <source file='/a/top.qcow2'/>
      <backingStore type='file'>
        <format type='qcow2'/>
        <source file='/a/mid.qcow2'/>
        <backingStore type='file'>
          <format type='qcow2'/>
          <source file='/a/base.qcow2'/>
          <backingStore/>
        </backingStore>
      </backingStore>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk><backingStore/><target dev='vdb'/></disk>`
	got := stripBackingStore(in)
	if strings.Contains(got, "backingStore") || strings.Contains(got, "mid.qcow2") {
		t.Fatalf("not stripped:\n%s", got)
	}
	for _, keep := range []string{"/a/top.qcow2", "<target dev='vda' bus='virtio'/>", "<target dev='vdb'/>", "</disk>"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("lost %q:\n%s", keep, got)
		}
	}
	if stripBackingStore("<a/>") != "<a/>" {
		t.Fatal("changed a definition without backing stores")
	}
}

func TestBackupXML(t *testing.T) {
	d := []migDisk{{Target: "vda"}, {Target: "vdb"}}
	x := backupXML("/p/.w", d, map[string]string{"vda": "vda.qcow2", "vdb": "vdb.qcow2"}, []string{"sda"}, "")
	if strings.Contains(x, "incremental") {
		t.Errorf("a full backup names no checkpoint: %s", x)
	}
	for _, want := range []string{"mode='push'", "<disk name='vda' backup='yes' type='file'><target file='/p/.w/vda.qcow2'/><driver type='qcow2'/></disk>", "<disk name='sda' backup='no'/>"} {
		if !strings.Contains(x, want) {
			t.Errorf("missing %q in %s", want, x)
		}
	}
}

func TestParseScan(t *testing.T) {
	m := "@@@abc"
	out := m + " /pool/a-1.vmbackup\n4096\n{\"x\":1}\n\n" +
		m + " /pool/evil/../b.vmbackup\n1\n{}\n" +
		m + " /other/c.vmbackup\n1\n{}\n" +
		m + " /pool/notabackup\n1\n{}\n" +
		m + " /pool/d.vmbackup\n77\n{\"y\":2}\n"
	got := parseScan(out, m, "/pool/")
	if len(got) != 2 || got[0].Dir != "/pool/a-1.vmbackup" || got[0].Size != 4096 || string(got[0].Manifest) != `{"x":1}` || got[1].Size != 77 {
		t.Fatalf("%+v", got)
	}
	if len(parseScan("", m, "/pool")) != 0 {
		t.Fatal("empty output gave entries")
	}
}

func TestPruneFullBackups(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var all []retained
	for i := 0; i < 5; i++ {
		all = append(all, retained{Key: "a" + string(rune('0'+i)), UUID: "u1", Created: base.Add(time.Duration(i) * time.Hour)})
	}
	all = append(all, retained{Key: "other", UUID: "u2", Created: base.Add(-time.Hour)})
	p := pruneChains(all, "u1", 2, "")
	if len(p) != 3 || p[0] != "a2" || p[2] != "a0" {
		t.Fatalf("%+v", p)
	}
	if len(pruneChains(all, "u1", 0, "")) != 0 || len(pruneChains(all, "u1", 9, "")) != 0 || len(pruneChains(all, "u2", 1, "")) != 0 {
		t.Fatal("pruned too much")
	}
}

func mkTar(t *testing.T, hs ...tar.Header) []byte {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range hs {
		body := strings.Repeat("x", int(h.Size))
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	w.Close()
	return b.Bytes()
}

func TestVetTar(t *testing.T) {
	reg := func(n string, size int64) tar.Header {
		return tar.Header{Name: n, Size: size, Typeflag: tar.TypeReg, Mode: 0o644}
	}
	var out bytes.Buffer
	names, err := vetTar(bytes.NewReader(mkTar(t, reg("manifest.json", 10), reg("vda.qcow2", 100))), &out, 1<<20)
	if err != nil || len(names) != 2 {
		t.Fatalf("%v %v", names, err)
	}
	// The rewritten archive must itself be readable.
	if _, err := vetTar(&out, &bytes.Buffer{}, 1<<20); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]byte{
		"dotdot":    mkTar(t, reg("../x", 1)),
		"abs":       mkTar(t, reg("/etc/passwd", 1)),
		"subdir":    mkTar(t, reg("a/b", 1)),
		"dotslash":  mkTar(t, reg("./manifest.json", 1)),
		"symlink":   mkTar(t, tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
		"hardlink":  mkTar(t, tar.Header{Name: "x", Typeflag: tar.TypeLink, Linkname: "y"}),
		"dir":       mkTar(t, tar.Header{Name: "d", Typeflag: tar.TypeDir}),
		"dup":       mkTar(t, reg("a", 1), reg("a", 1)),
		"too big":   mkTar(t, reg("a", 600), reg("b", 600)),
		"empty":     mkTar(t),
		"not a tar": []byte("hello, this is not a tar archive at all......................................................................................................................................................................................................................................................................................................................................................................................................................"),
	}
	for name, data := range bad {
		if _, err := vetTar(bytes.NewReader(data), &bytes.Buffer{}, 1000); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRemapDisks(t *testing.T) {
	in := "<devices>\n    <disk type='file' device='disk'>\n      <source file='/old/a.qcow2'/>\n      <target dev='vda'/>\n    </disk>\n    <disk type='file' device='cdrom'>\n      <source file='/iso/x.iso'/>\n      <target dev='sda'/>\n    </disk>\n  </devices>"
	got := remapDisks(in, map[string]string{"/old/a.qcow2": "/new/b.qcow2", "/iso/x.iso": ""})
	if !strings.Contains(got, "<source file='/new/b.qcow2'/>") || strings.Contains(got, "x.iso") || !strings.Contains(got, "<target dev='sda'/>") {
		t.Fatal(got)
	}
}

// A manifest written by the version before the rename (appVersion
// "virt-proj dev") is read like any other: the product name is informational.
func TestParseManifestFromBeforeRename(t *testing.T) {
	raw := `{"format":1,"vm":"web-1","uuid":"0dc0f2ac-2086-4f51-9633-9d6952479016","created":"2026-11-10T19:30:00Z",` +
		`"sourceNode":"n1","appVersion":"virt-proj dev","live":false,"disks":[{"target":"vda","file":"vda.qcow2","format":"qcow2","virtualSize":1073741824,"bytes":1000}],"tpm":false,"notes":[]}`
	m, err := parseBackupManifest([]byte(raw))
	if err != nil || m.AppVersion != "virt-proj dev" || m.VM != "web-1" {
		t.Fatalf("%+v %v", m, err)
	}
	if backupAppVersion != "virticle dev" {
		t.Fatalf("new manifests carry %q", backupAppVersion)
	}
}

// A manifest from before qcow2 variable stores has no nvramFormat; a new one records it.
func TestManifestNVRAMFormat(t *testing.T) {
	old := `{"format":1,"vm":"a","uuid":"0dc0f2ac-2086-4f51-9633-9d6952479016","created":"2026-11-10T19:30:00Z","sourceNode":"n","appVersion":"x","live":false,` +
		`"disks":[{"target":"vda","file":"vda.qcow2","format":"qcow2","virtualSize":1,"bytes":1}],"nvram":"nvram.fd","tpm":false,"notes":[]}`
	m, err := parseBackupManifest([]byte(old))
	if err != nil || m.NVRAMFormat != "" {
		t.Fatalf("old manifest: %+v %v", m, err)
	}
	g := goodManifest()
	g.NVRAM, g.NVRAMFormat = "nvram.fd", "qcow2"
	raw, _ := json.Marshal(g)
	if m, err := parseBackupManifest(raw); err != nil || m.NVRAMFormat != "qcow2" {
		t.Fatalf("new manifest: %+v %v", m, err)
	}
}
