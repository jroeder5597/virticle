package tlscert

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"testing"
	"time"
)

// A certificate made on first start for a listener on every interface covers
// the machine's host name and each of its addresses, so opening
// https://<address>:8443 from another computer shows only the self-signed
// warning and no name mismatch.
func TestFirstCertificateCoversMachineNamesAndAddresses(t *testing.T) {
	dir := t.TempDir()
	cf, _, generated, err := Ensure(dir, NamesFor(":8443"), time.Now())
	if err != nil || !generated {
		t.Fatalf("Ensure: generated=%v err=%v", generated, err)
	}
	b, _ := os.ReadFile(cf)
	blk, _ := pem.Decode(b)
	x, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		if x.VerifyHostname(h) != nil {
			t.Errorf("host name %q missing from %v", h, x.DNSNames)
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			if x.VerifyHostname(n.IP.String()) != nil {
				t.Errorf("address %s missing from %v", n.IP, x.IPAddresses)
			}
		}
	}
}
