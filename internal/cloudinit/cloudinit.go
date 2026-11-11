// Package cloudinit builds NoCloud seed images (and other small "files on a
// labelled volume" images) in memory, in pure Go.
package cloudinit

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Label is the volume label cloud-init's NoCloud datasource looks for.
const Label = "cidata"

// Config describes what the guest should be provisioned with.
type Config struct {
	Hostname string
	User     string
	Password string // hashed (SHA-512 crypt) before it is written
	// PasswordHash is an already hashed password ($6$...), written as is. It
	// is what saved profiles carry; Password wins if both are set.
	PasswordHash string
	SSHKeys      []string
	Packages     []string
	// RawUserData, when set, replaces the generated user-data verbatim.
	RawUserData string
	// NetworkConfig, when set, is written as network-config verbatim.
	NetworkConfig string
}

var (
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	userRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// HashRE matches a SHA-512 crypt hash as HashPassword makes it.
	HashRE    = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./0-9A-Za-z]{1,16}\$[./0-9A-Za-z]{86}$`)
	packageRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_:=~-]*$`)
)

// Validate checks the user-supplied fields.
func (c Config) Validate() error {
	if c.RawUserData != "" {
		return nil
	}
	if c.Hostname != "" && (len(c.Hostname) > 253 || !hostnameRE.MatchString(c.Hostname)) {
		return fmt.Errorf("invalid hostname %q", c.Hostname)
	}
	if c.User != "" && !userRE.MatchString(c.User) {
		return fmt.Errorf("invalid user name %q: use lowercase letters, digits, dash and underscore", c.User)
	}
	if c.PasswordHash != "" && !HashRE.MatchString(c.PasswordHash) {
		return fmt.Errorf("the password hash is not a SHA-512 crypt hash")
	}
	if (c.Password != "" || c.PasswordHash != "") && c.User == "" {
		return fmt.Errorf("a password needs a user name")
	}
	if len(c.SSHKeys) > 0 && c.User == "" {
		return fmt.Errorf("SSH keys need a user name")
	}
	for _, k := range c.SSHKeys {
		if f := strings.Fields(k); len(f) < 2 || !strings.HasPrefix(f[0], "ssh-") && !strings.HasPrefix(f[0], "ecdsa-") && !strings.HasPrefix(f[0], "sk-") {
			return fmt.Errorf("not an SSH public key: %.40q", k)
		}
	}
	for _, p := range c.Packages {
		if !packageRE.MatchString(p) {
			return fmt.Errorf("invalid package name %q", p)
		}
	}
	return nil
}

// quote renders s as a YAML double-quoted scalar. JSON string syntax is a
// subset of YAML's, so the encoder does all the escaping.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSpace(b.String())
}

// UserData renders the #cloud-config document (or the raw override).
func (c Config) UserData() (string, error) {
	if c.RawUserData != "" {
		return c.RawUserData, nil
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	if c.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\nmanage_etc_hosts: true\n", quote(c.Hostname))
	}
	if c.User != "" {
		fmt.Fprintf(&b, "users:\n  - name: %s\n    shell: /bin/bash\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n", quote(c.User))
		if hash := c.PasswordHash; c.Password != "" || hash != "" {
			if c.Password != "" {
				var err error
				if hash, err = HashPassword(c.Password); err != nil {
					return "", err
				}
			}
			fmt.Fprintf(&b, "    lock_passwd: false\n    passwd: %s\n", quote(hash))
		}
		if len(c.SSHKeys) > 0 {
			b.WriteString("    ssh_authorized_keys:\n")
			for _, k := range c.SSHKeys {
				fmt.Fprintf(&b, "      - %s\n", quote(strings.TrimSpace(k)))
			}
		}
	}
	if c.Password != "" || c.PasswordHash != "" {
		b.WriteString("ssh_pwauth: true\n")
	}
	if len(c.Packages) > 0 {
		b.WriteString("package_update: true\npackages:\n")
		for _, p := range c.Packages {
			fmt.Fprintf(&b, "  - %s\n", quote(p))
		}
		// The agent's unit is started by a udev rule when the virtio channel
		// appears, which is before a first-boot install; start it by hand.
		for _, p := range c.Packages {
			if p == "qemu-guest-agent" {
				b.WriteString("runcmd:\n  - [systemctl, start, qemu-guest-agent]\n")
			}
		}
	}
	return b.String(), nil
}

// MetaData renders meta-data with a fresh instance id, so cloud-init treats
// every new seed as a first boot.
func (c Config) MetaData() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	s := "instance-id: " + quote("iid-"+hex.EncodeToString(raw)) + "\n"
	if c.Hostname != "" {
		s += "local-hostname: " + quote(c.Hostname) + "\n"
	}
	return s, nil
}

// Seed builds the NoCloud ISO for the configuration.
func (c Config) Seed() ([]byte, error) {
	ud, err := c.UserData()
	if err != nil {
		return nil, err
	}
	md, err := c.MetaData()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"user-data": []byte(ud), "meta-data": []byte(md)}
	if c.NetworkConfig != "" {
		files["network-config"] = []byte(c.NetworkConfig)
	}
	return BuildImage(Label, files)
}
