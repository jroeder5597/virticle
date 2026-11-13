// Package ldaptest is a small in-process LDAP server for tests: simple binds,
// searches (and, or, not, equality, present and the Active Directory
// in-chain rule), the rootDSE, LDAPS and StartTLS with a throw-away CA. It is
// not a directory; it holds a fixed list of entries.
package ldaptest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// Entry is one directory entry.
type Entry struct {
	DN    string
	Attrs map[string][]string
}

// Server is a running fake directory.
type Server struct {
	Entries   []Entry
	Passwords map[string]string // DN to password for simple binds
	AD        bool              // advertise Active Directory in the rootDSE
	CAPEM     string            // the CA that signed the server certificate

	mu    sync.Mutex
	plain net.Listener
	tlsL  net.Listener
	cfg   *tls.Config
	// Binds records the DNs bound, in order, for assertions.
	Binds []string
}

// Start listens on loopback: a plain port (StartTLS is available on it) and an
// LDAPS port. Close stops both.
func Start() (*Server, error) {
	s := &Server{Passwords: map[string]string{}}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	s.CAPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	s.cfg = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	var err error
	if s.plain, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return nil, err
	}
	if s.tlsL, err = tls.Listen("tcp", "127.0.0.1:0", s.cfg); err != nil {
		s.plain.Close()
		return nil, err
	}
	go s.accept(s.plain)
	go s.accept(s.tlsL)
	return s, nil
}

// PlainURL is the ldap:// address (StartTLS works on it).
func (s *Server) PlainURL() string { return "ldap://localhost:" + port(s.plain) }

// TLSURL is the ldaps:// address.
func (s *Server) TLSURL() string { return "ldaps://localhost:" + port(s.tlsL) }

func port(l net.Listener) string {
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func (s *Server) Close() {
	s.plain.Close()
	s.tlsL.Close()
}

func (s *Server) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer func() { c.Close() }()
	r := bufio.NewReader(c)
	for {
		p, err := ber.ReadPacket(r)
		if err != nil || len(p.Children) < 2 {
			return
		}
		id := p.Children[0].Value
		op := p.Children[1]
		switch op.Tag {
		case 0: // bind
			dn, _ := op.Children[1].Value.(string)
			pw := string(op.Children[2].Data.Bytes())
			code := 49 // invalidCredentials
			s.mu.Lock()
			s.Binds = append(s.Binds, dn)
			if want, ok := s.Passwords[dn]; ok && want == pw && pw != "" {
				code = 0
			}
			s.mu.Unlock()
			reply(c, id, result(1, code))
		case 2: // unbind
			return
		case 3: // search
			s.search(c, id, op)
		case 23: // extended: StartTLS
			if oid := string(op.Children[0].Data.Bytes()); oid == "1.3.6.1.4.1.1466.20037" {
				if _, isTLS := c.(*tls.Conn); isTLS {
					reply(c, id, result(24, 2))
					continue
				}
				reply(c, id, result(24, 0))
				tc := tls.Server(c, s.cfg)
				if tc.Handshake() != nil {
					return
				}
				c = tc
				r = bufio.NewReader(tc)
			} else {
				reply(c, id, result(24, 2))
			}
		default:
			return
		}
	}
}

func result(tag ber.Tag, code int) *ber.Packet {
	p := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, int64(code), ""))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	return p
}

func reply(c net.Conn, id interface{}, op *ber.Packet) {
	m := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	m.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	m.AppendChild(op)
	c.Write(m.Bytes())
}

func (s *Server) search(c net.Conn, id interface{}, op *ber.Packet) {
	base, _ := op.Children[0].Value.(string)
	scope, _ := op.Children[1].Value.(int64)
	filter := op.Children[6]
	var out []Entry
	if base == "" && scope == 0 {
		caps := []string{}
		if s.AD {
			caps = []string{"1.2.840.113556.1.4.800"}
		}
		root := Entry{DN: "", Attrs: map[string][]string{"supportedCapabilities": caps, "defaultNamingContext": {s.suffix()}}}
		out = []Entry{root}
	} else {
		s.mu.Lock()
		for _, e := range s.Entries {
			if inScope(e.DN, base, scope) && s.match(filter, e) {
				out = append(out, e)
			}
		}
		s.mu.Unlock()
	}
	for _, e := range out {
		ent := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 4, nil, "")
		ent.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, e.DN, ""))
		attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		for k, vals := range e.Attrs {
			a := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
			a.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, k, ""))
			set := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
			for _, v := range vals {
				set.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, ""))
			}
			a.AppendChild(set)
			attrs.AppendChild(a)
		}
		ent.AppendChild(attrs)
		reply(c, id, ent)
	}
	reply(c, id, result(5, 0))
}

func (s *Server) suffix() string {
	if len(s.Entries) == 0 {
		return ""
	}
	dn := strings.ToLower(s.Entries[0].DN)
	if i := strings.Index(dn, "dc="); i >= 0 {
		return dn[i:]
	}
	return ""
}

func inScope(dn, base string, scope int64) bool {
	dn, base = strings.ToLower(dn), strings.ToLower(base)
	switch scope {
	case 0:
		return dn == base
	case 1:
		i := strings.Index(dn, ",")
		return i >= 0 && dn[i+1:] == base
	}
	return base == "" || dn == base || strings.HasSuffix(dn, ","+base)
}

func (e Entry) has(attr, val string) bool {
	for k, vs := range e.Attrs {
		if strings.EqualFold(k, attr) {
			for _, v := range vs {
				if strings.EqualFold(v, val) {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) match(f *ber.Packet, e Entry) bool {
	switch f.Tag {
	case 0: // and
		for _, c := range f.Children {
			if !s.match(c, e) {
				return false
			}
		}
		return true
	case 1: // or
		for _, c := range f.Children {
			if s.match(c, e) {
				return true
			}
		}
		return false
	case 2: // not
		return !s.match(f.Children[0], e)
	case 3: // equality
		attr, _ := f.Children[0].Value.(string)
		val, _ := f.Children[1].Value.(string)
		if strings.EqualFold(attr, "dn") {
			return strings.EqualFold(e.DN, val)
		}
		return e.has(attr, val)
	case 7: // present
		attr := string(f.Data.Bytes())
		for k := range e.Attrs {
			if strings.EqualFold(k, attr) {
				return true
			}
		}
		return false
	case 9: // extensible match: only the in-chain rule on a membership attribute
		var attr, val string
		for _, c := range f.Children {
			switch c.Tag {
			case 2:
				attr = string(c.Data.Bytes())
			case 3:
				val = string(c.Data.Bytes())
			}
		}
		return s.inChain(e, attr, val, map[string]bool{})
	}
	return false
}

// inChain: e is a member of val, directly or through groups that are members of groups.
func (s *Server) inChain(e Entry, attr, val string, seen map[string]bool) bool {
	if seen[strings.ToLower(e.DN)] {
		return false
	}
	seen[strings.ToLower(e.DN)] = true
	if e.has(attr, val) {
		return true
	}
	for _, g := range s.Entries {
		if e.has(attr, g.DN) && s.inChain(g, attr, val, seen) {
			return true
		}
	}
	return false
}
