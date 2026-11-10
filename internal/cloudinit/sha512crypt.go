package cloudinit

import (
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"strconv"
	"strings"
)

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// HashPassword returns a SHA-512 crypt hash ($6$) of the password with a
// fresh random salt, the format cloud-init's "passwd" field and /etc/shadow use.
func HashPassword(password string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	for i, b := range raw {
		salt[i] = cryptAlphabet[int(b)%64]
	}
	return sha512Crypt(password, string(salt), 5000), nil
}

// sha512Crypt implements Ulrich Drepper's SHA-crypt specification for $6$.
// rounds == 5000 is the default and is not written into the result.
func sha512Crypt(password, salt string, rounds int) string {
	if len(salt) > 16 {
		salt = salt[:16]
	}
	pw, sl := []byte(password), []byte(salt)
	sum := func(parts ...[]byte) []byte {
		h := sha512.New()
		for _, p := range parts {
			h.Write(p)
		}
		return h.Sum(nil)
	}
	repeat := func(b []byte, n int) []byte {
		out := make([]byte, 0, n)
		for len(out) < n {
			out = append(out, b...)
		}
		return out[:n]
	}

	b := sum(pw, sl, pw)
	a := sha512.New()
	a.Write(pw)
	a.Write(sl)
	a.Write(repeat(b, len(pw)))
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 == 1 {
			a.Write(b)
		} else {
			a.Write(pw)
		}
	}
	da := a.Sum(nil)

	dp := sha512.New()
	for i := 0; i < len(pw); i++ {
		dp.Write(pw)
	}
	p := repeat(dp.Sum(nil), len(pw))

	ds := sha512.New()
	for i := 0; i < 16+int(da[0]); i++ {
		ds.Write(sl)
	}
	s := repeat(ds.Sum(nil), len(sl))

	c := da
	for i := 0; i < rounds; i++ {
		h := sha512.New()
		if i&1 == 1 {
			h.Write(p)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(s)
		}
		if i%7 != 0 {
			h.Write(p)
		}
		if i&1 == 1 {
			h.Write(c)
		} else {
			h.Write(p)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	enc := func(b2, b1, b0 byte, n int) {
		v := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for i := 0; i < n; i++ {
			out.WriteByte(cryptAlphabet[v&0x3f])
			v >>= 6
		}
	}
	order := [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26},
		{6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32},
		{12, 33, 54}, {34, 55, 13}, {56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38},
		{18, 39, 60}, {40, 61, 19}, {62, 20, 41}}
	for _, o := range order {
		enc(c[o[0]], c[o[1]], c[o[2]], 4)
	}
	enc(0, 0, c[63], 2)

	prefix := "$6$"
	if rounds != 5000 {
		prefix += "rounds=" + strconv.Itoa(rounds) + "$"
	}
	return fmt.Sprintf("%s%s$%s", prefix, salt, out.String())
}
