package hostconn

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// A limit on storage calls to one node.
//
// The libvirt proxy daemon of a node serves the controller's connection with at most five calls in flight (its
// default max_client_requests). A storage call can wait for a long time: libvirt's refresh of a pool on an NFS
// share whose server is gone sits in the kernel until the server is back, and it holds the lock of that pool, so
// every later call that touches the pool, or lists the pools, waits behind it (this also happens when another
// program on the node, not the controller, refreshes the pool). If all five slots hold storage calls, even a
// call about networks or VMs on that node waits, and the node looks dead.
//
// So storage calls are limited to three at a time, which keeps two slots free. The short ones (listing pools,
// reading a pool's state) also have a time limit: a call that has not answered after callLimit is given up on
// (the caller gets ErrStorageBusy; the call itself goes on in the background and keeps its slot until libvirt
// answers). When three calls are given up on, every storage call fails at once instead of queueing, until one
// of them returns. Pages then answer within a few seconds, and then immediately, that the node's storage is
// not answering.
//
// Long operations (creating, uploading, deleting volumes) are not in the gate: a person starts them for one
// pool, after the checks in the API.
const storageSlots = 3

var (
	// gateWait is how long a storage call waits for a free slot.
	gateWait = 2 * time.Second
	// callLimit is how long a short storage call may take.
	callLimit = 6 * time.Second
)

// ErrStorageBusy is returned when the storage calls of a node are stuck.
var ErrStorageBusy = errors.New("the storage service of this node is not answering: a storage call is stuck, usually on an NFS share whose server is gone. It continues when that call ends")

type storGate struct {
	once  sync.Once
	sem   chan struct{}
	stuck atomic.Int32 // calls given up on that have not returned yet
}

func (g *storGate) acquire() error {
	g.once.Do(func() { g.sem = make(chan struct{}, storageSlots) })
	if g.stuck.Load() >= storageSlots {
		return ErrStorageBusy
	}
	select {
	case g.sem <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(gateWait)
	defer t.Stop()
	select {
	case g.sem <- struct{}{}:
		return nil
	case <-t.C:
		return ErrStorageBusy
	}
}

func (g *storGate) release() { <-g.sem }

// call runs fn in a slot and waits for it at most callLimit.
func (g *storGate) call(fn func()) error {
	if err := g.acquire(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	t := time.NewTimer(callLimit)
	defer t.Stop()
	select {
	case <-done:
		g.release()
		return nil
	case <-t.C:
		g.stuck.Add(1)
		go func() {
			<-done
			g.stuck.Add(-1)
			g.release()
		}()
		return ErrStorageBusy
	}
}

// hold runs fn in a slot with no time limit: for a call that may be slow when nothing is wrong.
func (g *storGate) hold(fn func()) error {
	if err := g.acquire(); err != nil {
		return err
	}
	defer g.release()
	fn()
	return nil
}

// The methods below shadow the ones of the embedded libvirt connection and add the gate.

func (h *Host) ConnectListAllStoragePools(need int32, flags libvirt.ConnectListAllStoragePoolsFlags) (pools []libvirt.StoragePool, n uint32, err error) {
	if gerr := h.gate.call(func() { pools, n, err = h.Libvirt.ConnectListAllStoragePools(need, flags) }); gerr != nil {
		return nil, 0, gerr
	}
	return
}

// StoragePoolRefresh scans the pool's directory, which takes as long as the directory is big; it has no time
// limit, only a slot.
func (h *Host) StoragePoolRefresh(p libvirt.StoragePool, flags uint32) (err error) {
	if gerr := h.gate.hold(func() { err = h.Libvirt.StoragePoolRefresh(p, flags) }); gerr != nil {
		return gerr
	}
	return
}

func (h *Host) StoragePoolGetInfo(p libvirt.StoragePool) (state uint8, capacity, allocation, available uint64, err error) {
	if gerr := h.gate.call(func() { state, capacity, allocation, available, err = h.Libvirt.StoragePoolGetInfo(p) }); gerr != nil {
		return 0, 0, 0, 0, gerr
	}
	return
}

func (h *Host) StoragePoolGetXMLDesc(p libvirt.StoragePool, flags libvirt.StorageXMLFlags) (xml string, err error) {
	if gerr := h.gate.call(func() { xml, err = h.Libvirt.StoragePoolGetXMLDesc(p, flags) }); gerr != nil {
		return "", gerr
	}
	return
}

func (h *Host) StoragePoolIsActive(p libvirt.StoragePool) (active int32, err error) {
	if gerr := h.gate.call(func() { active, err = h.Libvirt.StoragePoolIsActive(p) }); gerr != nil {
		return 0, gerr
	}
	return
}

func (h *Host) StoragePoolListAllVolumes(p libvirt.StoragePool, need int32, flags uint32) (vols []libvirt.StorageVol, n uint32, err error) {
	if gerr := h.gate.call(func() { vols, n, err = h.Libvirt.StoragePoolListAllVolumes(p, need, flags) }); gerr != nil {
		return nil, 0, gerr
	}
	return
}

func (h *Host) StoragePoolLookupByName(name string) (pool libvirt.StoragePool, err error) {
	if gerr := h.gate.call(func() { pool, err = h.Libvirt.StoragePoolLookupByName(name) }); gerr != nil {
		return libvirt.StoragePool{}, gerr
	}
	return
}

func (h *Host) StoragePoolGetAutostart(p libvirt.StoragePool) (auto int32, err error) {
	if gerr := h.gate.call(func() { auto, err = h.Libvirt.StoragePoolGetAutostart(p) }); gerr != nil {
		return 0, gerr
	}
	return
}

func (h *Host) StorageVolLookupByName(p libvirt.StoragePool, name string) (vol libvirt.StorageVol, err error) {
	if gerr := h.gate.call(func() { vol, err = h.Libvirt.StorageVolLookupByName(p, name) }); gerr != nil {
		return libvirt.StorageVol{}, gerr
	}
	return
}

// StorageVolLookupByPath, StorageVolGetInfo and StorageVolGetXMLDesc look at the volume's file on disk.
func (h *Host) StorageVolLookupByPath(file string) (vol libvirt.StorageVol, err error) {
	if gerr := h.gate.call(func() { vol, err = h.Libvirt.StorageVolLookupByPath(file) }); gerr != nil {
		return libvirt.StorageVol{}, gerr
	}
	return
}

func (h *Host) StorageVolGetInfo(v libvirt.StorageVol) (typ int8, capacity, allocation uint64, err error) {
	if gerr := h.gate.call(func() { typ, capacity, allocation, err = h.Libvirt.StorageVolGetInfo(v) }); gerr != nil {
		return 0, 0, 0, gerr
	}
	return
}

func (h *Host) StorageVolGetXMLDesc(v libvirt.StorageVol, flags uint32) (xml string, err error) {
	if gerr := h.gate.call(func() { xml, err = h.Libvirt.StorageVolGetXMLDesc(v, flags) }); gerr != nil {
		return "", gerr
	}
	return
}
