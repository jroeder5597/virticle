package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// agentCmd sends one QEMU guest agent command and returns its "return" value.
func agentCmd(h *hostconn.Host, dom libvirt.Domain, execute string, args any) (json.RawMessage, error) {
	return agentCmdT(h, dom, execute, args, 15)
}

// agentCmdT is agentCmd with its own timeout in seconds, for calls made on
// behalf of a page view that must not hang.
func agentCmdT(h *hostconn.Host, dom libvirt.Domain, execute string, args any, timeoutSec int32) (json.RawMessage, error) {
	req := map[string]any{"execute": execute}
	if args != nil {
		req["arguments"] = args
	}
	b, _ := json.Marshal(req)
	res, err := h.QEMUDomainAgentCommand(dom, string(b), timeoutSec, 0)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, errors.New("the guest agent returned nothing")
	}
	var out struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Desc string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(res[0]), &out); err != nil {
		return nil, fmt.Errorf("unreadable guest agent reply: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("guest agent: %s", out.Error.Desc)
	}
	return out.Return, nil
}

// agentOS returns the guest's OS id as the agent reports it ("debian",
// "mswindows", ...).
func agentOS(h *hostconn.Host, dom libvirt.Domain) (string, error) {
	raw, err := agentCmd(h, dom, "guest-get-osinfo", nil)
	if err != nil {
		return "", err
	}
	var o struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return "", err
	}
	return o.ID, nil
}

type execResult struct {
	Exited   bool
	ExitCode int
	Out, Err string
}

// agentStart launches a program in the guest and returns its pid without
// waiting for it.
func agentStart(h *hostconn.Host, dom libvirt.Domain, path string, args ...string) (int, error) {
	raw, err := agentCmd(h, dom, "guest-exec", map[string]any{"path": path, "arg": args, "capture-output": true})
	if err != nil {
		return 0, err
	}
	var r struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	return r.PID, nil
}

// agentWait polls guest-exec-status until the program has exited.
func agentWait(h *hostconn.Host, dom libvirt.Domain, pid int, timeout time.Duration) (execResult, error) {
	deadline := time.Now().Add(timeout)
	for {
		raw, err := agentCmd(h, dom, "guest-exec-status", map[string]int{"pid": pid})
		if err != nil {
			return execResult{}, err
		}
		var s struct {
			Exited   bool   `json:"exited"`
			ExitCode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
			ErrData  string `json:"err-data"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return execResult{}, err
		}
		if s.Exited {
			o, _ := base64.StdEncoding.DecodeString(s.OutData)
			e, _ := base64.StdEncoding.DecodeString(s.ErrData)
			return execResult{true, s.ExitCode, string(o), string(e)}, nil
		}
		if time.Now().After(deadline) {
			return execResult{}, fmt.Errorf("the command in the guest did not finish within %s", timeout)
		}
		time.Sleep(time.Second)
	}
}

// linuxFirstBootFixups prepare a disk image for booting on new hardware: the
// guest gets a working network and a way to create its SSH host keys even
// when cloud-init does not run. Used by the seal script and, through
// virt-sysprep --run-command, by the libguestfs path.
const linuxFirstBootFixups = `
# cloud-init pins its netplan config to the NIC's MAC address, which a clone
# does not have; without a seed cloud-init stays disabled and the clone would
# have no network. A seeded boot rewrites this file.
if [ -f /etc/netplan/50-cloud-init.yaml ]; then
  printf 'network:\n  version: 2\n  ethernets:\n    all-en:\n      match:\n        name: "en*"\n      dhcp4: true\n      dhcp6: true\n' > /etc/netplan/50-cloud-init.yaml
fi
rm -f /etc/systemd/network/10-cloud-init-*.network
for u in ssh sshd; do
  if [ -e /lib/systemd/system/$u.service ] || [ -e /usr/lib/systemd/system/$u.service ]; then
    mkdir -p /etc/systemd/system/$u.service.d
    printf '[Service]\nExecStartPre=\nExecStartPre=-/usr/bin/ssh-keygen -A\nExecStartPre=/usr/sbin/sshd -t\n' > /etc/systemd/system/$u.service.d/10-regenerate-hostkeys.conf
    break
  fi
done
`

// linuxSealScript removes everything that makes a booted Linux guest unique.
// On the next boot systemd creates a fresh machine-id; cloud-init (when
// installed) re-runs as a first boot; a drop-in also makes ssh generate any
// missing host keys before sshd starts, so images without cloud-init, or
// clones booted without a seed, still come up with new keys.
const linuxSealScript = `
: > /etc/machine-id
rm -f /var/lib/dbus/machine-id
rm -f /etc/ssh/ssh_host_*
command -v cloud-init >/dev/null 2>&1 && cloud-init clean --logs
` + linuxFirstBootFixups + `rm -f /var/lib/dhcp/*.leases /var/lib/dhclient/* /var/lib/NetworkManager/*.lease /var/lib/NetworkManager/*.lease.* /run/systemd/netif/leases/*
rm -f /etc/udev/rules.d/70-persistent-net.rules
journalctl --rotate >/dev/null 2>&1; journalctl --vacuum-time=1s >/dev/null 2>&1
rm -rf /var/log/journal/*/* 2>/dev/null
find /var/log -type f \( -name '*.gz' -o -name '*.xz' -o -name '*.[0-9]' -o -name '*.old' \) -delete 2>/dev/null
find /var/log -type f -exec truncate -s 0 {} + 2>/dev/null
rm -f /root/.bash_history /home/*/.bash_history /root/.ssh/known_hosts
sync
nohup setsid sh -c 'sleep 3; systemctl poweroff || poweroff -f' >/dev/null 2>&1 </dev/null &
exit 0
`

const windowsSysprep = `C:\Windows\System32\Sysprep\sysprep.exe`

var windowsSysprepArgs = []string{"/generalize", "/oobe", "/shutdown", "/quiet"}
