package nodecmd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/syslog"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"virticle/internal/nodeproto"
)

// The start guard and the checks before an unplanned failover.
//
// hook-qemu is what libvirt's QEMU hook (installed by "virticle node prepare") calls before a VM starts. A VM
// that Virticle replicates carries a "replication" record naming its standby. The hook asks the standby, over the
// node-to-node peer key, whether the VM was started there from its copy (peer-recv, "took-over"). When it was,
// the start is refused: the old VM must not run beside the new one. Every other outcome starts the VM: the
// standby cannot be reached, answers badly, or takes too long. A dead standby must never keep a healthy VM down.
//
// peer-ask is the same kind of question asked by the controller, from the standby, towards a source node it
// cannot reach itself (the checks before an unplanned failover). vm-probe looks, from the standby, for the VM's
// addresses on a bridge the standby shares with the VM's node.

func init() {
	Register("hook-qemu", verbHookQemu)
	Register("peer-ask", verbPeerAsk)
	Register("vm-probe", verbVMProbe)
}

const (
	guardAskTotal  = 5 * time.Second // the whole question to the standby
	guardHardLimit = 7 * time.Second // the hook is over by then, whatever happens
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// guardFacts is what the hook needs from the domain XML libvirt hands it.
type guardFacts struct {
	UUID string
	// Repl is set when the VM is a replicated source.
	Repl *guardRepl
	// HasFailedOver and Gen: the VM's own failover record (generation 0: it never took over).
	HasFailedOver bool
	Gen           int
}

type guardRepl struct{ Name, Host, Port, User string }

// parseGuardXML reads the uuid and the application metadata from the start of a domain XML. It stops at the end
// of the metadata, so it reads little of a big definition. Whatever it cannot read counts as absent.
func parseGuardXML(r io.Reader) guardFacts {
	var f guardFacts
	d := xml.NewDecoder(r)
	d.Strict = false
	depth, inMeta := 0, false
	for {
		tok, err := d.Token()
		if err != nil {
			return f
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case depth == 2 && t.Name.Local == "uuid":
				var s string
				if d.DecodeElement(&s, &t) == nil {
					f.UUID = strings.ToLower(strings.TrimSpace(s))
				}
				depth--
			case depth == 2 && t.Name.Local == "metadata":
				inMeta = true
			case depth == 2 && (t.Name.Local == "devices" || t.Name.Local == "memory"):
				return f // the metadata comes before these
			case inMeta && strings.HasSuffix(t.Name.Space, "/ns/1"):
				switch t.Name.Local {
				case "replication":
					rp := &guardRepl{}
					for _, a := range t.Attr {
						switch a.Name.Local {
						case "target-name":
							rp.Name = a.Value
						case "peer-host":
							rp.Host = a.Value
						case "peer-port":
							rp.Port = a.Value
						case "peer-user":
							rp.User = a.Value
						}
					}
					f.Repl = rp
				case "failed-over":
					f.HasFailedOver = true
					for _, a := range t.Attr {
						if a.Name.Local == "gen" {
							f.Gen, _ = strconv.Atoi(a.Value)
						}
					}
				}
			}
		case xml.EndElement:
			if depth == 2 && t.Name.Local == "metadata" {
				return f
			}
			depth--
		}
	}
}

// guardNote writes a line to the journal (and nowhere else: a hook's output is not for the VM's user).
func (c *Ctx) guardNote(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if c.Cfg.Log != nil {
		c.Cfg.Log(msg)
		return
	}
	if w, err := syslog.New(syslog.LOG_WARNING|syslog.LOG_DAEMON, "virticle-guard"); err == nil {
		_ = w.Warning(msg)
		w.Close()
	}
}

// verbHookQemu: hook-qemu ACCOUNT OBJECT OPERATION SUBOPERATION [EXTRA]; the domain XML is on stdin. Exit status
// HookRefuse (with the reason on stderr) means: do not start this VM.
func verbHookQemu(c *Ctx) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.guardNote("the start guard failed (%v); the VM starts", r)
			err = nil
		}
	}()
	if len(c.Args) < 4 {
		return nil
	}
	account, object, op, sub := c.Args[0], c.Args[1], c.Args[2], c.Args[3]
	if op != "prepare" || sub != "begin" {
		return nil
	}
	if object == nodeproto.SelftestVM {
		return guardSelftest(c, account)
	}
	f := parseGuardXML(io.LimitReader(c.Stdin, 4<<20))
	if f.Repl == nil {
		return nil // not a replicated VM: nothing to ask
	}
	if f.Repl.Host == "" || !uuidRE.MatchString(f.UUID) {
		c.guardNote("%s: no standby address on record yet; the VM starts without asking", object)
		return nil
	}
	type answer struct {
		took bool
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- answer{err: fmt.Errorf("%v", r)}
			}
		}()
		took, err := guardAsk(c, account, f)
		ch <- answer{took, err}
	}()
	var a answer
	select {
	case a = <-ch:
	case <-time.After(guardHardLimit):
		a.err = errors.New("no answer in time")
	}
	name := f.Repl.Name
	if name == "" {
		name = f.Repl.Host
	}
	if a.err != nil {
		c.guardNote("%s: could not ask the standby %s (%v); the VM starts", object, name, a.err)
		return nil
	}
	if !a.took {
		return nil
	}
	c.guardNote("%s: refused to start; it was started on %s from its copy", object, name)
	fmt.Fprintf(c.Stderr, "this VM was started on %s from its copy; it is kept off here. Use Replicate back in Virticle.\n", name)
	return ExitCode(nodeproto.HookRefuse)
}

// guardAsk asks the standby whether it took the VM over.
func guardAsk(c *Ctx, account string, f guardFacts) (bool, error) {
	a, err := c.hookAccount(account)
	if err != nil {
		return false, err
	}
	port := f.Repl.Port
	if port == "" {
		port = "22"
	}
	user := f.Repl.User
	if user == "" {
		user = "root"
	}
	if err := peerArgs(f.Repl.Host, port, user); err != nil {
		return false, err
	}
	out, err := c.peerAsk(a, f.Repl.Host, port, user, fmt.Sprintf("took-over %s %d", f.UUID, f.Gen), guardAskTotal, 3)
	if err != nil {
		return false, err
	}
	switch firstWord(out) {
	case "@@took-over":
		return true, nil
	case "@@not-took-over":
		return false, nil
	}
	return false, fmt.Errorf("unexpected answer %q", firstLine([]byte(out)))
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// hookAccount finds the peer account the hook was written for (the hook runs as root, without sudo).
func (c *Ctx) hookAccount(name string) (*Account, error) {
	if c.Cfg.Account != nil {
		return c.Cfg.Account, nil
	}
	if !peerUserRE.MatchString(name) {
		return nil, errors.New("unsupported account")
	}
	a, err := c.lookupAccount(name)
	if err != nil {
		return nil, err
	}
	if !PlainPath(a.Home) {
		return nil, errors.New("unsupported home directory")
	}
	return a, nil
}

// peerAsk sends one header line to the peer-recv of user@host:port and returns its answer. total bounds the whole
// call, connect the connection attempt (seconds).
func (c *Ctx) peerAsk(a *Account, host, port, user, header string, total time.Duration, connect int) (string, error) {
	ctx, cancel := context.WithTimeout(c.Context, total)
	defer cancel()
	d := *c
	d.Context = ctx
	out, err := d.peerSSH(a, host, port, user, connect, strings.NewReader("virticle-peer 1 "+header+"\n"))
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return "", err
		}
		msg := strings.TrimSpace(firstLinesJoined(out, 2))
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// guardSelftest answers prepare's probe VM: it says what the hook can do in this environment (the answer is the
// error text libvirt shows when the hook refuses the start). "denied" is what SELinux or AppArmor did.
func guardSelftest(c *Ctx, account string) error {
	word := func(err error) string {
		switch {
		case err == nil:
			return "ok"
		case errors.Is(err, os.ErrNotExist):
			return "missing"
		case errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM):
			return "denied"
		}
		return "failed"
	}
	ssh := "missing"
	if cmd, err := c.Command("ssh", "-V"); err == nil {
		ssh = word(cmd.Run())
	}
	key := "failed"
	if a, err := c.hookAccount(account); err == nil {
		fd, err := os.Open(a.Home + "/.ssh/" + peerKeyName)
		if err == nil {
			fd.Close()
		}
		key = word(err)
	}
	nt := "ok"
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:22", time.Second); err == nil {
		conn.Close()
	} else if word(err) == "denied" { // refused or timed out is an answer from the network, so the call was allowed
		nt = "denied"
	}
	fmt.Fprintf(c.Stderr, "%s exec=ok ssh=%s key=%s net=%s\n", nodeproto.SelftestMark, ssh, key, nt)
	return ExitCode(nodeproto.HookRefuse)
}

// --- the answers a standby or a source node gives (called by peer-recv) ---

const virshURI = "qemu:///system"

// virshOut runs virsh with a time limit; on failure the output is returned with the error text.
func (c *Ctx) virshOut(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(c.Context, 4*time.Second)
	defer cancel()
	d := *c
	d.Context = ctx
	cmd, err := d.Command("virsh", append([]string{"-q", "-c", virshURI}, args...)...)
	if err != nil {
		return nil, err
	}
	var eb bytes.Buffer
	cmd.Stderr = &eb
	out, err := cmd.Output()
	if err != nil {
		return append(out, eb.Bytes()...), err
	}
	return out, nil
}

// peerTookOver: header "took-over UUID GEN". The standby answers from its own libvirt metadata.
func (c *Ctx) peerTookOver(uuid string, askerGen int) error {
	out, err := c.virshOut("dumpxml", "--inactive", uuid)
	if err != nil {
		if strings.Contains(string(out), "failed to get domain") {
			fmt.Fprintln(c.Stdout, "@@not-took-over")
			return nil
		}
		return Fail("cannot read the VM: %s", firstLine(out))
	}
	f := parseGuardXML(bytes.NewReader(out))
	if nodeproto.TookOver(f.HasFailedOver, f.Gen, askerGen) {
		fmt.Fprintf(c.Stdout, "@@took-over %d\n", f.Gen)
	} else {
		fmt.Fprintln(c.Stdout, "@@not-took-over")
	}
	return nil
}

// peerVMState: header "vm-state UUID". The state of the VM on this node.
func (c *Ctx) peerVMState(uuid string) error {
	out, err := c.virshOut("domstate", uuid)
	if err != nil {
		if strings.Contains(string(out), "failed to get domain") {
			fmt.Fprintln(c.Stdout, "@@vm absent")
			return nil
		}
		return Fail("cannot read the VM: %s", firstLine(out))
	}
	st := strings.ReplaceAll(strings.TrimSpace(string(out)), " ", "")
	if st == "" || strings.Trim(st, "abcdefghijklmnopqrstuvwxyz-") != "" {
		st = "unknown"
	}
	fmt.Fprintf(c.Stdout, "@@vm %s\n", st)
	return nil
}

// --- peer-ask, run by the controller ---

// verbPeerAsk: peer-ask HOST PORT USER vm-state UUID | took-over UUID GEN. Prints the peer's answer line.
func verbPeerAsk(c *Ctx) error {
	if err := c.Need(5, 6, "peer-ask HOST PORT USER vm-state UUID | took-over UUID GEN"); err != nil {
		return err
	}
	host, port, user, kind, uuid := c.Args[0], c.Args[1], c.Args[2], c.Args[3], c.Args[4]
	if err := peerArgs(host, port, user); err != nil {
		return err
	}
	if !uuidRE.MatchString(uuid) {
		return Die("unsupported VM identifier")
	}
	header := kind + " " + uuid
	switch {
	case kind == "vm-state" && len(c.Args) == 5:
	case kind == "took-over" && len(c.Args) == 6 && numOK(c.Args[5]):
		header += " " + c.Args[5]
	default:
		return Die("unsupported question")
	}
	a, err := c.account()
	if err != nil {
		return err
	}
	out, err := c.peerAsk(a, host, port, user, header, 6*time.Second, 4)
	if err != nil {
		return Fail("%s", err.Error())
	}
	if !strings.HasPrefix(out, "@@") {
		return Fail("unexpected answer: %s", firstLine([]byte(out)))
	}
	fmt.Fprintln(c.Stdout, firstLine([]byte(out)))
	return nil
}

// --- vm-probe, run on the standby ---

var (
	probeBridgeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)
	arpReplyRE    = regexp.MustCompile(`(?i)reply from ([0-9.]+) \[([0-9a-f:]{17})\]`)
	lladdrRE      = regexp.MustCompile(`(?i)lladdr ([0-9a-f:]{17})`)
)

const probeTotal = 2 * time.Second

// verbVMProbe: vm-probe BRIDGE IP... Looks for each address on the bridge with an ordinary ARP request. (Not with
// duplicate address detection: a probe for an address a guest uses made a guest drop that address in the lab.)
// Prints "@@nobridge" when this node has no such bridge, "@@noaddr" when the bridge has no IPv4 address to ask
// from, else "@@tool NAME" and per address "@@reply IP MAC" or "@@silent IP".
func verbVMProbe(c *Ctx) error {
	if err := c.Need(2, 17, "vm-probe BRIDGE IP..."); err != nil {
		return err
	}
	br := c.Args[0]
	if !probeBridgeRE.MatchString(br) {
		return Die("unsupported bridge name")
	}
	var ips []string
	for _, s := range c.Args[1:] {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			return Die("only IPv4 addresses are probed")
		}
		ips = append(ips, ip.String())
	}
	if _, err := os.Stat(c.Cfg.sys() + "/class/net/" + br); err != nil {
		fmt.Fprintln(c.Stdout, "@@nobridge")
		return nil
	}
	if c.HasTool("ip") {
		// An ARP request needs an address of this node on the bridge as its source.
		if out, err := c.Output("ip", "-4", "-o", "addr", "show", "dev", br); err == nil && strings.TrimSpace(string(out)) == "" {
			fmt.Fprintln(c.Stdout, "@@noaddr")
			return nil
		}
	}
	tool := "none"
	switch {
	case c.HasTool("arping"):
		tool = "arping"
	case c.HasTool("ping") && c.HasTool("ip"):
		tool = "ip-neigh"
	}
	fmt.Fprintln(c.Stdout, "@@tool "+tool)
	if tool == "none" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Context, probeTotal+time.Second)
	defer cancel()
	d := *c
	d.Context = ctx
	res := make([]string, len(ips))
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mac := d.probeOne(tool, br, ip); mac != "" {
				res[i] = "@@reply " + ip + " " + mac
			} else {
				res[i] = "@@silent " + ip
			}
		}()
	}
	wg.Wait()
	sort.Strings(res)
	for _, l := range res {
		fmt.Fprintln(c.Stdout, l)
	}
	return nil
}

// probeOne returns the hardware address that answered for ip on the bridge, "" when nothing did.
func (c *Ctx) probeOne(tool, br, ip string) string {
	if tool == "arping" {
		cmd, err := c.Command("arping", "-I", br, "-c", "2", "-w", "2", ip)
		if err != nil {
			return ""
		}
		out, _ := cmd.CombinedOutput()
		if m := arpReplyRE.FindStringSubmatch(string(out)); m != nil && m[1] == ip {
			return strings.ToLower(m[2])
		}
		return ""
	}
	if cmd, err := c.Command("ping", "-c", "1", "-W", "1", "-I", br, ip); err == nil {
		_ = cmd.Run()
	}
	cmd, err := c.Command("ip", "neigh", "show", ip, "dev", br)
	if err != nil {
		return ""
	}
	out, _ := cmd.Output()
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "FAILED") || strings.Contains(l, "INCOMPLETE") {
			continue
		}
		if m := lladdrRE.FindStringSubmatch(l); m != nil {
			return strings.ToLower(m[1])
		}
	}
	return ""
}
