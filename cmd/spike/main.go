// Command spike proves the core architecture end to end against two libvirt
// hosts reached only over SSH:
//
//  1. libvirt RPC over an SSH-forwarded UNIX socket (pure Go, no agent)
//  2. lifecycle event subscription
//  3. image upload through the libvirt storage stream
//  4. live migration without shared storage (disk mirrored over NBD)
//  5. live external snapshot with memory state
//  6. migrating a VM that has snapshots
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virt-proj/internal/hostconn"
)

const (
	vmName   = "spike-vm"
	poolName = "default"
	poolDir  = "/var/lib/libvirt/images"
	imageURL = "https://download.cirros-cloud.net/0.6.3/cirros-0.6.3-x86_64-disk.img"
)

var eventNames = []string{"defined", "undefined", "started", "suspended", "resumed", "stopped", "shutdown", "pmsuspended", "crashed"}

func main() {
	var (
		nodeA   = flag.String("a", "192.168.150.11", "first node (SSH address)")
		nodeB   = flag.String("b", "192.168.150.12", "second node (SSH address)")
		user    = flag.String("user", "root", "SSH user on the nodes")
		key     = flag.String("key", "lab/keys/controller", "SSH private key for the nodes")
		known   = flag.String("known-hosts", "lab/keys/known_hosts", "known_hosts file (trust on first use)")
		jump    = flag.String("jump", os.Getenv("VSPIKE_JUMP"), "optional jump host as user@addr (password from VSPIKE_JUMP_PASS)")
		image   = flag.String("image", "lab/cirros.img", "local cache path for the test image")
		cleanup = flag.Bool("cleanup-only", false, "remove the test VM from both nodes and exit")
	)
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	var jt *hostconn.Target
	if *jump != "" {
		u, a, ok := strings.Cut(*jump, "@")
		if !ok {
			log.Fatalf("-jump must be user@addr")
		}
		jt = &hostconn.Target{Addr: a, User: u, Password: os.Getenv("VSPIKE_JUMP_PASS")}
	}
	dial := func(addr string) *hostconn.Host {
		h, err := hostconn.Dial(hostconn.Target{Addr: addr, User: *user, KeyFile: *key, Jump: jt}, *known)
		check(err, "connect "+addr)
		name, err := h.ConnectGetHostname()
		check(err, "hostname")
		ver, _ := h.ConnectGetLibVersion()
		h.Name = name
		log.Printf("connected: %s (libvirt %d.%d.%d)", name, ver/1000000, ver/1000%1000, ver%1000)
		return h
	}
	a, b := dial(*nodeA), dial(*nodeB)
	defer a.Close()
	defer b.Close()

	for _, h := range []*hostconn.Host{a, b} {
		check(purge(h), "cleanup on "+h.Name)
	}
	if *cleanup {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, h := range []*hostconn.Host{a, b} {
		watch(ctx, h)
	}

	step("upload test image to " + a.Name + " through the libvirt storage stream")
	check(upload(a, *image), "upload")

	step("define and start " + vmName + " on " + a.Name)
	dom, err := a.DomainDefineXMLFlags(domainXML(), libvirt.DomainDefineValidate)
	check(err, "define")
	check(a.DomainCreate(dom), "start")
	ip := waitIP(a, dom)
	log.Printf("guest has address %s", ip)
	check(waitPing(a, ip), "guest reachable before migration")

	step(fmt.Sprintf("live migrate %s -> %s with disk copy (no shared storage)", a.Name, b.Name))
	check(migrate(a, b, dom, libvirt.MigrateNonSharedDisk), "migration")
	dom = verifyOn(b, a, ip)

	step("live external snapshot with memory on " + b.Name)
	_, err = b.DomainSnapshotCreateXML(dom, snapshotXML("s1"), uint32(libvirt.DomainSnapshotCreateLive))
	check(err, "snapshot")
	names, err := b.DomainSnapshotListNames(dom, 16, 0)
	check(err, "list snapshots")
	log.Printf("snapshots on %s: %v", b.Name, names)
	out, _ := b.Run("qemu-img info --backing-chain -U " + poolDir + "/" + vmName + ".s1.qcow2 | grep -E '^image|backing file:'")
	log.Printf("disk chain:\n%s", out)

	step(fmt.Sprintf("migrate back %s -> %s with the snapshot present", b.Name, a.Name))
	err = migrate(b, a, dom, libvirt.MigrateNonSharedDisk)
	if err == nil {
		log.Fatalf("UNEXPECTED: libvirt migrated a domain with snapshots; the workaround below is unnecessary")
	}
	log.Printf("plain migration refused as expected: %v", err)

	step("workaround: carry the snapshot chain across by hand")
	check(migrateWithSnapshots(b, a, dom), "snapshot-preserving migration")
	dom = verifyOn(a, b, ip)
	names, err = a.DomainSnapshotListNames(dom, 16, 0)
	check(err, "list snapshots on destination")
	log.Printf("snapshots on %s after migration: %v", a.Name, names)

	step("revert to the migrated snapshot on " + a.Name + " (restores disk and memory)")
	snap, err := a.DomainSnapshotLookupByName(dom, "s1", 0)
	check(err, "lookup snapshot")
	check(a.DomainRevertToSnapshot(snap, uint32(libvirt.DomainSnapshotRevertRunning)), "revert")
	check(waitPing(a, ip), "guest reachable after revert")
	log.Printf("verified: reverted to s1 on %s and guest answers ping", a.Name)

	time.Sleep(time.Second) // let trailing events print
	log.Printf("spike finished")
}

func step(s string) { log.Printf("=== %s", s) }

func check(err error, what string) {
	if err != nil {
		log.Fatalf("FAILED %s: %v", what, err)
	}
}

// watch prints every domain lifecycle event the host emits.
func watch(ctx context.Context, h *hostconn.Host) {
	ch, err := h.LifecycleEvents(ctx)
	check(err, "subscribe events on "+h.Name)
	go func() {
		for ev := range ch {
			name := fmt.Sprint(ev.Event)
			if int(ev.Event) < len(eventNames) {
				name = eventNames[ev.Event]
			}
			log.Printf("    [event %s] %s %s (detail %d)", h.Name, ev.Dom.Name, name, ev.Detail)
		}
	}()
}

// purge removes the test VM, its snapshot metadata and its volumes.
func purge(h *hostconn.Host) error {
	if dom, err := h.DomainLookupByName(vmName); err == nil {
		h.DomainDestroy(dom) // error ignored: may already be shut off
		if err := h.DomainUndefineFlags(dom, libvirt.DomainUndefineSnapshotsMetadata); err != nil {
			return err
		}
	}
	pool, err := h.StoragePoolLookupByName(poolName)
	if err != nil {
		return err
	}
	if err := h.StoragePoolRefresh(pool, 0); err != nil {
		return err
	}
	vols, _, err := h.StoragePoolListAllVolumes(pool, 1, 0)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if strings.HasPrefix(v.Name, vmName+".") {
			if err := h.StorageVolDelete(v, 0); err != nil {
				return fmt.Errorf("delete %s: %w", v.Name, err)
			}
		}
	}
	return nil
}

func upload(h *hostconn.Host, cache string) error {
	if _, err := os.Stat(cache); err != nil {
		resp, err := http.Get(imageURL)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download %s: %s", imageURL, resp.Status)
		}
		f, err := os.Create(cache + ".part")
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			return err
		}
		f.Close()
		if err := os.Rename(cache+".part", cache); err != nil {
			return err
		}
	}
	f, err := os.Open(cache)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()

	pool, err := h.StoragePoolLookupByName(poolName)
	if err != nil {
		return err
	}
	vol, err := h.StorageVolCreateXML(pool, fmt.Sprintf(
		`<volume><name>%s.qcow2</name><capacity unit='bytes'>%d</capacity><target><format type='qcow2'/></target></volume>`,
		vmName, st.Size()), 0)
	if err != nil {
		return err
	}
	start := time.Now()
	if err := h.StorageVolUpload(vol, f, 0, 0, 0); err != nil {
		return err
	}
	log.Printf("uploaded %d MiB in %s", st.Size()>>20, time.Since(start).Round(time.Millisecond))
	return h.StoragePoolRefresh(pool, 0)
}

func domainXML() string {
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%[1]s</name>
  <memory unit='MiB'>256</memory>
  <vcpu>1</vcpu>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <features><acpi/><apic/></features>
  <cpu mode='host-model'/>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='%[2]s/%[1]s.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='network'>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
    <console type='pty'/>
    <rng model='virtio'><backend model='random'>/dev/urandom</backend></rng>
  </devices>
</domain>`, vmName, poolDir)
}

func snapshotXML(name string) string {
	return fmt.Sprintf(`<domainsnapshot>
  <name>%[1]s</name>
  <memory snapshot='external' file='%[3]s/%[2]s.%[1]s.mem'/>
  <disks>
    <disk name='vda' snapshot='external'>
      <source file='%[3]s/%[2]s.%[1]s.qcow2'/>
    </disk>
  </disks>
</domainsnapshot>`, name, vmName, poolDir)
}

func waitIP(h *hostconn.Host, dom libvirt.Domain) string {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		ifs, err := h.DomainInterfaceAddresses(dom, uint32(libvirt.DomainInterfaceAddressesSrcLease), 0)
		if err == nil {
			for _, i := range ifs {
				for _, addr := range i.Addrs {
					return addr.Addr
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	log.Fatalf("FAILED: guest never obtained a DHCP lease")
	return ""
}

func waitPing(h *hostconn.Host, ip string) error {
	var out string
	var err error
	for i := 0; i < 20; i++ {
		if out, err = h.Run("ping -c 2 -W 1 " + ip); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("%w: %s", err, out)
}

// migrate runs a peer-to-peer live migration: the source libvirtd connects to
// the destination libvirtd itself, so the controller only issues one call.
func migrate(src, dst *hostconn.Host, dom libvirt.Domain, extra libvirt.DomainMigrateFlags) error {
	flags := libvirt.MigrateLive | libvirt.MigratePeer2peer | libvirt.MigratePersistDest |
		libvirt.MigrateUndefineSource | libvirt.MigrateAutoConverge | extra
	uri := "qemu+ssh://root@" + dst.Name + "/system"

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := src.DomainMigratePerform3Params(dom, libvirt.OptString{uri}, nil, nil, flags)
		done <- err
	}()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				log.Printf("migration completed in %s", time.Since(start).Round(time.Millisecond))
			}
			return err
		case <-tick.C:
			_, params, err := src.DomainGetJobStats(dom, 0)
			if err != nil || len(params) == 0 {
				continue
			}
			p := map[string]any{}
			for _, tp := range params {
				p[tp.Field] = tp.Value.I
			}
			log.Printf("    progress: elapsed=%vms disk=%v/%v mem=%v/%v",
				p["time_elapsed"], p["disk_processed"], p["disk_total"], p["memory_processed"], p["memory_total"])
		}
	}
}

// verifyOn asserts the VM is running on dst, gone from src, and that the
// guest still answers on the address it had before the move.
func verifyOn(dst, src *hostconn.Host, ip string) libvirt.Domain {
	dom, err := dst.DomainLookupByName(vmName)
	check(err, "lookup on destination")
	state, _, err := dst.DomainGetState(dom, 0)
	check(err, "state on destination")
	if libvirt.DomainState(state) != libvirt.DomainRunning {
		log.Fatalf("FAILED: domain state on %s is %d, want running", dst.Name, state)
	}
	if _, err := src.DomainLookupByName(vmName); err == nil {
		log.Fatalf("FAILED: domain still defined on source %s", src.Name)
	}
	check(waitPing(dst, ip), "guest reachable after migration")
	up, _ := dst.Run(`virsh domstats --cpu-total ` + vmName + ` | grep cpu.time`)
	log.Printf("verified: running on %s, absent from %s, guest answers ping at %s (%s)", dst.Name, src.Name, ip, strings.TrimSpace(up))
	return dom
}

// migrateWithSnapshots moves a running VM whose disk has external snapshots.
// libvirt refuses to migrate a domain that has snapshot metadata, so:
//
//  1. save every snapshot's XML and drop the metadata on the source
//  2. copy the read-only parts of the chain (backing images, memory states)
//  3. pre-create the active overlay on the destination on top of the copy
//  4. migrate with NON_SHARED_INC, which mirrors only the active overlay
//  5. redefine the snapshots on the destination from the saved XML
//
// If the migration fails the metadata is put back on the source.
//
// The spike knows its own file names; the real implementation must walk the
// <backingStore> chain and the snapshot XML to find them.
func migrateWithSnapshots(src, dst *hostconn.Host, dom libvirt.Domain) error {
	snaps, _, err := src.DomainListAllSnapshots(dom, 1, uint32(libvirt.DomainSnapshotListTopological))
	if err != nil {
		return err
	}
	current := ""
	if cur, err := src.DomainSnapshotCurrent(dom, 0); err == nil {
		current = cur.Name
	}
	type saved struct{ name, xml string }
	var meta []saved
	for _, s := range snaps {
		x, err := src.DomainSnapshotGetXMLDesc(s, uint32(libvirt.DomainSnapshotXMLSecure))
		if err != nil {
			return err
		}
		meta = append(meta, saved{s.Name, x})
	}
	redefine := func(h *hostconn.Host, d libvirt.Domain) error {
		for _, m := range meta {
			flags := libvirt.DomainSnapshotCreateRedefine
			if m.name == current {
				flags |= libvirt.DomainSnapshotCreateCurrent
			}
			if _, err := h.DomainSnapshotCreateXML(d, m.xml, uint32(flags)); err != nil {
				return fmt.Errorf("redefine snapshot %s on %s: %w", m.name, h.Name, err)
			}
		}
		return nil
	}

	readOnly := []string{vmName + ".qcow2", vmName + ".s1.mem"}
	active := vmName + ".s1.qcow2"

	start := time.Now()
	for _, f := range readOnly {
		p := poolDir + "/" + f
		if out, err := src.Run(fmt.Sprintf("scp -q -p %s root@%s:%s", p, dst.Name, p)); err != nil {
			return fmt.Errorf("copy %s: %w: %s", f, err, out)
		}
	}
	log.Printf("copied %d read-only chain files in %s", len(readOnly), time.Since(start).Round(time.Millisecond))

	srcPool, err := src.StoragePoolLookupByName(poolName)
	if err != nil {
		return err
	}
	if err := src.StoragePoolRefresh(srcPool, 0); err != nil {
		return err
	}
	vol, err := src.StorageVolLookupByName(srcPool, active)
	if err != nil {
		return err
	}
	_, capacity, _, err := src.StorageVolGetInfo(vol)
	if err != nil {
		return err
	}
	dstPool, err := dst.StoragePoolLookupByName(poolName)
	if err != nil {
		return err
	}
	if err := dst.StoragePoolRefresh(dstPool, 0); err != nil {
		return err
	}
	if _, err := dst.StorageVolCreateXML(dstPool, fmt.Sprintf(
		`<volume><name>%s</name><capacity unit='bytes'>%d</capacity><target><format type='qcow2'/></target>`+
			`<backingStore><path>%s/%s.qcow2</path><format type='qcow2'/></backingStore></volume>`,
		active, capacity, poolDir, vmName), 0); err != nil {
		return fmt.Errorf("pre-create overlay: %w", err)
	}

	for i := len(snaps) - 1; i >= 0; i-- { // children first
		if err := src.DomainSnapshotDelete(snaps[i], libvirt.DomainSnapshotDeleteMetadataOnly); err != nil {
			return err
		}
	}
	if err := migrate(src, dst, dom, libvirt.MigrateNonSharedInc); err != nil {
		if rerr := redefine(src, dom); rerr != nil {
			return fmt.Errorf("%w (and restoring snapshot metadata failed: %v)", err, rerr)
		}
		return err
	}
	moved, err := dst.DomainLookupByName(vmName)
	if err != nil {
		return err
	}
	return redefine(dst, moved)
}
