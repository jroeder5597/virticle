package ctlbackup

// Encryption is the age format (https://age-encryption.org/v1) with one
// scrypt recipient, i.e. a passphrase. age is small, has a written
// specification, authenticates every 64 KiB chunk (a changed byte is an error,
// not garbage) and can be opened with the age tool on any machine, so a backup
// stays readable even without Virticle: age -d FILE | tar xz. The tar.gz inside
// is the same as an unencrypted backup.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"filippo.io/age"
)

// MinPassphrase is the shortest passphrase accepted for a new backup.
const MinPassphrase = 10

// CheckPassphrase is the whole passphrase policy for making a backup.
func CheckPassphrase(p string) error {
	if utf8.RuneCountInString(p) < MinPassphrase {
		return fmt.Errorf("the passphrase must be at least %d characters", MinPassphrase)
	}
	return nil
}

var ageMagic = []byte("age-encryption.org/")

// Encrypt returns a writer that encrypts into w with the passphrase. Close it
// to finish the file.
func Encrypt(w io.Writer, pass string) (io.WriteCloser, error) {
	rec, err := age.NewScryptRecipient(pass)
	if err != nil {
		return nil, err
	}
	return age.Encrypt(w, rec)
}

// ErrWrongPassphrase is returned by Open when the passphrase does not fit.
var ErrWrongPassphrase = errors.New("wrong passphrase")

// ErrNeedPassphrase is returned by Open for an encrypted backup when no
// passphrase was given.
var ErrNeedPassphrase = errors.New("this backup is encrypted; a passphrase is needed")

// IsEncrypted looks at the first bytes of a backup file.
func IsEncrypted(head []byte) bool { return bytes.HasPrefix(head, ageMagic) }

// Open returns the plain archive stream of a backup file, decrypting it when
// it is encrypted. ask is called only for an encrypted backup; it may be nil,
// which gives ErrNeedPassphrase.
func Open(r io.Reader, ask func() (string, error)) (io.Reader, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(len(ageMagic))
	if !IsEncrypted(head) {
		return br, nil
	}
	if ask == nil {
		return nil, ErrNeedPassphrase
	}
	pass, err := ask()
	if err != nil {
		return nil, err
	}
	id, err := age.NewScryptIdentity(pass)
	if err != nil {
		return nil, err
	}
	id.SetMaxWorkFactor(22)
	dr, err := age.Decrypt(br, id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, ErrWrongPassphrase
		}
		return nil, fmt.Errorf("this backup cannot be decrypted: %w", err)
	}
	return dr, nil
}
