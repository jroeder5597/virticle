// Package secretbox seals small secrets (two-step sign-in seeds, identity
// provider and directory passwords, notification addresses and signing secrets)
// before they are written to the database. The key is a random file,
// secret.key, in the data directory, created on first use with mode 0600. This
// protects the database file and its backups when they are copied away without
// the data directory; whoever can read the whole directory can open them.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFile is the name of the key in the data directory.
const KeyFile = "secret.key"

// legacyKeyFile is where an earlier build kept its key (32 raw bytes). A data
// directory that has it and no KeyFile adopts it, so nothing sealed is lost.
const legacyKeyFile = "settings.key"

const prefix = "v1:"

// ErrUnreadable is returned when a stored secret cannot be decrypted, usually
// because the key file was replaced or lost.
var ErrUnreadable = errors.New("the stored secret cannot be read with this controller's key")

type Box struct{ aead cipher.AEAD }

// New makes a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("the secret key must be 32 bytes")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Box{g}, nil
}

// OpenDir loads the key from the data directory, creating it (mode 0600) on
// first use. Two processes starting at once settle on one key: the file is
// created exclusively and the loser reads the winner's. A key left by an
// earlier build under the old name is renamed into place first. An empty dir
// gives a key that lives only in memory (tests).
func OpenDir(dir string) (*Box, error) {
	if dir == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		return New(key)
	}
	path := filepath.Join(dir, KeyFile)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		adoptLegacyKey(dir)
	}
	for range 3 {
		b, err := os.ReadFile(path)
		if err == nil {
			key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("%s is damaged: remove it only if you accept that two-step sign-in, notification addresses and other stored secrets must be set up again", path)
			}
			return New(key)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return nil, werr
		}
		return New(key)
	}
	return nil, errors.New("could not create " + path)
}

// adoptLegacyKey turns dir/settings.key (32 raw bytes) into dir/secret.key
// (base64 text) and removes the old file. Any failure leaves both as they were.
func adoptLegacyKey(dir string) {
	old := filepath.Join(dir, legacyKeyFile)
	raw, err := os.ReadFile(old)
	if err != nil || len(raw) != 32 {
		return
	}
	tmp := filepath.Join(dir, KeyFile+".adopt")
	if os.WriteFile(tmp, []byte(base64.StdEncoding.EncodeToString(raw)+"\n"), 0o600) != nil {
		return
	}
	if os.Rename(tmp, filepath.Join(dir, KeyFile)) != nil {
		os.Remove(tmp)
		return
	}
	os.Remove(old)
}

// Seal returns the encrypted, text-safe form of s. The empty string stays empty.
func (b *Box) Seal(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawStdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(s), nil)), nil
}

// Open reverses Seal.
func (b *Box) Open(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil || !strings.HasPrefix(s, prefix) || len(raw) < b.aead.NonceSize() {
		return "", ErrUnreadable
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", ErrUnreadable
	}
	return string(plain), nil
}
