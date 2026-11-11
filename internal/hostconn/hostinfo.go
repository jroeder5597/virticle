package hostconn

import (
	"encoding/xml"
	"errors"
	"time"
)

// HostUUID is the libvirt host UUID (the <host><uuid> of the capabilities),
// the stable identity of a machine regardless of how it is addressed.
func (h *Host) HostUUID() (string, error) {
	caps, err := h.ConnectGetCapabilities()
	if err != nil {
		return "", err
	}
	return ParseHostUUID(caps)
}

// ParseHostUUID reads the host UUID from capabilities XML.
func ParseHostUUID(capsXML string) (string, error) {
	var x struct {
		Host struct {
			UUID string `xml:"uuid"`
		} `xml:"host"`
	}
	if err := xml.Unmarshal([]byte(capsXML), &x); err != nil {
		return "", err
	}
	if x.Host.UUID == "" {
		return "", errors.New("the capabilities carry no host uuid")
	}
	return x.Host.UUID, nil
}

// DialTimeout is Dial bounded by d as a whole: the SSH handshakes and the
// libvirt session setup together. The libvirt handshake has no timeout of its
// own, so a node that accepts SSH but never answers would hang the caller.
func DialTimeout(t Target, knownHosts string, d time.Duration) (*Host, error) {
	t.Timeout = d
	if t.Jump != nil {
		j := *t.Jump
		j.Timeout = d
		t.Jump = &j
	}
	type res struct {
		h   *Host
		err error
	}
	ch := make(chan res, 1)
	go func() {
		h, err := Dial(t, knownHosts)
		ch <- res{h, err}
	}()
	select {
	case r := <-ch:
		return r.h, r.err
	case <-time.After(d + 3*time.Second):
		go func() {
			if r := <-ch; r.h != nil {
				r.h.Close()
			}
		}()
		return nil, errors.New("timed out connecting")
	}
}
