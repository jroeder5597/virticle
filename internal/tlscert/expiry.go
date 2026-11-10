package tlscert

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// NotAfter returns the expiry time of the first certificate in a PEM file,
// which is the one the server presents.
func NotAfter(certFile string) (time.Time, error) {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return time.Time{}, err
	}
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return time.Time{}, fmt.Errorf("no certificate in %s", certFile)
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return c.NotAfter, nil
	}
}
