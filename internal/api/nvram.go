package api

import (
	"fmt"
	"log"
	"path"
	"regexp"
	"strings"

	"github.com/digitalocean/go-libvirt"

	"virticle/internal/hostconn"
)

// UEFI variable stores. A VM whose firmware keeps its variables in a qcow2
// file can take internal snapshots (libvirt refuses them for a raw store), so
// new UEFI VMs get a qcow2 store. libvirt 11 cannot convert the firmware's
// raw VARS template itself ("conversion of the nvram template to another
// target format is not supported"), so the node command converts it
// (hostconn.NewNVRAM) before the VM first starts, and the definition points
// at the converted file. Existing VMs with a raw store are never converted.

var reNvramFull = regexp.MustCompile(`<nvram\b([^>]*)>([^<]*)</nvram>`)

// nvramEl is what a domain definition says about its UEFI variable store.
type nvramEl struct {
	Template, TemplateFormat, Format, Path string
}

func attrOf(attrs, name string) string {
	m := regexp.MustCompile(`\b` + name + `=(?:'([^']*)'|"([^"]*)")`).FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	return xmlUnescape.Replace(m[1] + m[2])
}

func parseNvramEl(domXML string) (nvramEl, bool) {
	m := reNvramFull.FindStringSubmatch(domXML)
	if m == nil {
		return nvramEl{}, false
	}
	return nvramEl{
		Template: attrOf(m[1], "template"), TemplateFormat: attrOf(m[1], "templateFormat"),
		Format: attrOf(m[1], "format"), Path: strings.TrimSpace(xmlUnescape.Replace(m[2])),
	}, true
}

// nvramIsQcow2 reports whether the definition keeps its UEFI variables in qcow2.
func nvramIsQcow2(domXML string) bool {
	n, ok := parseNvramEl(domXML)
	return ok && n.Format == "qcow2"
}

// withQcow2Nvram rewrites the <nvram> element to a qcow2 store at p.
func withQcow2Nvram(domXML string, n nvramEl, p string) string {
	tf := n.TemplateFormat
	if tf == "" {
		tf = "raw"
	}
	el := fmt.Sprintf("<nvram template='%s' templateFormat='%s' format='qcow2'>%s</nvram>", esc(n.Template), esc(tf), esc(p))
	done := false
	return reNvramFull.ReplaceAllStringFunc(domXML, func(m string) string {
		if done {
			return m
		}
		done = true
		return el
	})
}

// useQcow2Nvram gives a freshly defined, never started UEFI VM a qcow2
// variable store. It returns a note (empty when all went well or the VM has no
// UEFI store) saying why the VM keeps the raw store; this never fails the
// caller. A VM that already has a qcow2 definition gets its file created when
// it is missing (a clone, which has no path yet).
func useQcow2Nvram(h *hostconn.Host, dom libvirt.Domain) string {
	raw, err := h.DomainGetXMLDesc(dom, libvirt.DomainXMLInactive)
	if err != nil {
		return ""
	}
	n, ok := parseNvramEl(raw)
	if !ok || n.Path == "" || n.Template == "" || !safePathRE.MatchString(n.Path) || !safePathRE.MatchString(n.Template) {
		return ""
	}
	target := path.Join(path.Dir(n.Path), dom.Name+"_VARS.qcow2")
	if !safePathRE.MatchString(target) {
		return ""
	}
	if n.Format == "qcow2" && n.Path == target {
		if ex, err := h.Exists(target); err == nil && ex[target] {
			return ""
		}
	}
	if ex, err := h.Exists(target); err != nil || ex[target] {
		return "" // something is there already; never touch it
	}
	if err := h.NewNVRAM(n.Template, target); err != nil {
		log.Printf("nvram %s: no qcow2 store on %s: %v", dom.Name, h.Name, err)
		return "The UEFI variable store of this VM is a raw file (the node could not make a qcow2 one: " + impFirstLine(err.Error()) + "), so snapshots of it are external."
	}
	if n.Format == "qcow2" && n.Path == target {
		return ""
	}
	if _, err := h.DomainDefineXMLFlags(withQcow2Nvram(raw, n, target), libvirt.DomainDefineValidate); err != nil {
		_ = h.Remove(target)
		log.Printf("nvram %s: qcow2 store refused on %s: %v", dom.Name, h.Name, err)
		return "The UEFI variable store of this VM is a raw file (libvirt did not accept a qcow2 one), so snapshots of it are external."
	}
	return ""
}
