package prepare

// Firewall support, shared by "virticle node prepare" (the migration ports on
// a node) and "virticle setup" (the web ports on the controller's machine).
//
// The rules are only ever additive: allow rules for our own ports, in a
// firewalld service or ufw rules that carry our name. Nothing is dropped,
// SSH rules are never looked at, and removal touches only what carries our
// name. Everything runs as shell on the machine (over SSH for a node, locally
// for the controller), so one script serves both.

import (
	"fmt"
	"strings"
)

// MigrationPorts are the TCP ports libvirt uses for the data of a live
// migration on the target node.
const MigrationPorts = "49152-49215"

// NodeFirewallService names the firewalld service and the ufw rule comment
// that prepare adds on a node.
const NodeFirewallService = "virticle-node"

// FirewallSpec says what to open.
type FirewallSpec struct {
	Service     string   // firewalld service name, and the ufw rule comment
	Description string   // one line for the firewalld service
	Ports       []string // TCP ports: "8443" or a range "49152-49215". Empty: remove what Service added
	Actor       string   // "Prepare" or "Setup", for the sentences
	Purpose     string   // "for migrations between nodes"
}

// NodeFirewall is what prepare opens on a node.
func NodeFirewall() FirewallSpec {
	return FirewallSpec{
		Service:     NodeFirewallService,
		Description: "Virticle: live migration data between nodes",
		Ports:       []string{MigrationPorts},
		Actor:       "Prepare",
		Purpose:     "for migrations between nodes",
	}
}

// PortsText writes ports in plain words: "TCP port 8443", "TCP ports 80 and
// 8443", "TCP ports 49152 to 49215".
func PortsText(ports []string) string {
	var parts []string
	plural := len(ports) > 1
	for _, p := range ports {
		if a, b, ok := strings.Cut(p, "-"); ok {
			p, plural = a+" to "+b, true
		}
		parts = append(parts, p)
	}
	list := strings.Join(parts, ", ")
	if len(parts) > 1 {
		list = strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
	if plural {
		return "TCP ports " + list
	}
	return "TCP port " + list
}

// fwLib is the shell that tells which firewall is in use and whether our
// ports are open. It needs the variables fwsvc and fwports (space separated).
const fwLib = `
fw_kind() {
  if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then echo firewalld
  elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi '^Status: active'; then echo ufw
  else echo none; fi
}
fw_nft() {
  if command -v nft >/dev/null 2>&1 && nft list ruleset 2>/dev/null | grep -q 'hook input'; then echo yes
  elif command -v iptables >/dev/null 2>&1 && iptables -S INPUT 2>/dev/null | grep -qv '^-P INPUT ACCEPT'; then echo yes
  fi
}
fw_ufw_ours() { ufw show added 2>/dev/null | grep -F "comment '$fwsvc'" | sed -n 's/^ufw allow \([^ ]*\) .*/\1/p'; }
fw_ufw_want() { for p in $fwports; do echo "$p" | tr - :; done | sed 's|$|/tcp|'; }
fw_open() {
  [ -n "$fwports" ] || { echo no; return; }
  case "$1" in
  firewalld)
    firewall-cmd --permanent --get-services 2>/dev/null | tr ' ' '\n' | grep -qx "$fwsvc" || { echo no; return; }
    [ "$(firewall-cmd --permanent --service="$fwsvc" --get-ports 2>/dev/null | tr ' ' '\n' | sed 's|/tcp$||' | grep . | sort)" = "$(printf '%s\n' $fwports | sort)" ] || { echo no; return; }
    firewall-cmd --permanent --query-service="$fwsvc" >/dev/null 2>&1 || { echo no; return; }
    firewall-cmd --query-service="$fwsvc" >/dev/null 2>&1 || { echo no; return; }
    echo yes;;
  ufw)
    [ "$(fw_ufw_ours | sort)" = "$(fw_ufw_want | sort)" ] && echo yes || echo no;;
  *) echo no;;
  esac
}
`

// fwProbe prints the firewall facts for the node's migration ports; it is
// part of FactsScript.
const fwProbe = fwLib + `
fwsvc=` + NodeFirewallService + `; fwports="` + MigrationPorts + `"
fwkind=$(fw_kind)
echo "fw=$fwkind"
echo "fw_open=$(fw_open "$fwkind")"
if [ "$fwkind" = none ]; then echo "fw_nft=$(fw_nft)"; fi
`

// fwApply is the shell that makes the firewall hold exactly fwports in the
// service fwsvc (or removes the service when fwports is empty). It prints
// markers, no sentences: @@kind NAME, @@changed or @@unchanged, @@nft,
// @@failed COMMAND and @@detail TEXT when a command failed (it then stops;
// it always exits 0).
const fwApply = `
fw() { d=$1; shift; o=$("$@" 2>&1) || { echo "@@failed $d"; echo "@@detail $(echo "$o" | head -n 1)"; exit 0; }; }
fw_firewalld() {
  ch=0; reload=0
  has=$(firewall-cmd --permanent --get-services 2>/dev/null | tr ' ' '\n' | grep -cx "$fwsvc")
  if [ -z "$fwports" ]; then
    if [ "$has" != 0 ]; then
      for z in $(firewall-cmd --permanent --get-zones 2>/dev/null); do
        if firewall-cmd --permanent --zone="$z" --query-service="$fwsvc" >/dev/null 2>&1; then
          fw "firewall-cmd --permanent --zone=$z --remove-service=$fwsvc" firewall-cmd --permanent --zone="$z" --remove-service="$fwsvc"
        fi
      done
      fw "firewall-cmd --permanent --delete-service=$fwsvc" firewall-cmd --permanent --delete-service="$fwsvc"
      fw "firewall-cmd --reload" firewall-cmd --reload
      ch=1
    fi
  else
    if [ "$has" = 0 ]; then
      fw "firewall-cmd --permanent --new-service=$fwsvc" firewall-cmd --permanent --new-service="$fwsvc"
      fw "firewall-cmd --permanent --service=$fwsvc --set-short=$fwsvc" firewall-cmd --permanent --service="$fwsvc" --set-short="$fwsvc"
      fw "firewall-cmd --permanent --service=$fwsvc --set-description=..." firewall-cmd --permanent --service="$fwsvc" --set-description="$fwdesc"
      ch=1; reload=1
    fi
    have=$(firewall-cmd --permanent --service="$fwsvc" --get-ports 2>/dev/null | tr ' ' '\n' | sed 's|/tcp$||' | grep .)
    for p in $fwports; do
      echo "$have" | grep -qx "$p" || { fw "firewall-cmd --permanent --service=$fwsvc --add-port=$p/tcp" firewall-cmd --permanent --service="$fwsvc" --add-port="$p/tcp"; ch=1; reload=1; }
    done
    for h in $have; do
      case " $fwports " in *" $h "*) ;; *) fw "firewall-cmd --permanent --service=$fwsvc --remove-port=$h/tcp" firewall-cmd --permanent --service="$fwsvc" --remove-port="$h/tcp"; ch=1; reload=1;; esac
    done
    firewall-cmd --permanent --query-service="$fwsvc" >/dev/null 2>&1 || { fw "firewall-cmd --permanent --add-service=$fwsvc" firewall-cmd --permanent --add-service="$fwsvc"; ch=1; reload=1; }
    firewall-cmd --query-service="$fwsvc" >/dev/null 2>&1 || reload=1
    if [ "$reload" = 1 ]; then fw "firewall-cmd --reload" firewall-cmd --reload; ch=1; fi
  fi
  if [ "$ch" = 1 ]; then echo @@changed; else echo @@unchanged; fi
}
fw_ufw() {
  ch=0
  ours=$(fw_ufw_ours); want=$(fw_ufw_want)
  for o in $ours; do
    echo "$want" | grep -qxF "$o" || { fw "ufw delete allow $o" ufw delete allow "$o"; ch=1; }
  done
  for w in $want; do
    echo "$ours" | grep -qxF "$w" || { fw "ufw allow $w comment '$fwsvc'" ufw allow "$w" comment "$fwsvc"; ch=1; }
  done
  if [ "$ch" = 1 ]; then echo @@changed; else echo @@unchanged; fi
}
kind=$(fw_kind)
echo "@@kind $kind"
case "$kind" in
firewalld) fw_firewalld;;
ufw) fw_ufw;;
*) if [ -n "$fwports" ] && [ "$(fw_nft)" = yes ]; then echo @@nft; fi; echo @@unchanged;;
esac
true`

// FirewallScript is the shell that applies s (or removes it when s has no
// ports). Run it as root.
func FirewallScript(s FirewallSpec) string {
	return "fwsvc=" + shq(s.Service) + "\nfwports=" + shq(strings.Join(s.Ports, " ")) + "\nfwdesc=" + shq(s.Description) + "\n" + fwLib + fwApply
}

// FirewallFacts are the parsed firewall lines of the facts script.
type FirewallFacts struct {
	Kind string // firewalld, ufw or none
	Open bool   // our ports are open as we would open them
	Nft  bool   // none of the two, but input rules exist (nftables or iptables)
}

// FirewallCheckScript prints the firewall facts for s without changing
// anything.
func FirewallCheckScript(s FirewallSpec) string {
	return "fwsvc=" + shq(s.Service) + "\nfwports=" + shq(strings.Join(s.Ports, " ")) + "\n" + fwLib + `
fwkind=$(fw_kind)
echo "fw=$fwkind"
echo "fw_open=$(fw_open "$fwkind")"
if [ "$fwkind" = none ]; then echo "fw_nft=$(fw_nft)"; fi
true`
}

// ParseFirewallFacts reads the fw lines from a facts or check script's output.
func ParseFirewallFacts(out string) FirewallFacts {
	var f FirewallFacts
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), "=")
		if !ok {
			continue
		}
		switch k {
		case "fw":
			f.Kind = strings.TrimSpace(v)
		case "fw_open":
			f.Open = strings.TrimSpace(v) == "yes"
		case "fw_nft":
			f.Nft = strings.TrimSpace(v) == "yes"
		}
	}
	return f
}

func (s FirewallSpec) where(kind string) string {
	if kind == "ufw" {
		return "ufw (rule " + s.Service + ")"
	}
	return "firewalld (service " + s.Service + ")"
}

// FirewallCheckRow is the check row: what applying s would do.
func FirewallCheckRow(f FirewallFacts, s FirewallSpec) Row {
	row := Row{ID: "firewall", Title: "Firewall"}
	ports := PortsText(s.Ports)
	switch {
	case f.Kind == "firewalld" || f.Kind == "ufw":
		name := f.Kind
		if f.Open {
			row.Status, row.Detail = OK, fmt.Sprintf("%s is active and %s %s already open %s.", name, ports, are(ports), strings.TrimPrefix(s.where(f.Kind), name+" "))
		} else {
			row.Status, row.Detail = Info, fmt.Sprintf("%s is active. %s will open %s %s.", name, s.Actor, ports, s.Purpose)
		}
	case f.Nft:
		row.Status, row.Detail = Info, fmt.Sprintf("A firewall may be active (nftables rules found); open these ports yourself: %s %s.", ports, s.Purpose)
	default:
		row.Status, row.Detail = OK, "No firewall is active on this host. Nothing to open."
	}
	return row
}

func are(ports string) string {
	if strings.HasPrefix(ports, "TCP ports") {
		return "are"
	}
	return "is"
}

// ManualCommands are the commands an administrator runs by hand for kind.
func ManualCommands(kind string, s FirewallSpec) string {
	var c []string
	switch kind {
	case "ufw":
		for _, p := range s.Ports {
			c = append(c, fmt.Sprintf("ufw allow %s/tcp comment '%s'", strings.ReplaceAll(p, "-", ":"), s.Service))
		}
	default:
		c = append(c, "firewall-cmd --permanent --new-service="+s.Service)
		for _, p := range s.Ports {
			c = append(c, fmt.Sprintf("firewall-cmd --permanent --service=%s --add-port=%s/tcp", s.Service, p))
		}
		c = append(c, "firewall-cmd --permanent --add-service="+s.Service, "firewall-cmd --reload")
	}
	return strings.Join(c, " && ")
}

// RunFunc runs a shell script as root on the machine and returns its output.
type RunFunc func(script string) (string, error)

// ApplyFirewall opens s's ports (or removes the rules when s has no ports).
// The row is OK when the firewall is as wanted (changed says whether
// something was done), Info when the administrator has to open ports (nftables
// without a front end), and Warning when a firewall command failed; the
// warning carries the commands to run by hand. It never returns an error row:
// a firewall problem must not stop the caller.
func ApplyFirewall(run RunFunc, s FirewallSpec) (row Row, changed bool) {
	row = Row{ID: "firewall", Title: "Firewall"}
	ports := PortsText(s.Ports)
	out, err := run(FirewallScript(s))
	kind, failed, detail, nft := "", "", "", false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "@@kind "):
			kind = strings.TrimPrefix(l, "@@kind ")
		case strings.HasPrefix(l, "@@failed "):
			failed = strings.TrimPrefix(l, "@@failed ")
		case strings.HasPrefix(l, "@@detail "):
			detail = strings.TrimPrefix(l, "@@detail ")
		case l == "@@nft":
			nft = true
		case l == "@@changed":
			changed = true
		}
	}
	remove := len(s.Ports) == 0
	manual := func(kind string) string {
		if remove {
			return ""
		}
		if kind == "" || kind == "none" {
			return " If a firewall is active, open " + ports + " yourself."
		}
		return " Run as root: " + ManualCommands(kind, s)
	}
	switch {
	case failed != "":
		row.Status = Warning
		row.Detail = "The firewall command `" + failed + "` failed"
		if detail != "" {
			row.Detail += " (" + detail + ")"
		}
		row.Detail += ". Nothing else was changed in the firewall." + manual(kind)
		if remove {
			row.Detail += " Remove the rule " + s.Service + " yourself."
		}
		return row, false
	case err != nil:
		row.Status = Warning
		row.Detail = "Could not change the firewall: " + firstLine(err.Error()+" "+out) + "." + manual(kind)
		return row, false
	case kind != "firewalld" && kind != "ufw":
		row.Status = OK
		switch {
		case remove:
			row.Detail = "No firewall is active on this host. Nothing to remove."
		case nft:
			row.Status = Info
			row.Detail = fmt.Sprintf("A firewall may be active (nftables rules found); open these ports yourself: %s %s.", ports, s.Purpose)
		default:
			row.Detail = "No firewall is active on this host. Nothing to open."
		}
		return row, false
	}
	row.Status = OK
	switch {
	case remove && changed:
		if kind == "firewalld" {
			row.Detail = "Removed the firewalld service " + s.Service + "."
		} else {
			row.Detail = "Removed the ufw rules marked " + s.Service + "."
		}
	case remove:
		row.Detail = "No rule of Virticle (" + s.Service + ") found in " + kind + ". Nothing to remove."
	case changed:
		row.Detail = fmt.Sprintf("Opened %s in %s.", ports, s.where(kind))
	default:
		row.Detail = fmt.Sprintf("%s %s already open in %s.", ports, are(ports), s.where(kind))
	}
	return row, changed
}
