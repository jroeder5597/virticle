// Package catalog is the short list of well-known cloud images a node can
// download into a storage pool with one click.
//
// The list is a JSON file embedded in the binary (images.json). An
// administrator may add or replace entries with <data>/images.json, which has
// the same format (a JSON array of objects) and is merged over the built-in
// list by id: an entry with a known id replaces the built-in one, any other id
// is added. Invalid entries are skipped and reported through the log; the
// controller never fails to start because of this file.
//
// Fields: id, name, version, arch (x86_64), url, checksumUrl, algorithm
// (sha256 or sha512), checksumName (optional: the file name to look for in the
// checksum file; default is the last path element of url), checksum (optional:
// a hex digest pinned by the administrator, which makes checksumUrl
// unnecessary), defaultUser, volumeName (suggested volume name), cloudInit,
// sizeBytes (approximate, for display).
//
// Security note, true of every entry: the checksum file is fetched from the
// same server (or at best the same project) as the image. It protects against
// a corrupted or truncated download, NOT against a compromised mirror or
// origin, which could serve a matching pair. Distros that publish a
// GPG-signed checksum file (Debian SHA512SUMS.sign, Ubuntu SHA256SUMS.gpg,
// Fedora's clear-signed CHECKSUM) would allow real authenticity checks; that
// is a possible later improvement and is not implemented. The "checksum" field
// is the way to pin a digest obtained out of band.
package catalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

//go:embed images.json
var builtinJSON []byte

// Image is one catalogue entry.
type Image struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	Arch         string `json:"arch"`
	URL          string `json:"url"`
	ChecksumURL  string `json:"checksumUrl,omitempty"`
	Algorithm    string `json:"algorithm"`
	ChecksumName string `json:"checksumName,omitempty"`
	Checksum     string `json:"checksum,omitempty"`
	DefaultUser  string `json:"defaultUser,omitempty"`
	VolumeName   string `json:"volumeName"`
	CloudInit    bool   `json:"cloudInit"`
	SizeBytes    int64  `json:"sizeBytes,omitempty"`
}

var (
	idRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	volRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,200}$`)
	userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hexRE  = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// HashLen is the length of a hex digest for a supported algorithm, 0 if unsupported.
func HashLen(algorithm string) int {
	switch algorithm {
	case "sha256":
		return 64
	case "sha512":
		return 128
	}
	return 0
}

func checkURL(what, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("%s contains spaces or control characters", what)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http:// or https:// URL", what)
	}
	return nil
}

// Validate checks one entry.
func Validate(i Image) error {
	switch {
	case !idRE.MatchString(i.ID):
		return errors.New("id must be lower-case letters, digits, dot, dash or underscore")
	case i.Name == "" || len(i.Name) > 120:
		return errors.New("name is required (at most 120 characters)")
	case len(i.Version) > 40:
		return errors.New("version is too long")
	case i.Arch != "x86_64":
		return errors.New(`arch must be "x86_64"`)
	case HashLen(i.Algorithm) == 0:
		return errors.New(`algorithm must be "sha256" or "sha512"`)
	case !volRE.MatchString(i.VolumeName):
		return errors.New("volumeName must start with a letter or digit and contain only letters, digits, dot, dash, plus and underscore")
	case i.DefaultUser != "" && !userRE.MatchString(i.DefaultUser):
		return errors.New("defaultUser is not a valid login name")
	case i.SizeBytes < 0:
		return errors.New("sizeBytes must not be negative")
	case strings.ContainsAny(i.ChecksumName, "\r\n\x00"):
		return errors.New("checksumName contains control characters")
	}
	if err := checkURL("url", i.URL); err != nil {
		return err
	}
	if i.Checksum != "" {
		if len(i.Checksum) != HashLen(i.Algorithm) || !hexRE.MatchString(i.Checksum) {
			return fmt.Errorf("checksum must be %d hex digits for %s", HashLen(i.Algorithm), i.Algorithm)
		}
	} else if err := checkURL("checksumUrl", i.ChecksumURL); err != nil {
		return fmt.Errorf("%w (or pin a checksum)", err)
	}
	if i.ChecksumURL != "" {
		if err := checkURL("checksumUrl", i.ChecksumURL); err != nil {
			return err
		}
	}
	return nil
}

// FileName is the name to look for in the checksum file.
func (i Image) FileName() string {
	if i.ChecksumName != "" {
		return i.ChecksumName
	}
	u, err := url.Parse(i.URL)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// Builtin returns the embedded list. It panics only if the embedded file is
// broken, which TestBuiltin prevents.
func Builtin() []Image {
	var l []Image
	if err := json.Unmarshal(builtinJSON, &l); err != nil {
		panic("catalog: images.json: " + err.Error())
	}
	return l
}

// Merge applies override entries over base by id. Invalid entries are skipped
// and reported in the returned problems.
func Merge(base, override []Image) (merged []Image, problems []string) {
	merged = append([]Image(nil), base...)
	for n, o := range override {
		if err := Validate(o); err != nil {
			label := o.ID
			if label == "" {
				label = fmt.Sprintf("#%d", n+1)
			}
			problems = append(problems, fmt.Sprintf("entry %s: %v", label, err))
			continue
		}
		replaced := false
		for k := range merged {
			if merged[k].ID == o.ID {
				merged[k], replaced = o, true
				break
			}
		}
		if !replaced {
			merged = append(merged, o)
		}
	}
	return merged, problems
}

// ParseOverride decodes an override file. Entries that are valid JSON but not
// valid images are left to Merge to report; unknown fields are an error so
// that a typo in a field name does not silently lose a setting.
func ParseOverride(data []byte) ([]Image, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var l []Image
	if err := dec.Decode(&l); err != nil {
		return nil, err
	}
	return l, nil
}

// Source loads the catalogue, re-reading <data>/images.json when it changes.
type Source struct {
	DataDir string
	Logf    func(format string, args ...any)

	mu     sync.Mutex
	stamp  string
	cached []Image
}

func (s *Source) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// Images returns the current catalogue. Problems with the override file are
// logged once per change of the file; the built-in list is always available.
func (s *Source) Images() []Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := "none"
	var file string
	if s.DataDir != "" {
		file = filepath.Join(s.DataDir, "images.json")
		if fi, err := os.Stat(file); err == nil {
			stamp = fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size())
		} else if !os.IsNotExist(err) {
			stamp = "err:" + err.Error()
		}
	}
	if s.cached != nil && stamp == s.stamp {
		return s.cached
	}
	s.stamp = stamp
	list := Builtin()
	if stamp != "none" {
		data, err := os.ReadFile(file)
		if err != nil {
			s.logf("image catalogue: cannot read %s: %v (using the built-in list)", file, err)
		} else if over, err := ParseOverride(data); err != nil {
			s.logf("image catalogue: %s is not valid: %v (using the built-in list)", file, err)
		} else {
			var problems []string
			list, problems = Merge(list, over)
			for _, p := range problems {
				s.logf("image catalogue: %s: %s (skipped)", file, p)
			}
		}
	}
	s.cached = list
	return list
}

// Find returns the entry with the given id.
func (s *Source) Find(id string) (Image, bool) {
	for _, i := range s.Images() {
		if i.ID == id {
			return i, true
		}
	}
	return Image{}, false
}

// ParseChecksum finds the digest for file in the text of a checksum file. It
// understands the GNU form ("<hex>  name" or "<hex> *name", Debian, Ubuntu,
// AlmaLinux, CirrOS), the BSD form ("SHA256 (name) = <hex>", Rocky, Fedora),
// ignores comments, blank lines and the PGP armour around a clear-signed
// file, and accepts names with a leading "./" or a directory part. The digest
// must have the length of algorithm; a BSD line that names a different
// algorithm is ignored. The result is lower case.
func ParseChecksum(text, file, algorithm string) (string, error) {
	want := HashLen(algorithm)
	if want == 0 {
		return "", fmt.Errorf("unsupported algorithm %q", algorithm)
	}
	if file == "" {
		return "", errors.New("no file name to look for")
	}
	found := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-----") || strings.HasPrefix(line, "Hash:") {
			continue
		}
		var name, sum string
		if m := bsdLine.FindStringSubmatch(line); m != nil {
			if !strings.EqualFold(strings.ReplaceAll(m[1], "-", ""), algorithm) {
				continue
			}
			name, sum = m[2], m[3]
		} else if m := gnuLine.FindStringSubmatch(line); m != nil {
			sum, name = m[1], m[2]
		} else {
			continue
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "*"), "./")
		if name != file && path.Base(name) != file {
			continue
		}
		if len(sum) != want {
			continue
		}
		sum = strings.ToLower(sum)
		if found != "" && found != sum {
			return "", fmt.Errorf("the checksum file lists %s with different checksums", file)
		}
		found = sum
	}
	if found == "" {
		return "", fmt.Errorf("no %s checksum for %s in the checksum file", algorithm, file)
	}
	return found, nil
}

var (
	bsdLine = regexp.MustCompile(`^(?i:(SHA-?256|SHA-?512))\s*\(\s*(.+?)\s*\)\s*=\s*([0-9a-fA-F]+)$`)
	gnuLine = regexp.MustCompile(`^([0-9a-fA-F]{32,})\s+(.+)$`)
)
