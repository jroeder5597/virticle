package api

// Live migration coordinated by the controller.
//
// libvirt's peer-to-peer migration makes the source libvirtd open a
// qemu+ssh connection to the target as root. With a scoped service account
// there is no key for that: the account's SSH key may only deliver a file (see
// peertrust.go). So when either node is in helper mode the controller does
// what virDomainMigrate does on the client side: it calls the five phases of
// libvirt's migration protocol itself, on its own two libvirt connections.
//
//	Begin (source) -> Prepare (target) -> Perform (source) -> Finish (target) -> Confirm (source)
//
// The controller carries only the cookies and the domain XML. The memory
// stream and the NBD disk mirror still run between the two QEMU processes, node
// to node, to the address stored for the target. Nothing needs SSH between
// the nodes. Root and sudo nodes keep the peer-to-peer path.

import (
	"fmt"
	"log"

	"github.com/digitalocean/go-libvirt"
)

// withParam returns params with the string parameter field set to value.
func withParam(params []libvirt.TypedParam, field, value string) []libvirt.TypedParam {
	out := make([]libvirt.TypedParam, 0, len(params)+1)
	for _, p := range params {
		if p.Field != field {
			out = append(out, p)
		}
	}
	return append(out, libvirt.TypedParam{Field: field, Value: *libvirt.NewTypedParamValueString(value)})
}

// performFromController runs the migration phases and returns the error of
// the first that failed. The source is told to resume when anything fails, and
// the target to discard what it prepared.
func (m *migRun) performFromController(flags libvirt.DomainMigrateFlags, params []libvirt.TypedParam) error {
	// Not peer-to-peer: the controller is the client of both libvirtds.
	flags &^= libvirt.MigratePeer2peer
	// The target does not take the options that only the source acts on.
	destFlags := uint32(flags &^ (libvirt.MigrateAutoConverge | libvirt.MigrateAbortOnError))

	cookie, xml, err := m.src.DomainMigrateBegin3Params(m.dom, params, uint32(flags))
	if err != nil {
		return fmt.Errorf("starting the migration on %s: %w", m.src.Name, err)
	}
	succeeded := false
	var confirmCookie []byte
	defer func() {
		if succeeded {
			return
		}
		if err := m.src.DomainMigrateConfirm3Params(m.dom, params, confirmCookie, uint32(flags), 1); err != nil {
			log.Printf("migrate %s: ending the failed migration on %s: %v", m.plan.Name, m.src.Name, err)
		}
	}()

	dparams := withParam(withParam(params, libvirt.MigrateParamDestXML, xml), libvirt.MigrateParamDestName, m.dom.Name)
	cookie, uriOut, err := m.dst.DomainMigratePrepare3Params(dparams, cookie, destFlags)
	if err != nil {
		return fmt.Errorf("preparing %s: %w", m.dst.Name, err)
	}
	pparams := params
	if len(uriOut) > 0 && uriOut[0] != "" {
		pparams = withParam(params, libvirt.MigrateParamURI, uriOut[0])
	}

	cookie, perr := m.src.DomainMigratePerform3Params(m.dom, nil, pparams, cookie, flags)
	cancelled := int32(0)
	if perr != nil {
		cancelled = 1
	}
	_, fcookie, ferr := m.dst.DomainMigrateFinish3Params(dparams, cookie, destFlags, cancelled)
	if perr != nil || ferr != nil {
		m.discardIncoming()
	}
	if perr != nil {
		return perr
	}
	if ferr != nil {
		return fmt.Errorf("finishing the migration on %s: %w", m.dst.Name, ferr)
	}
	confirmCookie = fcookie
	succeeded = true
	if err := m.src.DomainMigrateConfirm3Params(m.dom, params, confirmCookie, uint32(flags), 0); err != nil {
		// The VM runs on the target; the source could not tidy up.
		log.Printf("migrate %s: confirming on %s: %v", m.plan.Name, m.src.Name, err)
	}
	return nil
}

// discardIncoming destroys the paused, not yet defined domain a failed
// migration can leave on the target (the pre-flight made sure that no domain of
// this UUID existed there before). A domain that is running, or defined, is
// left alone for the caller to judge.
func (m *migRun) discardIncoming() {
	d, err := m.dst.DomainLookupByUUID(m.dom.UUID)
	if err != nil {
		return
	}
	if persistent, err := m.dst.DomainIsPersistent(d); err != nil || persistent != 0 {
		return
	}
	if state, _, err := m.dst.DomainGetState(d, 0); err != nil || stateName(uint8(state)) != "paused" {
		return
	}
	if err := m.dst.DomainDestroy(d); err != nil {
		log.Printf("migrate %s: discarding the incoming domain on %s: %v", m.plan.Name, m.dst.Name, err)
	}
}
