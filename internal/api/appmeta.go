package api

import (
	"errors"
	"log"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
	"virticle/internal/snapsched"
)

// The application keeps its per-VM data (template mark, tags, snapshot and
// backup schedules) in ONE element of the domain's <metadata>. Versions
// before the rename used the namespace snapsched.LegacyNS; the current one is
// snapsched.NS. Reads accept both, writes produce only the current one.

// metaStore is the part of libvirt that holds that element, so the rules
// below can be tested without a node.
type metaStore interface {
	// get returns the element of namespace ns; "" when the domain has none.
	get(ns string) (string, error)
	// set stores value as the element of ns under the given prefix; "" removes it.
	set(ns, prefix, value string) error
}

type libvirtMeta struct {
	h   *hostconn.Host
	dom libvirt.Domain
}

func isNoMetadata(err error) bool {
	var lv libvirt.Error
	return errors.As(err, &lv) && lv.Code == uint32(libvirt.ErrNoDomainMetadata)
}

func (m libvirtMeta) get(ns string) (string, error) {
	v, err := m.h.DomainGetMetadata(m.dom, int32(libvirt.DomainMetadataElement), libvirt.OptString{ns}, metaFlags(m.h, m.dom, false))
	if isNoMetadata(err) {
		return "", nil
	}
	return v, err
}

func (m libvirtMeta) set(ns, prefix, value string) error {
	var v, p libvirt.OptString
	if value != "" {
		v = libvirt.OptString{value}
	}
	if prefix != "" {
		p = libvirt.OptString{prefix}
	}
	err := m.h.DomainSetMetadata(m.dom, int32(libvirt.DomainMetadataElement), v, p, libvirt.OptString{ns}, metaFlags(m.h, m.dom, true))
	if value == "" && isNoMetadata(err) {
		return nil
	}
	return err
}

// readMeta returns the application metadata of a domain. The element under
// the current namespace wins; only when there is none is the legacy one used.
// The two are never merged: every write stores the complete content under
// the current namespace.
func readMeta(m metaStore) (string, error) {
	v, err := m.get(snapsched.NS)
	if err != nil || v != "" {
		return v, err
	}
	return m.get(snapsched.LegacyNS)
}

// writeMeta stores the complete metadata under the current namespace and
// then removes the legacy element, so a domain never keeps two copies that
// can drift apart. If only the removal fails the new element is still the
// one that is read, so the failure is logged and not returned.
func writeMeta(m metaStore, meta string) error {
	if err := m.set(snapsched.NS, snapsched.Prefix, meta); err != nil {
		return err
	}
	if err := m.set(snapsched.LegacyNS, "", ""); err != nil {
		log.Printf("metadata: could not remove the old-namespace element after writing the new one (the new one is used): %v", err)
	}
	return nil
}

func domainMeta(h *hostconn.Host, dom libvirt.Domain) (string, error) {
	return readMeta(libvirtMeta{h, dom})
}

func setDomainMeta(h *hostconn.Host, dom libvirt.Domain, meta string) error {
	return writeMeta(libvirtMeta{h, dom}, meta)
}
