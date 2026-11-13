// Package totp implements time-based one-time passwords (RFC 6238 with
// HMAC-SHA-1, 6 digits, 30 second steps), which is what Google Authenticator,
// Microsoft Authenticator, Duo Mobile, Authy and 1Password all speak.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Period is the length of one step in seconds.
	Period = 30
	digits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit seed in the base32 form apps accept.
func NewSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// Step is the time step number of t.
func Step(t time.Time) int64 { return t.Unix() / Period }

// Code is the code for a step.
func Code(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("bad secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, v%1000000), nil
}

// Verify checks code against the steps just before, at and just after now (a
// phone clock may be off by up to half a minute either way). Only a step
// later than lastStep is accepted, so a code cannot be used twice. It returns
// the step that matched; the caller must store it as the new lastStep, and do
// so atomically with the check.
func Verify(secret, code string, now time.Time, lastStep int64) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits {
		return 0, false
	}
	cur := Step(now)
	for _, s := range []int64{cur - 1, cur, cur + 1} {
		if s <= lastStep {
			continue
		}
		want, err := Code(secret, s)
		if err == nil && hmac.Equal([]byte(want), []byte(code)) {
			return s, true
		}
	}
	return 0, false
}

// URI is the otpauth address apps read from the QR code.
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}
