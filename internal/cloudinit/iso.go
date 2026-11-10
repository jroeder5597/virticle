package cloudinit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

const sector = 2048

// BuildImage returns an ISO9660 image with the given volume label that holds
// the files (flat, no directories) under their exact names. Both Rock Ridge
// and Joliet name records are written: Linux (cloud-init wants the lowercase
// names "user-data" and "meta-data") reads either, Windows reads Joliet.
// Plain level 1 names would be mangled to USER_DAT.;1.
func BuildImage(label string, files map[string][]byte) ([]byte, error) {
	if label == "" || len(label) > 32 {
		return nil, fmt.Errorf("volume label must be 1 to 32 characters")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		if n == "" || strings.ContainsAny(n, "/\\\x00") || len(n) > 64 {
			return nil, fmt.Errorf("invalid file name %q", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)

	// Layout in sectors: 16 primary descriptor, 17 Joliet descriptor,
	// 18 terminator, 19-22 path tables, 23 primary root directory,
	// 24 Joliet root directory, 25.. file data.
	const (
		ptL, ptM, jptL, jptM = 19, 20, 21, 22
		rootDir, jRootDir    = 23, 24
		firstData            = 25
	)
	type ent struct {
		name, id string
		data     []byte
		lba      int
	}
	ents := make([]*ent, len(names))
	next := firstData
	used := map[string]bool{}
	for i, n := range names {
		e := &ent{name: n, id: isoName(n, used), data: files[n], lba: next}
		next += (len(e.data) + sector - 1) / sector
		ents[i] = e
	}
	total := next

	now := time.Now().UTC()
	dirRec := func(id []byte, lba, size int, dir bool, su []byte) []byte {
		n := 33 + len(id)
		if n%2 == 1 {
			n++
		}
		b := make([]byte, n, n+len(su))
		b[0] = byte(n + len(su))
		both32(b[2:], uint32(lba))
		both32(b[10:], uint32(size))
		b[18], b[19], b[20] = byte(now.Year()-1900), byte(now.Month()), byte(now.Day())
		b[21], b[22], b[23] = byte(now.Hour()), byte(now.Minute()), byte(now.Second())
		if dir {
			b[25] = 2
		}
		both16(b[28:], 1)
		b[32] = byte(len(id))
		copy(b[33:], id)
		return append(b, su...)
	}
	// Rock Ridge system use entries: SP marks the extension, PX gives the
	// mode, NM carries the real file name.
	px := func(mode uint32) []byte {
		b := make([]byte, 36)
		copy(b, "PX")
		b[2], b[3] = 36, 1
		both32(b[4:], mode)
		both32(b[12:], 1)
		return b
	}
	nm := func(name string) []byte { return append([]byte{'N', 'M', byte(5 + len(name)), 1, 0}, name...) }
	sp := []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0}

	rootP := dirRec([]byte{0}, rootDir, sector, true, nil)
	rootJ := dirRec([]byte{0}, jRootDir, sector, true, nil)
	var prim, jol bytes.Buffer
	prim.Write(dirRec([]byte{0}, rootDir, sector, true, append(append([]byte{}, sp...), px(040555)...)))
	prim.Write(dirRec([]byte{1}, rootDir, sector, true, px(040555)))
	jol.Write(rootJ)
	jol.Write(dirRec([]byte{1}, jRootDir, sector, true, nil))
	for _, e := range ents {
		su := append(px(0100444), nm(e.name)...)
		if len(su)%2 == 1 {
			su = append(su, 0)
		}
		prim.Write(dirRec([]byte(e.id), e.lba, len(e.data), false, su))
		jol.Write(dirRec(ucs2(e.name), e.lba, len(e.data), false, nil))
	}
	if prim.Len() > sector || jol.Len() > sector {
		return nil, fmt.Errorf("too many files for the image directory")
	}

	img := make([]byte, total*sector)
	at := func(lba int) []byte { return img[lba*sector : (lba+1)*sector] }

	pathTable := func(rootLBA int, order binary.ByteOrder) []byte {
		b := make([]byte, 10)
		b[0] = 1
		order.PutUint32(b[2:], uint32(rootLBA))
		order.PutUint16(b[6:], 1)
		return b
	}
	copy(at(ptL), pathTable(rootDir, binary.LittleEndian))
	copy(at(ptM), pathTable(rootDir, binary.BigEndian))
	copy(at(jptL), pathTable(jRootDir, binary.LittleEndian))
	copy(at(jptM), pathTable(jRootDir, binary.BigEndian))
	copy(at(rootDir), prim.Bytes())
	copy(at(jRootDir), jol.Bytes())
	for _, e := range ents {
		copy(img[e.lba*sector:], e.data)
	}

	descriptor := func(lba int, joliet bool, root []byte, l, m int) {
		b := at(lba)
		b[0] = 1
		if joliet {
			b[0] = 2
		}
		copy(b[1:], "CD001")
		b[6] = 1
		// Text fields are space padded (UCS-2 spaces for Joliet).
		text := func(off, n int, s string) {
			dst := b[off : off+n]
			if joliet {
				for i := range dst {
					dst[i] = 0
					if i%2 == 1 {
						dst[i] = ' '
					}
				}
				copy(dst, ucs2(s))
				return
			}
			for i := range dst {
				dst[i] = ' '
			}
			copy(dst, s)
		}
		text(8, 32, "")
		if joliet {
			text(40, 32, label)
		} else {
			text(40, 32, strings.ToUpper(label))
		}
		both32(b[80:], uint32(total))
		if joliet {
			copy(b[88:], []byte{0x25, 0x2F, 0x45}) // UCS-2 level 3
		}
		both16(b[120:], 1)
		both16(b[124:], 1)
		both16(b[128:], sector)
		both32(b[132:], 10) // path table size
		binary.LittleEndian.PutUint32(b[140:], uint32(l))
		binary.BigEndian.PutUint32(b[148:], uint32(m))
		copy(b[156:190], root)
		for _, off := range []int{190, 318, 446, 574} { // set, publisher, preparer, application
			text(off, 128, "")
		}
		for _, off := range []int{702, 739, 776} { // copyright, abstract, bibliographic
			text(off, 37, "")
		}
		ts := now.Format("20060102150405") + "00"
		for _, off := range []int{813, 830, 864} { // created, modified, effective
			copy(b[off:], ts)
		}
		copy(b[847:], "0000000000000000") // no expiry
		b[881] = 1
	}
	descriptor(16, false, rootP, ptL, ptM)
	descriptor(17, true, rootJ, jptL, jptM)
	end := at(18)
	end[0] = 255
	copy(end[1:], "CD001")
	end[6] = 1
	return img, nil
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func ucs2(s string) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u>>8), byte(u))
	}
	return out
}

// isoName makes a unique level 1 style identifier (NAME.EXT;1) for readers
// that understand neither extension; the real name travels in the Rock Ridge
// and Joliet records.
func isoName(name string, used map[string]bool) string {
	clean := func(s string, n int) string {
		var b strings.Builder
		for _, r := range strings.ToUpper(s) {
			if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		if b.Len() > n {
			return b.String()[:n]
		}
		return b.String()
	}
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		base, ext = name[:i], name[i+1:]
	}
	b, e := clean(base, 8), clean(ext, 3)
	for i := 1; ; i++ {
		id := b + "." + e + ";1"
		if !used[id] {
			used[id] = true
			return id
		}
		sfx := fmt.Sprint(i)
		b = clean(base, 8-len(sfx)) + sfx
	}
}
