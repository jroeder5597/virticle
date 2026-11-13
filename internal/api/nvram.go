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

// withQcow2Nvram rewrites the <nvram> element to a qcow2 store at p and
// removes the firmware selection (see withoutFirmwareSelection). The template
// of the new element is the store itself, in qcow2: libvirt 11 refuses to
// start a VM whose template format differs from its store format, even when
// the store exists, and it takes any definition whose template is the
// distribution's file for one to select the firmware for again. The store
// never needs to be built from it, so naming the store costs nothing. The
// distribution's own raw template is kept in the application metadata
// (nvramMeta) for the node that must build a store (clone, migration).
func withQcow2Nvram(domXML string, n nvramEl, p string) string {
	el := fmt.Sprintf("<nvram template='%s' templateFormat='qcow2' format='qcow2'>%s</nvram>", esc(p), esc(p))
	done := false
	out := reNvramFull.ReplaceAllStringFunc(domXML, func(m string) string {
		if done {
			return m
		}
		done = true
		return el
	})
	return withoutFirmwareSelection(out)
}

var reNvramMeta = regexp.MustCompile(`<[A-Za-z_][\w.-]*:nvram\b([^>]*)/?>`)

// rawNvramTemplate is the distribution's raw VARS template a VM's qcow2
// store was made from: the template of the <nvram> element, or for a store
// that names itself as its template the one kept in the metadata. "" when
// unknown.
func rawNvramTemplate(domXML string, n nvramEl) string {
	if n.Template != "" && n.Template != n.Path && !strings.HasSuffix(n.Template, ".qcow2") {
		return n.Template
	}
	if m := reNvramMeta.FindStringSubmatch(domXML); m != nil {
		return attrOf(m[1], "template")
	}
	return ""
}

// keepRawNvramTemplate records tpl in the application metadata of dom.
func keepRawNvramTemplate(h *hostconn.Host, dom libvirt.Domain, tpl string) error {
	old, err := domainMeta(h, dom)
	if err != nil {
		return err
	}
	meta, err := metaSetChild(old, "nvram", fmt.Sprintf("<nvram template='%s'/>", esc(tpl)))
	if err != nil {
		return err
	}
	return setDomainMeta(h, dom, meta)
}

// withRawNvramTemplate points the <nvram> element of a clone's definition back
// at the distribution's template (the clone's store is a new file).
func withRawNvramTemplate(domXML, tpl string) string {
	if tpl == "" {
		return domXML
	}
	done := false
	return reNvramFull.ReplaceAllStringFunc(domXML, func(m string) string {
		if done {
			return m
		}
		done = true
		sm := reNvramFull.FindStringSubmatch(m)
		attrs := regexp.MustCompile(`\s(?:template|templateFormat)=(?:'[^']*'|"[^"]*")`).ReplaceAllString(sm[1], "")
		return fmt.Sprintf("<nvram template='%s' templateFormat='raw'%s>%s</nvram>", esc(tpl), attrs, sm[2])
	})
}

var (
	reOSFirmwareAttr = regexp.MustCompile(`(<os\b[^>]*?)\s+firmware=(?:'[^']*'|"[^"]*")`)
	reFirmwareBlock  = regexp.MustCompile(`(?s)[ \t]*<firmware>.*?</firmware>[ \t]*\n?`)
)

// withoutFirmwareSelection turns a definition that lets libvirt pick the
// firmware (<os firmware='efi'>) into one that names it. The definition
// already holds what libvirt picked (loader, store, Secure Boot attribute).
// libvirt picks the firmware again on every start, and then compares the
// distribution's raw VARS template with the qcow2 store and refuses with
// "conversion of the nvram template to another target format is not
// supported", although the store exists. Seen on Rocky 10 (libvirt 11.10).
func withoutFirmwareSelection(domXML string) string {
	domXML = reOSFirmwareAttr.ReplaceAllString(domXML, "$1")
	return reFirmwareBlock.ReplaceAllString(domXML, "")
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
	if !ok || n.Path == "" || n.Template == "" || !safePathRE.MatchString(n.Path) {
		return ""
	}
	n.Template = rawNvramTemplate(raw, n)
	if n.Template == "" || !safePathRE.MatchString(n.Template) {
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
	// Also when the definition already names this qcow2 store (libvirt 11
	// does that for a new firmware-selected VM): the firmware selection is
	// still removed, see withoutFirmwareSelection.
	_, err = h.DomainDefineXMLFlags(withQcow2Nvram(raw, n, target), libvirt.DomainDefineValidate)
	if err != nil {
		_ = h.Remove(target)
		log.Printf("nvram %s: qcow2 store refused on %s: %v", dom.Name, h.Name, err)
		return "The UEFI variable store of this VM is a raw file (libvirt did not accept a qcow2 one), so snapshots of it are external."
	}
	if err := keepRawNvramTemplate(h, dom, n.Template); err != nil {
		log.Printf("nvram %s: could not record the firmware template on %s: %v", dom.Name, h.Name, err)
	}
	return ""
}

var reNvramTemplateAttr = regexp.MustCompile(`(<nvram\b[^>]*?\btemplate=)(?:'[^']*'|"[^"]*")`)

// withNvramTemplate sets the template attribute of the <nvram> element.
func withNvramTemplate(domXML, tpl string) string {
	return reNvramTemplateAttr.ReplaceAllString(domXML, "${1}'"+esc(tpl)+"'")
}
