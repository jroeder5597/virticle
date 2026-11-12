package hostconn

// Direct node to node file transfer for nodes in helper mode.
//
// Each node's service account has its own SSH key (created by "virticle node
// prepare", the private half never leaves the node). The controller
//   - reads the sending node's public key (PeerKey),
//   - installs it on the receiving node in the service account's
//     authorized_keys with restrict,command="sudo -n <helper> peer-recv", so
//     that key can deliver one new file and do nothing else (PeerAllow),
//   - records the receiving node's SSH host key on the sending node from the
//     controller's own known_hosts (PeerSetHostKey), because the sending node
//     checks host keys strictly and never trusts one on first use,
//   - then asks the sending node to send a file (PeerSend). The data goes from
//     node to node; the controller only sees progress lines.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func (h *Host) needHelper() error {
	if !h.helperMode() {
		return ErrNotHelper
	}
	return nil
}

// PeerKey returns the public peer key ("ssh-ed25519 AAAA...") of the node's service account.
func (h *Host) PeerKey() (string, error) {
	if err := h.needHelper(); err != nil {
		return "", err
	}
	out, err := h.RunHelper("peer-key")
	if err != nil {
		return "", fmt.Errorf("%s", firstLineOf(out, err))
	}
	if !strings.HasPrefix(out, "ssh-ed25519 ") || strings.ContainsAny(out, "\n\r") {
		return "", fmt.Errorf("unexpected answer from peer-key: %q", out)
	}
	return out, nil
}

// PeerAllow lets the holder of the public key deliver files to this node (and nothing else).
// It is idempotent.
func (h *Host) PeerAllow(pub string) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	out, err := h.RunHelperInput(pub+"\n", "peer-allow")
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// PeerDisallow removes a peer key installed by PeerAllow.
func (h *Host) PeerDisallow(pub string) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	out, err := h.RunHelperInput(pub+"\n", "peer-disallow")
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// PeerSetHostKey records the host keys ("ssh-ed25519 AAAA...") of host:port
// for the peer connections of this node, replacing any earlier ones.
func (h *Host) PeerSetHostKey(host, port string, keys []string) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	out, err := h.RunHelperInput(strings.Join(keys, "\n")+"\n", "peer-hostkey-set", host, port)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// PeerUnsetHostKey forgets host:port.
func (h *Host) PeerUnsetHostKey(host, port string) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	out, err := h.RunHelper("peer-hostkey-unset", host, port)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// PeerList returns the peers and hosts on record ("allow BLOB" and "host NAME TYPE" lines).
func (h *Host) PeerList() ([]string, error) {
	if err := h.needHelper(); err != nil {
		return nil, err
	}
	out, err := h.RunHelper("peer-list")
	if err != nil {
		return nil, fmt.Errorf("%s", firstLineOf(out, err))
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
}

// PeerPing checks, from this node, that user@host:port accepts its peer key,
// that the recorded host key matches and that the helper on the far side
// answers. The error carries ssh's own message.
func (h *Host) PeerPing(host, port, user string) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	out, err := h.RunHelper("peer-ping", host, port, user)
	if err != nil {
		return fmt.Errorf("%s", firstLineOf(out, err))
	}
	return nil
}

// PeerSend sends the file src of this node to user@host:port, where the
// helper there creates dst (which must not exist) with the given mode (three
// octal digits). progress, when set, is called with bytes sent and total.
func (h *Host) PeerSend(ctx context.Context, host, port, user, src, dst, mode string, progress func(done, total uint64)) error {
	if err := h.needHelper(); err != nil {
		return err
	}
	var tail []string
	done := false
	err := h.Stream(ctx, HelperCmd("peer-send", host, port, user, src, dst, mode), func(l string) {
		if d, t, ok := parseProgress(l); ok {
			if progress != nil {
				progress(d, t)
			}
			return
		}
		if strings.HasPrefix(l, "@@done ") {
			done = true
			return
		}
		if l = strings.TrimSpace(l); l != "" {
			tail = append(tail, l)
			if len(tail) > 4 {
				tail = tail[1:]
			}
		}
	})
	if err != nil || !done {
		msg := strings.Join(tail, "; ")
		if msg == "" {
			msg = "no confirmation from the helper"
		}
		if err == nil {
			err = errors.New("incomplete")
		}
		return fmt.Errorf("%s: %w", msg, err)
	}
	return nil
}

func parseProgress(l string) (done, total uint64, ok bool) {
	f := strings.Fields(l)
	if len(f) != 3 || f[0] != "@@progress" {
		return 0, 0, false
	}
	d, e1 := strconv.ParseUint(f[1], 10, 64)
	t, e2 := strconv.ParseUint(f[2], 10, 64)
	return d, t, e1 == nil && e2 == nil
}

// KnownHostKeys returns the host keys ("ssh-ed25519 AAAA...") the known_hosts
// file at path holds for addr (host or host:port; port 22 when absent).
func KnownHostKeys(path, addr string) ([]string, error) {
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, err
	}
	addr = withPort(addr)
	tcp, _ := net.ResolveTCPAddr("tcp", addr)
	if tcp == nil {
		tcp = &net.TCPAddr{}
	}
	var ke *knownhosts.KeyError
	if !errors.As(check(addr, tcp, probeKey{}), &ke) || len(ke.Want) == 0 {
		return nil, fmt.Errorf("no host key on record for %s", addr)
	}
	var keys []string
	for _, k := range ke.Want {
		keys = append(keys, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.Key))))
	}
	return keys, nil
}
