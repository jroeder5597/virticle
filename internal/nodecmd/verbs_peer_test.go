package nodecmd

import (
	"os"
	"strings"
	"testing"
)

const peerKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe virticle-peer@n1"
const peerBlob = "AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe"
const ctrlLine = `restrict,pty,port-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIControllerControllerControllerContro virticle-controller` + "\n"

// asRoot makes the node's peer account root (the program was started directly
// by root): the home is the same fake directory.
func (n *tnode) asRoot() {
	n.cfg.Account = &Account{Name: "root", Home: n.abs("home/svc"), UID: 0, GID: 0}
}

func TestPeerKey(t *testing.T) {
	for _, mode := range []string{"service account", "root"} {
		n := newNode(t)
		if mode == "root" {
			n.asRoot()
		}
		n.denied(mode+": no key yet", "", "peer-key")
		n.put("home/svc/.ssh/virticle_peer_ed25519.pub", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwn virticle-peer@n2\n")
		if got := n.ok("", "peer-key"); got != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwn" {
			t.Errorf("%s: peer-key: %q", mode, got)
		}
		os.Remove(n.abs("home/svc/.ssh/virticle_peer_ed25519.pub"))
		n.put("home/svc/.ssh/virticle_peer_ed25519.pub", "ssh-rsa AAAA x\n")
		n.denied(mode+": not an ed25519 key", "", "peer-key")
		// a symbolic link in place of the key file is not followed
		os.Remove(n.abs("home/svc/.ssh/virticle_peer_ed25519.pub"))
		n.put("outside/key.pub", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwnOwn x\n")
		os.Symlink(n.abs("outside/key.pub"), n.abs("home/svc/.ssh/virticle_peer_ed25519.pub"))
		n.denied(mode+": key file is a link", "", "peer-key")
	}
}

func TestPeerAllowDisallow(t *testing.T) {
	cases := []struct {
		mode string
		root bool
		line string
	}{
		{"service account", false, `restrict,command="sudo -n /usr/local/libexec/virticle-node peer-recv" ssh-ed25519 ` + peerBlob + ` virticle-peer`},
		{"root", true, `restrict,command="/usr/local/libexec/virticle-node peer-recv" ssh-ed25519 ` + peerBlob + ` virticle-peer`},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			n := newNode(t)
			if tc.root {
				n.asRoot()
			}
			ak := n.put("home/svc/.ssh/authorized_keys", ctrlLine)
			n.ok(peerKey+"\n", "peer-allow")
			n.ok(peerKey+"\n", "peer-allow") // idempotent
			b, _ := os.ReadFile(ak)
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != 2 || lines[0]+"\n" != ctrlLine {
				t.Fatalf("authorized_keys:\n%s", b)
			}
			if lines[1] != tc.line {
				t.Errorf("peer line %q\nwant      %q", lines[1], tc.line)
			}
			if got := n.ok("", "peer-list"); !strings.Contains(got, "allow "+peerBlob) {
				t.Errorf("peer-list: %q", got)
			}
			// The controller's own key cannot be removed through peer-disallow.
			n.ok("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIControllerControllerControllerContro x\n", "peer-disallow")
			if b, _ = os.ReadFile(ak); !strings.Contains(string(b), "virticle-controller") {
				t.Error("peer-disallow removed the controller key")
			}
			n.ok(peerKey+"\n", "peer-disallow")
			if b, _ = os.ReadFile(ak); string(b) != ctrlLine {
				t.Errorf("after disallow:\n%s", b)
			}
			for _, bad := range []string{
				"", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ", "ssh-ed25519 short", "ssh-ed25519 AAAA;id;AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPe\n",
				"command=\"id\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPeerPe\n",
				"ssh-ed25519 " + strings.Repeat("A", 200) + "\n",
			} {
				n.denied("peer-allow "+bad, bad, "peer-allow")
				n.denied("peer-disallow "+bad, bad, "peer-disallow")
			}
			if b, _ = os.ReadFile(ak); string(b) != ctrlLine {
				t.Errorf("a refused key changed the file:\n%s", b)
			}
			// A key added without authorized_keys existing.
			os.Remove(ak)
			n.ok(peerKey+"\n", "peer-allow")
			if b, _ = os.ReadFile(ak); strings.TrimSpace(string(b)) != tc.line {
				t.Errorf("new file:\n%s", b)
			}
			if len(n.glob("home/svc/.ssh/.authorized_keys.*")) != 0 {
				t.Error("a temporary file was left")
			}
			// A symbolic link in place of authorized_keys is not followed.
			_ = os.Remove(ak)
			if err := os.Symlink(n.abs("outside/x"), ak); err != nil {
				t.Fatal(err)
			}
			n.run(peerKey+"\n", "peer-allow")
			if n.exists("outside/x") {
				t.Error("peer-allow wrote through a symbolic link")
			}
			// .ssh missing or a link
			os.Remove(ak)
			os.RemoveAll(n.abs("home/svc/.ssh"))
			n.denied("no .ssh", peerKey+"\n", "peer-allow")
			os.Symlink(n.abs("outside"), n.abs("home/svc/.ssh"))
			n.denied(".ssh is a link", peerKey+"\n", "peer-allow")
			if n.exists("outside/authorized_keys") {
				t.Error("followed a linked .ssh")
			}
		})
	}
}

func TestSvcVerbsRefuseRoot(t *testing.T) {
	// Through sudo the service account could otherwise make root edit another
	// account's files. In a test that runs as root the guard is switched off
	// by Unprivileged, so check the logic with it on.
	if os.Geteuid() != 0 {
		t.Skip("needs to run as root")
	}
	n := newNode(t)
	n.cfg.Unprivileged = false
	for _, args := range [][]string{
		{"svc-allow", n.abs("home/svc"), "b", "", PeerMark},
		{"svc-hosts", n.abs("home/svc"), "h"},
		{"svc-list", n.abs("home/svc"), PeerMark},
		{"svc-peerkey", n.abs("home/svc")},
	} {
		n.refused("root runs "+args[0], "", args...)
	}
}

func TestDeriveAccount(t *testing.T) {
	lookup := func(name string) (*Account, error) {
		switch name {
		case "root":
			return &Account{Name: "root", Home: "/root"}, nil
		case "virticle":
			return &Account{Name: "virticle", Home: "/var/lib/virticle", UID: 998, GID: 998}, nil
		case "zero":
			return &Account{Name: "zero", Home: "/home/zero", UID: 0}, nil
		case "badhome":
			return &Account{Name: "badhome", Home: "relative", UID: 5}, nil
		}
		return nil, os.ErrNotExist
	}
	if a, err := DeriveAccount("virticle", 0, lookup); err != nil || a.Name != "virticle" || a.UID != 998 {
		t.Errorf("sudo caller: %+v %v", a, err)
	}
	for _, su := range []string{"", "root"} {
		if a, err := DeriveAccount(su, 0, lookup); err != nil || a.Name != "root" || a.Home != "/root" {
			t.Errorf("direct root (%q): %+v %v", su, a, err)
		}
		if _, err := DeriveAccount(su, 1000, lookup); err == nil {
			t.Errorf("a non-root user without sudo (%q) was accepted", su)
		}
	}
	for _, su := range []string{"zero", "unknown", "bad name", "UPPER", "a;b", "badhome", "../x"} {
		if _, err := DeriveAccount(su, 0, lookup); err == nil {
			t.Errorf("sudo user %q accepted", su)
		}
	}
}

func TestPeerLine(t *testing.T) {
	if got := PeerLine("B", false); got != `restrict,command="sudo -n /usr/local/libexec/virticle-node peer-recv" ssh-ed25519 B virticle-peer` {
		t.Errorf("%s", got)
	}
	if got := PeerLine("B", true); got != `restrict,command="/usr/local/libexec/virticle-node peer-recv" ssh-ed25519 B virticle-peer` {
		t.Errorf("%s", got)
	}
	for _, root := range []bool{false, true} {
		if !strings.Contains(PeerLine("B", root), PeerMark) {
			t.Error("a peer line must carry the mark")
		}
	}
}

const hostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostHostHostHostHostHostHostHostHostHostHostHostH"

func TestPeerHostKeys(t *testing.T) {
	for _, mode := range []string{"service account", "root"} {
		n := newNode(t)
		if mode == "root" {
			n.asRoot()
		}
		kh := n.abs("home/svc/.ssh/virticle_peer_known_hosts")
		n.ok(hostKey+"\n", "peer-hostkey-set", "192.168.150.12", "22")
		n.ok(hostKey+"\n", "peer-hostkey-set", "192.168.150.12", "22")
		n.ok(hostKey+" comment\n", "peer-hostkey-set", "node-2.example", "2222")
		b, _ := os.ReadFile(kh)
		if string(b) != "192.168.150.12 "+hostKey+"\n[node-2.example]:2222 "+hostKey+"\n" {
			t.Errorf("%s: known hosts:\n%s", mode, b)
		}
		// Replacing the key of a host drops the old one.
		other := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOtherOtherOtherOtherOtherOtherOtherOtherOtherOth"
		n.ok(other+"\n", "peer-hostkey-set", "192.168.150.12", "22")
		b, _ = os.ReadFile(kh)
		if strings.Count(string(b), "192.168.150.12 ") != 1 || !strings.Contains(string(b), other) {
			t.Errorf("replace:\n%s", b)
		}
		if got := n.ok("", "peer-list"); !strings.Contains(got, "host 192.168.150.12 ssh-ed25519") || !strings.Contains(got, "host [node-2.example]:2222 ssh-ed25519") {
			t.Errorf("peer-list: %q", got)
		}
		n.ok("", "peer-hostkey-unset", "192.168.150.12", "22")
		if b, _ = os.ReadFile(kh); strings.Contains(string(b), "192.168.150.12") {
			t.Errorf("unset:\n%s", b)
		}
		for _, c := range [][]string{
			{"", "22"}, {"-oProxyCommand=x", "22"}, {"host;id", "22"}, {"host name", "22"}, {"host", "0"}, {"host", "70000"}, {"host", "22x"}, {"host", ""},
		} {
			n.denied("hostkey-set "+strings.Join(c, ","), hostKey+"\n", "peer-hostkey-set", c[0], c[1])
			n.denied("hostkey-unset "+strings.Join(c, ","), "", "peer-hostkey-unset", c[0], c[1])
		}
		for _, bad := range []string{"", "ssh-ed25519 a;id\n", "bogus AAAAAAAA\n", "ssh-ed25519 " + strings.Repeat("A", 2000) + "\n", hostKey + "\nnot a key\n", strings.Repeat(hostKey+"\n", 5)} {
			n.denied("hostkey-set body "+bad[:min(len(bad), 20)], bad, "peer-hostkey-set", "h", "22")
		}
		// A symbolic link in place of the file is not followed.
		os.Remove(kh)
		os.Symlink(n.abs("outside/hosts"), kh)
		n.denied("known_hosts is a link", hostKey+"\n", "peer-hostkey-set", "h", "22")
		if n.exists("outside/hosts") {
			t.Error("wrote through a link")
		}
	}
}

func TestPeerRecv(t *testing.T) {
	n := newNode(t)
	hdr := func(mode, size, path string) string {
		return "virticle-peer 1 recv " + mode + " " + size + " " + path + "\n"
	}
	newImg, short := n.pool("new.img"), n.pool("short")
	if got := n.ok("virticle-peer 1 ping\n", "peer-recv"); got != "@@pong "+Version {
		t.Errorf("ping: %q", got)
	}
	if got := n.ok(hdr("600", "5", newImg)+"hello", "peer-recv"); got != "@@received 5" {
		t.Errorf("recv: %q", got)
	}
	if b, _ := os.ReadFile(newImg); string(b) != "hello" {
		t.Errorf("content %q", b)
	}
	if fi, _ := os.Stat(newImg); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	n.denied("overwrite", hdr("600", "3", newImg)+"bad", "peer-recv")
	if b, _ := os.ReadFile(newImg); string(b) != "hello" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
	n.denied("short data", hdr("600", "10", short)+"abc", "peer-recv")
	if n.exists("pools/default/short") || len(n.glob("pools/default/*part*")) != 0 {
		t.Error("an incomplete file was left behind")
	}
	n.ok(hdr("644", "0", n.pool("empty")), "peer-recv")
	n.ok(hdr("600", "2", n.abs("nvram/x_VARS.fd"))+"ab", "peer-recv")
	// the data after the header may be large
	big := strings.Repeat("0123456789abcdef", 100000)
	n.ok(hdr("640", "1600000", n.pool("big"))+big, "peer-recv")
	if b, _ := os.ReadFile(n.pool("big")); string(b) != big {
		t.Error("large content differs")
	}
	if fi, _ := os.Stat(n.pool("big")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	pool := n.abs("pools/default")
	for _, h := range []string{
		"", "\n", "hello\n", "virticle-peer 2 ping\n", "virticle-peer 1\n", "virticle-peer 1 exec id\n", "virticle-peer 1 ping extra\n",
		hdr("4755", "1", pool+"/x"), hdr("6000", "1", pool+"/x"), hdr("7", "1", pool+"/x"), hdr("600", "-1", pool+"/x"),
		hdr("600", "1x", pool+"/x"), hdr("600", "1", "/etc/evil"), hdr("600", "1", n.abs("pools/other/evil")), hdr("600", "1", pool+"/../../etc/evil"),
		hdr("600", "1", pool+"/x;id"), hdr("600", "1", pool+"/$(id)"), hdr("600", "1", "relative"),
		hdr("600", "1", pool+"/a b"), hdr("600", "1", pool), "virticle-peer 1 recv 600 1\n",
		hdr("600", "99999999999999999999", pool+"/x"), "virticle-peer 1 recv 600 1 " + pool + "/x extra\n",
		strings.Repeat("a", 500) + "\n",
	} {
		n.denied("header "+h, h+"x", "peer-recv")
	}
	if got := n.glob("pools/default/*"); len(got) != 3 {
		t.Errorf("a refused header created something: %v", got)
	}
	// a symlink planted at the destination is not followed
	if err := os.Symlink(n.abs("outside/target"), n.pool("planted")); err != nil {
		t.Fatal(err)
	}
	n.denied("planted link", hdr("600", "1", n.pool("planted"))+"x", "peer-recv")
	if n.exists("outside/target") {
		t.Error("peer-recv wrote through a planted link")
	}
	n.refused("arguments", "virticle-peer 1 ping\n", "peer-recv", "extra")
}

// fakeSSH installs a program as the node's ssh that records its arguments and
// hands the stream to peer-recv of a second fake node (this test binary).
func fakeSSH(t *testing.T, n, recv *tnode) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ssh := n.abs("fake-ssh")
	script := "#!/bin/sh\necho \"$@\" > " + recv.abs("ssh-args") + "\nNODECMD_TEST_RECV_ROOT=" + recv.root + " exec " + exe + "\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	n.cfg.Tools["ssh"] = ssh
}

func TestPeerSendReachesPeerRecv(t *testing.T) {
	for _, mode := range []string{"service account", "root"} {
		n := newNode(t)
		if mode == "root" {
			n.asRoot()
		}
		r := newNode(t)
		fakeSSH(t, n, r)
		data := strings.Repeat("0123456789abcdef", 70000) // 1.1 MB
		n.put("pools/default/disk.img", data)
		src := n.pool("disk.img")
		dst := r.pool("disk.img")
		out := n.ok("", "peer-send", "10.1.2.3", "22", "virticle", src, dst, "600")
		if !strings.HasSuffix(out, "@@done "+itoaT(len(data))) {
			t.Errorf("peer-send output: %q", out)
		}
		if b, _ := os.ReadFile(dst); string(b) != data {
			t.Errorf("received %d bytes, want %d", len(b), len(data))
		}
		args, _ := os.ReadFile(r.abs("ssh-args"))
		for _, want := range []string{"-o StrictHostKeyChecking=yes", "-o BatchMode=yes", "-o IdentitiesOnly=yes", "-F /dev/null", "-p 22", "-- virticle@10.1.2.3 peer-recv",
			"UserKnownHostsFile=" + n.abs("home/svc/.ssh/virticle_peer_known_hosts"), "-i " + n.abs("home/svc/.ssh/virticle_peer_ed25519")} {
			if !strings.Contains(string(args), want) {
				t.Errorf("ssh was run without %q: %s", want, args)
			}
		}
		// Sending again does not overwrite: the receiver refuses.
		n.denied("send over an existing file", "", "peer-send", "10.1.2.3", "22", "virticle", src, dst, "600")
		// The failure is a failure of the operation (1), not a refusal of the arguments.
		if _, code := n.run("", "peer-send", "10.1.2.3", "22", "virticle", src, dst, "600"); code != 1 {
			t.Errorf("exit %d for a refused transfer, want 1", code)
		}
		// Bad arguments.
		for _, a := range [][]string{
			{"-oProxyCommand=x", "22", "virticle"}, {"host", "22", "-oX"}, {"host", "x", "virticle"}, {"host", "22", "Vir ticle"},
			{"host;id", "22", "virticle"}, {"host", "22", "root;id"}, {"", "22", "virticle"},
		} {
			n.denied("send args "+strings.Join(a, ","), "", append(append([]string{"peer-send"}, a...), src, r.pool("x"), "600")...)
			n.denied("ping args "+strings.Join(a, ","), "", append([]string{"peer-ping"}, a...)...)
		}
		for _, m := range []string{"4755", "7777", "60", "rw", "", "-1"} {
			n.refused("send mode "+m, "", "peer-send", "h", "22", "virticle", src, r.pool("x"), m)
		}
		n.refused("send destination", "", "peer-send", "h", "22", "virticle", src, r.pool("x y"), "600")
		n.denied("send directory", "", "peer-send", "h", "22", "virticle", n.abs("pools/default"), r.pool("x"), "600")
		// ping
		if got := n.ok("", "peer-ping", "10.1.2.3", "22", "virticle"); got != "@@pong "+Version {
			t.Errorf("ping: %q", got)
		}
	}
}

func TestPeerSendReportsSSHFailure(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/f", "data")
	ssh := n.abs("bad-ssh")
	n.put("bad-ssh", "#!/bin/sh\ncat >/dev/null\necho 'Host key verification failed.' >&2\nexit 255\n")
	os.Chmod(ssh, 0o755)
	n.cfg.Tools["ssh"] = ssh
	out, code := n.run("", "peer-send", "h", "22", "virticle", n.pool("f"), n.abs("pools/other/f"), "600")
	if code != 1 || !strings.Contains(out, "sending to virticle@h failed: Host key verification failed.") {
		t.Errorf("exit %d: %s", code, out)
	}
	out, code = n.run("", "peer-ping", "h", "22", "virticle")
	if code != 1 || !strings.Contains(out, "Host key verification failed.") {
		t.Errorf("ping: exit %d: %s", code, out)
	}
	// An answer that is not the confirmation.
	n.put("bad-ssh", "#!/bin/sh\ncat >/dev/null\necho 'something else'\n")
	out, code = n.run("", "peer-send", "h", "22", "virticle", n.pool("f"), n.abs("pools/other/f"), "600")
	if code != 1 || !strings.Contains(out, "did not confirm") {
		t.Errorf("exit %d: %s", code, out)
	}
	out, code = n.run("", "peer-ping", "h", "22", "virticle")
	if code != 1 || !strings.Contains(out, "unexpected answer") {
		t.Errorf("ping: exit %d: %s", code, out)
	}
	// No ssh at all.
	n.cfg.Tools["ssh"] = ""
	if out, code = n.run("", "peer-ping", "h", "22", "virticle"); code != 1 || !strings.Contains(out, "not installed") {
		t.Errorf("no ssh: %d %s", code, out)
	}
}

func itoaT(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

// A key line left by the old shell script would shadow the new line for the
// same peer (sshd takes the first match), so peer-allow drops such lines.
func TestPeerAllowDropsOldScriptLines(t *testing.T) {
	n := newNode(t)
	old := `restrict,command="sudo -n /usr/local/libexec/virticle-helper peer-recv" ssh-ed25519 ` + peerBlob + " virticle-peer\n"
	ak := n.put("home/svc/.ssh/authorized_keys", old+ctrlLine)
	n.ok(peerKey+"\n", "peer-allow")
	b, _ := os.ReadFile(ak)
	if strings.Contains(string(b), "virticle-helper") {
		t.Fatalf("the old line is still there:\n%s", b)
	}
	if !strings.Contains(string(b), "virticle-controller") || strings.Count(string(b), peerBlob) != 1 {
		t.Fatalf("authorized_keys:\n%s", b)
	}
}
