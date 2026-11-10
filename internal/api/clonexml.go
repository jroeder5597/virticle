package api

import (
	"fmt"
	"regexp"
	"strings"
)

// vpNS is the namespace of this application's data in a domain's <metadata>.
const vpNS = "https://virt-proj.invalid/ns/1"

// cloneEdit describes the changes that turn a template's definition into a
// clone's. Everything not named here is left exactly as it was.
type cloneEdit struct {
	Name string
	// Disks maps a source file path to the clone's path. An empty value
	// empties that device (used for the template's own cloud-init seed).
	Disks map[string]string
	// AddCDROM, when set and not consumed by a Disks entry, is attached as a
	// new read-only SATA CD-ROM.
	AddCDROM string
}

var (
	reNameEl    = regexp.MustCompile(`(<name>)[^<]*(</name>)`)
	reUUIDEl    = regexp.MustCompile(`[ \t]*<uuid>[^<]*</uuid>[ \t]*\n?`)
	reMACEl     = regexp.MustCompile(`[ \t]*<mac\b[^>]*?(?:/>|>\s*</mac>)[ \t]*\n?`)
	reDiskEl    = regexp.MustCompile(`(?s)<disk\b.*?</disk>`)
	reFileSrc   = regexp.MustCompile(`<source\b[^>]*?\bfile=(['"])(.*?)(['"])[^>]*?/?>`)
	reNvramEl   = regexp.MustCompile(`(<nvram\b[^>]*>)[^<]*(</nvram>)`)
	reTargetDev = regexp.MustCompile(`<target\b[^>]*\bdev=['"]([^'"]+)['"]`)
	reMetaEmpty = regexp.MustCompile(`[ \t]*<metadata\s*(?:/>|>\s*</metadata>)[ \t]*\n?`)
	reVPElement = regexp.MustCompile(`(?s)[ \t]*<([A-Za-z_][\w.-]*):meta\b([^>]*?)(?:/>|>.*?</([A-Za-z_][\w.-]*):meta>)[ \t]*\n?`)
)

var xmlUnescape = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&#34;", `"`, "&#39;", "'")

// cloneDomainXML makes a targeted edit of a (preferably inactive) domain
// definition: new name, no <uuid> and no NIC <mac> (libvirt generates both),
// no template marker, new disk paths, an empty firmware NVRAM path (libvirt
// builds a fresh one from the firmware's template). All other text,
// including unknown elements, <metadata> and the qemu: namespace, is kept.
func cloneDomainXML(src string, e cloneEdit) (string, error) {
	if !strings.Contains(src, "<domain") {
		return "", fmt.Errorf("not a domain definition")
	}
	if !reNameEl.MatchString(src) {
		return "", fmt.Errorf("domain definition has no <name>")
	}
	replaced := false
	out := reNameEl.ReplaceAllStringFunc(src, func(m string) string {
		if replaced {
			return m
		}
		replaced = true
		return "<name>" + esc(e.Name) + "</name>"
	})
	if loc := reUUIDEl.FindStringIndex(out); loc != nil {
		out = out[:loc[0]] + out[loc[1]:]
	}
	out = reMACEl.ReplaceAllString(out, "")
	out = reNvramEl.ReplaceAllString(out, "$1$2")

	// Application metadata (template marker, snapshot schedule) is not inherited.
	out = reVPElement.ReplaceAllStringFunc(out, func(m string) string {
		if strings.Contains(m, vpNS) {
			return ""
		}
		return m
	})
	out = reMetaEmpty.ReplaceAllString(out, "")

	used := map[string]bool{}
	attached := false
	out = reDiskEl.ReplaceAllStringFunc(out, func(d string) string {
		if m := reTargetDev.FindStringSubmatch(d); m != nil {
			used[m[1]] = true
		}
		sm := reFileSrc.FindStringSubmatch(d)
		if sm == nil {
			return d
		}
		np, ok := e.Disks[xmlUnescape.Replace(sm[2])]
		if !ok {
			return d
		}
		if np == "" {
			// An emptied CD-ROM takes the new media when there is some.
			isCD := strings.Contains(d, "device='cdrom'") || strings.Contains(d, `device="cdrom"`)
			if isCD && e.AddCDROM != "" && !attached {
				attached = true
				np = e.AddCDROM
			} else {
				return strings.Replace(d, sm[0], "", 1)
			}
		}
		return strings.Replace(d, sm[0], strings.Replace(sm[0], sm[1]+sm[2]+sm[3], "'"+esc(np)+"'", 1), 1)
	})

	if e.AddCDROM != "" && !attached {
		dev := ""
		for c := 'a'; c <= 'z'; c++ {
			if !used["sd"+string(c)] {
				dev = "sd" + string(c)
				break
			}
		}
		if dev == "" {
			return "", fmt.Errorf("no free SATA device name for the seed CD-ROM")
		}
		i := strings.LastIndex(out, "</devices>")
		if i < 0 {
			return "", fmt.Errorf("domain definition has no <devices>")
		}
		cd := fmt.Sprintf("  <disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      <source file='%s'/>\n      <target dev='%s' bus='sata'/>\n      <readonly/>\n    </disk>\n  ", esc(e.AddCDROM), dev)
		out = out[:i] + cd + out[i:]
	}
	return out, nil
}
