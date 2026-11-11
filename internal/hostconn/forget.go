package hostconn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"os"
	"strings"

	"golang.org/x/crypto/ssh/knownhosts"
)

// ForgetHostKey removes every key recorded for addr from the known_hosts file
// at path, so that the next connection trusts whatever key the host presents
// (trust on first use again). It returns how many lines it removed or
// shortened. Hashed entries (HashKnownHosts) are recognised too.
func ForgetHostKey(path, addr string) (int, error) {
	khMu.Lock()
	defer khMu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	out, n := dropHost(string(b), knownhosts.Normalize(withPort(addr)))
	if n == 0 {
		return 0, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp, path)
}

// dropHost removes name from the host lists of known_hosts content. A line
// whose list becomes empty is dropped.
func dropHost(content, name string) (string, int) {
	var out []string
	n := 0
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.HasPrefix(line, "#") {
			out = append(out, line)
			continue
		}
		if strings.HasPrefix(f[0], "|1|") { // hashed entry: one host, HMAC-SHA1 of the name
			if hashedMatches(f[0], name) {
				n++
			} else {
				out = append(out, line)
			}
			continue
		}
		hosts := strings.Split(f[0], ",")
		var keep []string
		for _, h := range hosts {
			if h != name {
				keep = append(keep, h)
			}
		}
		switch {
		case len(keep) == len(hosts):
			out = append(out, line)
		case len(keep) > 0:
			f[0] = strings.Join(keep, ",")
			out = append(out, strings.Join(f, " "))
			n++
		default:
			n++
		}
	}
	return strings.Join(out, "\n"), n
}

// hashedMatches reports whether a |1|salt|hash host field is the hash of name.
func hashedMatches(field, name string) bool {
	p := strings.Split(field, "|")
	if len(p) != 4 {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(p[2])
	want, err2 := base64.StdEncoding.DecodeString(p[3])
	if err1 != nil || err2 != nil {
		return false
	}
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(name))
	return hmac.Equal(m.Sum(nil), want)
}
