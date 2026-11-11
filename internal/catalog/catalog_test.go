package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	l := Builtin()
	if len(l) != 10 {
		t.Fatalf("want 10 entries, got %d", len(l))
	}
	ids := map[string]bool{}
	for _, i := range l {
		if err := Validate(i); err != nil {
			t.Errorf("%s: %v", i.ID, err)
		}
		if ids[i.ID] {
			t.Errorf("duplicate id %s", i.ID)
		}
		ids[i.ID] = true
		if !strings.HasPrefix(i.URL, "https://") || !strings.HasPrefix(i.ChecksumURL, "https://") {
			t.Errorf("%s: built-in URLs must be https", i.ID)
		}
		if i.Checksum != "" {
			t.Errorf("%s: built-in entries must not pin a checksum", i.ID)
		}
	}
}

// The captured files in testdata are the real checksum files served by each
// distro (2026-11-11); this is what the parser has to cope with.
func TestParseRealFiles(t *testing.T) {
	want := map[string]string{
		"debian-12":    "0464b5d4ae3aa4f658468d88da83af7857b71548b30d46dcb6282770955a747fb95a983476db90d25afd0653af607224c7ebd05122ab71faa106fd712aaad652",
		"debian-13":    "d25999de0770d4cde1be72c299ba3ad94eac7ea8fd4d18f8cc86c69df69337e2e8a91d64465a9bfd8f2a78c566fcbcb0842eacc91c09efdef72d719d9f13b535",
		"ubuntu-22.04": "f77bc6d59cb8deaa9657095736b5f5003f7705b47055f01823459715c967de6a",
		"ubuntu-24.04": "04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94",
		"rocky-9":      "92c206cc6f790c61583247eefe87890f8828420662c17cacf247cec78ab4eec8",
		"rocky-10":     "9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48",
		"alma-9":       "6f94881458dfe966474544b12e86d078d81a600457188045d317b0ce3c3bdca7",
		"alma-10":      "0c3261d2b2d6ac47b4786bb4f0e96706b885caad760e653aac11276de283f3e3",
		"fedora-45":    "50bdc5e70f6a24f540932d15411c1c6ae9fcfa40fb990d876d83c5cc823b2ad0",
		"cirros-0.6.3": "7d6355852aeb6dbcd191bcda7cd74f1536cfe5cbf8a10495a7283a8396e4b75b",
	}
	files := map[string]string{
		"debian-12": "debian12.SHA512SUMS", "debian-13": "debian13.SHA512SUMS",
		"ubuntu-22.04": "ubuntu2204.SHA256SUMS", "ubuntu-24.04": "ubuntu2404.SHA256SUMS",
		"rocky-9": "rocky9.CHECKSUM", "rocky-10": "rocky10.CHECKSUM",
		"alma-9": "alma9.CHECKSUM", "alma-10": "alma10.CHECKSUM",
		"fedora-45": "fedora45.CHECKSUM", "cirros-0.6.3": "cirros063.SHA256SUMS",
	}
	for _, img := range Builtin() {
		data, err := os.ReadFile(filepath.Join("testdata", files[img.ID]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseChecksum(string(data), img.FileName(), img.Algorithm)
		if err != nil {
			t.Errorf("%s: %v", img.ID, err)
		} else if got != want[img.ID] {
			t.Errorf("%s: got %s", img.ID, got)
		}
	}
}

func TestParseChecksumForms(t *testing.T) {
	h64 := strings.Repeat("ab", 32)
	h128 := strings.Repeat("cd", 64)
	cases := []struct {
		name, text, file, algo, want string
		fail                         bool
	}{
		{"gnu two spaces", h64 + "  a.img\n", "a.img", "sha256", h64, false},
		{"gnu binary star", h64 + " *a.img\n", "a.img", "sha256", h64, false},
		{"gnu dot slash", h64 + "  ./a.img\n", "a.img", "sha256", h64, false},
		{"gnu directory", h64 + "  images/a.img\n", "a.img", "sha256", h64, false},
		{"upper case hash", strings.ToUpper(h64) + "  a.img\n", "a.img", "sha256", h64, false},
		{"crlf", h64 + "  a.img\r\n", "a.img", "sha256", h64, false},
		{"bsd", "SHA256 (a.img) = " + h64 + "\n", "a.img", "sha256", h64, false},
		{"bsd sha512", "SHA512 (a.img) = " + h128 + "\n", "a.img", "sha512", h128, false},
		{"bsd dashed", "SHA-256 (a.img) = " + h64 + "\n", "a.img", "sha256", h64, false},
		{"bsd wrong algorithm ignored", "SHA512 (a.img) = " + h128 + "\n", "a.img", "sha256", "", true},
		{"comments and armour", "-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n# a.img: 1 bytes\nSHA256 (a.img) = " + h64 + "\n-----BEGIN PGP SIGNATURE-----\nabc\n", "a.img", "sha256", h64, false},
		{"other file only", h64 + "  b.img\n", "a.img", "sha256", "", true},
		{"prefix is not a match", h64 + "  a.img.sig\n", "a.img", "sha256", "", true},
		{"suffix is not a match", h64 + "  xa.img\n", "a.img", "sha256", "", true},
		{"wrong length", h128 + "  a.img\n", "a.img", "sha256", "", true},
		{"md5 line ignored", strings.Repeat("a", 32) + "  a.img\n", "a.img", "sha256", "", true},
		{"same twice", h64 + "  a.img\n" + h64 + "  a.img\n", "a.img", "sha256", h64, false},
		{"conflict", h64 + "  a.img\n" + strings.Repeat("ef", 32) + "  a.img\n", "a.img", "sha256", "", true},
		{"empty", "", "a.img", "sha256", "", true},
		{"html error page", "<html><body>404 a.img</body></html>", "a.img", "sha256", "", true},
		{"bad algorithm", h64 + "  a.img\n", "a.img", "md5", "", true},
	}
	for _, c := range cases {
		got, err := ParseChecksum(c.text, c.file, c.algo)
		if (err != nil) != c.fail || got != c.want {
			t.Errorf("%s: got %q, %v", c.name, got, err)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Image{ID: "x-1", Name: "X", Arch: "x86_64", URL: "https://h/x.qcow2", ChecksumURL: "https://h/SUMS", Algorithm: "sha256", VolumeName: "x.qcow2"}
	if err := Validate(good); err != nil {
		t.Fatal(err)
	}
	pinned := good
	pinned.ChecksumURL, pinned.Checksum = "", strings.Repeat("0", 64)
	if err := Validate(pinned); err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*Image){
		"bad id":           func(i *Image) { i.ID = "A b" },
		"no name":          func(i *Image) { i.Name = "" },
		"arch":             func(i *Image) { i.Arch = "arm64" },
		"algorithm":        func(i *Image) { i.Algorithm = "md5" },
		"file url":         func(i *Image) { i.URL = "file:///etc/passwd" },
		"space in url":     func(i *Image) { i.URL = "https://h/a b" },
		"newline in url":   func(i *Image) { i.URL = "https://h/a\nb" },
		"option as url":    func(i *Image) { i.URL = "-o /etc/x" },
		"no checksum":      func(i *Image) { i.ChecksumURL = "" },
		"bad pinned":       func(i *Image) { i.ChecksumURL, i.Checksum = "", "xyz" },
		"pinned too short": func(i *Image) { i.ChecksumURL, i.Checksum = "", strings.Repeat("0", 63) },
		"volume path":      func(i *Image) { i.VolumeName = "../x" },
		"volume space":     func(i *Image) { i.VolumeName = "a b" },
		"user":             func(i *Image) { i.DefaultUser = "root; id" },
		"checksum name":    func(i *Image) { i.ChecksumName = "a\nb" },
	}
	for name, f := range mut {
		i := good
		f(&i)
		if Validate(i) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMergeOverride(t *testing.T) {
	over, err := ParseOverride([]byte(`[
	  {"id":"debian-13","name":"Debian 13 mirror","arch":"x86_64","url":"http://mirror.lan/d13.qcow2","checksumUrl":"http://mirror.lan/SHA512SUMS","algorithm":"sha512","volumeName":"d13.qcow2"},
	  {"id":"mine","name":"Mine","arch":"x86_64","url":"https://h/m.img","checksum":"` + strings.Repeat("a", 64) + `","algorithm":"sha256","volumeName":"m.img"},
	  {"id":"broken","name":"Broken","arch":"x86_64","url":"ftp://h/m.img","algorithm":"sha256","volumeName":"m.img"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	merged, problems := Merge(Builtin(), over)
	if len(merged) != 11 || len(problems) != 1 || !strings.Contains(problems[0], "broken") {
		t.Fatalf("%d entries, problems %v", len(merged), problems)
	}
	for _, i := range merged {
		if i.ID == "debian-13" && i.Name != "Debian 13 mirror" {
			t.Error("override did not replace the built-in entry")
		}
	}
	if _, err := ParseOverride([]byte(`[{"id":"a","nmae":"typo"}]`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ParseOverride([]byte(`{"not":"an array"}`)); err == nil {
		t.Error("object accepted")
	}
}

func TestSourceNeverFails(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	s := &Source{DataDir: dir, Logf: func(f string, a ...any) { logs = append(logs, f) }}
	if len(s.Images()) != 10 || len(logs) != 0 {
		t.Fatal("no override file: built-in list, no log")
	}
	file := filepath.Join(dir, "images.json")
	if err := os.WriteFile(file, []byte("{{{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(s.Images()) != 10 || len(logs) != 1 {
		t.Fatalf("broken file: want built-in list and one log line, got %d", len(logs))
	}
	s.Images()
	if len(logs) != 1 {
		t.Error("the same broken file was logged twice")
	}
	if err := os.WriteFile(file, []byte(`[{"id":"extra","name":"E","arch":"x86_64","url":"https://h/e.img","checksum":"`+strings.Repeat("1", 64)+`","algorithm":"sha256","volumeName":"e.img"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Find("extra"); !ok || len(s.Images()) != 11 {
		t.Error("changed file not picked up")
	}
}
