// Package secrets seals small secrets (two-step sign-in seeds, the identity
// provider's client secret, the directory bind password) before they go into
// the database. The key is a random file in the data directory, so a copy of
// the database alone does not reveal them; whoever can read the data
// directory can (it already holds the controller's SSH key).
package secrets

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

const (
	keyFile = "secret.key"
	prefix  = "v1:"
)

// Box seals and opens strings with AES-256-GCM.
type Box struct{ aead cipher.AEAD }

// New makes a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("the secret key must be 32 bytes")
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	a, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &Box{a}, nil
}

// Open loads the key from the data directory, creating it (mode 0600) on
// first use. Two processes starting at once settle on one key: the file is
// created exclusively and the loser reads the winner's.
func Open(dataDir string) (*Box, error) {
	path := filepath.Join(dataDir, keyFile)
	for range 3 {
		b, err := os.ReadFile(path)
		if err == nil {
			key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
			if err != nil || len(key) != 32 {
				return nil, fmt.Errorf("%s is damaged: remove it only if you accept that two-step sign-in and stored sign-in secrets must be set up again", path)
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

// Seal returns an opaque string holding s.
func (b *Box) Seal(s string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawStdEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(s), nil)), nil
}

// Unseal reverses Seal.
func (b *Box) Unseal(sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return "", errors.New("not a sealed value")
	}
	ct, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil || len(ct) < b.aead.NonceSize() {
		return "", errors.New("sealed value is damaged")
	}
	pt, err := b.aead.Open(nil, ct[:b.aead.NonceSize()], ct[b.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("cannot open the sealed value: the key file does not match")
	}
	return string(pt), nil
}
