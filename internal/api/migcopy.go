package api

// Node to node connections: what the source node needs in order to reach the
// target, how files are copied, and the pre-flight check for both.
//
// Trust model (also shown in the pre-flight message). Everything between two
// nodes is started on the SOURCE node as root:
//   - libvirtd on the source opens qemu+ssh://<user>@<target>/system itself
//     (it runs as root);
//   - the file copies are run by this controller on the source as root (a
//     login that is not root uses sudo for that, see hostconn.RunRoot).
// So the key that matters is root's on the source: ~root/.ssh/id_* of the
// source must be authorised in ~<user>/.ssh/authorized_keys of the target
// user, and root's known_hosts on the source must hold the target's host key.
// The target user needs access to libvirt (the libvirt group) because
// qemu+ssh talks to the target's libvirt socket as that user, and, to place
// disk images in root-owned pool directories, root or passwordless sudo on the
// target (the same privilege setting as for the controller's own commands).

import (
	"errors"
	"fmt"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// nodeCopyCmd is the command, run as root on the source, that copies file
// srcPath to dstPath on the target. mode is the octal permission string the
// copy gets (empty: 600). A target login with root uses scp; otherwise the
// bytes are piped through ssh into a root shell on the target, since scp's
// remote end could only write where the login user may.
func nodeCopyCmd(dst *hostconn.Host, ep migEndpoint, srcPath, dstPath, mode string) (string, error) {
	if mode == "" {
		mode = "600"
	}
	if dst.Privilege() == hostconn.PrivRoot || dst.Privilege() == "" {
		return "scp -q -p -o BatchMode=yes " + ep.sshPort("-P") + "-- " + shq(srcPath) + " " + shq(ep.User+"@"+scpHost(ep.Host)+":"+dstPath), nil
	}
	remote, err := dst.RootCmd("cat > " + shq(dstPath) + " && chmod " + mode + " " + shq(dstPath))
	if err != nil {
		return "", err
	}
	return "ssh -o BatchMode=yes -o ConnectTimeout=10 -o LogLevel=ERROR " + ep.sshPort("-p") + shq(ep.User+"@"+ep.Host) + " " + shq(remote) + " < " + shq(srcPath), nil
}

// probeScript runs on the target, through the SSH connection from the source.
// It prints what the pre-flight needs: the login's uid, whether it may use
// libvirt, and the libvirt host UUID of the machine it reached.
const probeScript = `echo "uid=$(id -u)"; ` +
	`if virsh -c qemu:///system uri >/dev/null 2>&1; then echo libvirt=ok; else echo libvirt=denied; fi; ` +
	`echo "uuid=$(virsh -c qemu:///system capabilities 2>/dev/null | sed -n 's:.*<uuid>\(.*\)</uuid>.*:\1:p' | head -1)"`

// parseProbe reads probeScript's output.
func parseProbe(out string) (uid, libvirtAccess, uuid string) {
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "uid":
			uid = v
		case "libvirt":
			libvirtAccess = v
		case "uuid":
			uuid = v
		}
	}
	return
}

// reachCmd tests a TCP connection from the node to host:port; the shell is
// bash because /dev/tcp is a bash feature. Exit 0: connected, 1: refused or
// unreachable at once, 124: no answer (typically a firewall dropping it).
func reachCmd(host, port string) string {
	return "timeout 6 bash -c " + shq("exec 3<>/dev/tcp/"+host+"/"+port)
}

const sshTrustHelp = "Live migration and file copies are started on the source node as root (libvirtd and scp both run as root there). " +
	"So root's SSH key on the source (~root/.ssh/id_*.pub) must be in the authorized_keys of %[2]s on the target, and root on the source must have accepted the target's host key once. " +
	"Fix: as root on %[1]s run ssh-keygen -t ed25519 (if root has no key), ssh-copy-id %[3]s, then ssh %[3]s true once to accept the host key."

// checkSSHTrust verifies exactly what a migration or a remote backup needs
// between src and the target node: TCP reachability of the target's SSH port
// and of the migration port range at the address other nodes will use, SSH
// login as root on the source with the target's configured user, libvirt access
// for that user, and that the machine reached is the node we think it is
// (compared by libvirt host UUID, not by name).
func (s *Server) checkSSHTrust(src, dst *hostconn.Host, n store.Node, c *checkList) {
	ep, err := endpointFor(n)
	if err != nil {
		c.bad("SSH between nodes", err.Error())
		return
	}
	port := ep.Port
	if port == "" {
		port = "22"
	}
	if _, err := src.Run(reachCmd(ep.Host, port)); err != nil {
		c.bad("SSH between nodes", fmt.Sprintf("%s cannot open a TCP connection to %s port %s. Nodes reach each other by IP address or name configured in the node's settings (migration address); check that it is right and that no firewall blocks the SSH port.", src.Name, ep.Host, port))
		return
	}
	help := fmt.Sprintf(sshTrustHelp, src.Name, ep.User, ep.String())
	out, err := src.RunRoot("ssh -o BatchMode=yes -o ConnectTimeout=10 -o LogLevel=ERROR " + ep.sshPort("-p") + shq(ep.User+"@"+ep.Host) + " " + shq(probeScript))
	if errors.Is(err, hostconn.ErrNoPrivilege) {
		c.bad("SSH between nodes", nodeCapabilities("none")["migration"].Reason)
		return
	}
	if err != nil {
		c.bad("SSH between nodes", fmt.Sprintf("root on %s cannot log in to %s without a password (%s). %s", src.Name, ep.String(), firstLine(out), help))
		return
	}
	uid, access, uuid := parseProbe(out)
	if access != "ok" {
		c.bad("SSH between nodes", fmt.Sprintf("logging in to %s works, but %s has no access to libvirt there (virsh -c qemu:///system fails). Add the user to the libvirt group on the target (usermod -aG libvirt %s); qemu+ssh connects to the target's libvirt socket as that user.", ep.String(), ep.User, ep.User))
		return
	}
	if want, err := dst.HostUUID(); err == nil && uuid != "" && !strings.EqualFold(want, uuid) {
		c.bad("SSH between nodes", fmt.Sprintf("%s reaches a different libvirt host (UUID %s) when connecting to %s, but the node %s has UUID %s. The migration address %s leads to the wrong machine.", src.Name, uuid, ep.Host, dst.Name, want, ep.Host))
		return
	}
	extra := ""
	if uid != "0" {
		extra = fmt.Sprintf("; %s is not root on the target, so files are placed with %s there", ep.User, dst.Privilege())
		if !dst.CanRoot() {
			c.bad("SSH between nodes", fmt.Sprintf("%s logs in as %s, which is not root and has no sudo on %s, so disk images cannot be placed in the pool directories. %s", src.Name, ep.User, dst.Name, nodeCapabilities("none")["migration"].Reason))
			return
		}
	}
	c.pass("SSH between nodes", fmt.Sprintf("root on %s can log in to %s without a password, the user has libvirt access and the machine is %s (libvirt host UUID matches)%s", src.Name, ep.String(), dst.Name, extra))

	// Migration ports: libvirt picks one of 49152-49215 on the target. Nothing
	// listens there yet, so an immediate refusal means "reachable", silence
	// means a firewall drops it.
	if _, err := src.Run(reachCmd(ep.Host, "49152")); err != nil && strings.Contains(err.Error(), "124") {
		c.warn("Migration ports", fmt.Sprintf("%s does not answer on %s:49152. The migration data connection uses TCP ports 49152-49215 on the target; open them in the target's firewall, or a live migration will hang.", src.Name, ep.Host))
	} else {
		c.pass("Migration ports", fmt.Sprintf("%s can reach %s on the migration port range (tested 49152)", src.Name, ep.Host))
	}
}

// checkNodeTransfer is the "SSH between nodes" check for a pair of which at
// least one node is in helper mode (see peertrust.go and migclient.go).
//   - Live migration is coordinated by the controller: no SSH between the
//     nodes is needed for it.
//   - Files that must be copied travel from node to node with the helper's
//     peer transfer. The trust for that is set up here, idempotently, and
//     tested with a real connection.
//   - A pair of different kinds can migrate when no file needs copying.
func (s *Server) checkNodeTransfer(src, dst *hostconn.Host, n store.Node, plan *migPlan, c *checkList) {
	ep, err := endpointFor(n)
	if err != nil {
		c.bad("SSH between nodes", err.Error())
		return
	}
	copies := len(plan.copyList())
	switch {
	case copies == 0:
		c.pass("SSH between nodes", "no file has to be copied between the nodes, and the controller coordinates the migration itself: no SSH between "+src.Name+" and "+dst.Name+" is needed")
	case !bothHelper(src, dst):
		c.bad("SSH between nodes", fmt.Sprintf("%d file(s) have to be copied, but %s and %s are set up differently (one uses the scoped helper account, the other root or sudo), and files cannot be copied between such nodes. Prepare both nodes the same way, or migrate a VM that needs no file copy", copies, src.Name, dst.Name))
		return
	default:
		port := ep.port()
		if _, err := src.Run(reachCmd(ep.Host, port)); err != nil {
			c.bad("SSH between nodes", fmt.Sprintf("%s cannot open a TCP connection to %s port %s. Nodes reach each other by the IP address or name configured in the node's settings (migration address); check that it is right and that no firewall blocks the SSH port.", src.Name, ep.Host, port))
			return
		}
		if err := s.ensurePeerTrust(src, dst, n, ep); err != nil {
			c.bad("SSH between nodes", err.Error())
			return
		}
		c.pass("SSH between nodes", fmt.Sprintf("%s sends files straight to %s as %s with its own node key (restricted to delivering files into storage pools), host key checked; the file data does not pass through the controller", src.Name, ep.String(), ep.User))
	}

	if _, err := src.Run(reachCmd(ep.Host, "49152")); err != nil && strings.Contains(err.Error(), "124") {
		c.warn("Migration ports", fmt.Sprintf("%s does not answer on %s:49152. The migration data connection uses TCP ports 49152-49215 on the target; open them in the target's firewall, or a live migration will hang.", src.Name, ep.Host))
	} else {
		c.pass("Migration ports", fmt.Sprintf("%s can reach %s on the migration port range (tested 49152)", src.Name, ep.Host))
	}
}
