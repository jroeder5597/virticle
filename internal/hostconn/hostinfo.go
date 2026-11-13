package hostconn

import (
	"encoding/xml"
	"errors"
	"time"
)

// AvailableMemory is the memory in bytes a new VM can count on: libvirt's free memory plus the buffers and the
// page cache, which the kernel gives back at once. libvirt's own "free" figure leaves the cache out, so a node
// that has just copied backups or read an NFS share looks full (a 2.6 GiB node showed 170 MiB free with 1.8 GiB of
// cache) and migrations or an evacuation to it were refused for lack of memory. A host that does not report the
// statistics falls back to libvirt's free figure.
func (h *Host) AvailableMemory() (uint64, error) {
	if _, n, err := h.NodeGetMemoryStats(0, -1, 0); err == nil && n > 0 {
		if st, _, err := h.NodeGetMemoryStats(n, -1, 0); err == nil {
			fields := map[string]uint64{}
			for _, s := range st {
				fields[s.Field] = s.Value
			}
			if v := availableFromStats(fields); v > 0 {
				return v, nil
			}
		}
	}
	return h.NodeGetFreeMemory()
}

// availableFromStats adds free, buffers and cached (KiB in libvirt's statistics) and returns bytes.
func availableFromStats(f map[string]uint64) uint64 {
	if _, ok := f["free"]; !ok {
		return 0
	}
	return (f["free"] + f["buffers"] + f["cached"]) * 1024
}

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
