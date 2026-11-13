// Package secretbox encrypts small secrets (webhook addresses, signing
// secrets) before they are written to the database. The key is a random file in
// the data directory, created on first use with mode 0600. This protects the
// database file and its backups when they are copied away without the data
// directory; it does not protect against someone who can read the whole
// directory.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const prefix = "v1:"

// ErrUnreadable is returned when a stored secret cannot be decrypted, usually
// because the key file was replaced or lost.
var ErrUnreadable = errors.New("the stored secret cannot be read with this controller's key")

type Box struct{ aead cipher.AEAD }

// OpenFile loads the key at path, creating it when it does not exist. An empty
// path gives a key that lives only in memory (tests).
func OpenFile(path string) (*Box, error) {
	var key []byte
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("%s: the key file must be 32 bytes", path)
			}
			key = b
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if path != "" {
			if err := os.WriteFile(path, key, 0o600); err != nil {
				return nil, err
			}
		}
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
