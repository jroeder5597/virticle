package nodecmd

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// disk-boot PATH: say what an existing disk image can start with, by reading its first 64 KiB through qemu-img
// (never mounting it, never running the guest) and looking at the partition table. Prints one JSON line:
//
//	{"table":"gpt","esp":true,"biosBoot":false,"bootCode":true}
//
// table is none (nothing recognisable: an empty disk, or one without a partition table), mbr, gpt, or unknown (a
// partition table that cannot be read). esp: an EFI System Partition exists. biosBoot: a BIOS boot partition exists
// (GPT). bootCode: the first 440 bytes of the first sector are not empty (a boot loader for BIOS).
func init() { Register("disk-boot", verbDiskBoot) }

// BootInfo is the result of looking at the first sectors of a disk.
type BootInfo struct {
	Table    string `json:"table"`
	ESP      bool   `json:"esp"`
	BiosBoot bool   `json:"biosBoot"`
	BootCode bool   `json:"bootCode"`
}

// bootProbeSize covers a GPT with 512 byte sectors (header at 512, 128 entries up to 17408) and with 4096 byte
// sectors (header at 4096, entries from 8192 to 24576).
const bootProbeSize = 64 << 10

var (
	guidESP     = []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	guidBIOSBoo = []byte{0x48, 0x61, 0x68, 0x21, 0x49, 0x64, 0x6F, 0x6E, 0x74, 0x4E, 0x65, 0x65, 0x64, 0x45, 0x46, 0x49}
)

// ParseBoot reads the start of a disk image (at least one sector; the rest as far as it goes).
func ParseBoot(b []byte) BootInfo {
	if len(b) < 512 {
		return BootInfo{Table: "none"}
	}
	var info BootInfo
	info.BootCode = !allZero(b[:440])
	mbrOK := b[510] == 0x55 && b[511] == 0xAA
	// GPT first: its header sits in the second sector, whatever the sector size (512 or 4096).
	for _, ss := range []int{512, 4096} {
		if len(b) >= ss+92 && bytes.Equal(b[ss:ss+8], []byte("EFI PART")) {
			return parseGPT(b, ss, info)
		}
	}
	if !mbrOK {
		info.Table = "none"
		return info
	}
	parts, ee := 0, false
	for i := 0; i < 4; i++ {
		e := b[446+16*i : 446+16*(i+1)]
		if e[4] == 0 {
			continue
		}
		if e[4] == 0xEE {
			ee = true
			continue
		}
		parts++
		if e[4] == 0xEF {
			info.ESP = true
		}
	}
	switch {
	case ee && parts == 0:
		info.Table = "unknown" // a protective MBR whose GPT header is not where either sector size puts it
	case parts == 0:
		info.Table = "none" // a boot sector without partitions
	default:
		info.Table = "mbr"
	}
	return info
}

func parseGPT(b []byte, ss int, info BootInfo) BootInfo {
	h := b[ss:]
	entryLBA := binary.LittleEndian.Uint64(h[72:80])
	n := binary.LittleEndian.Uint32(h[80:84])
	size := binary.LittleEndian.Uint32(h[84:88])
	if size < 128 || size > 4096 || n == 0 || n > 1024 || entryLBA < 2 || entryLBA > 64 {
		info.Table = "unknown"
		return info
	}
	info.Table = "gpt"
	start := int(entryLBA) * ss
	read := 0
	for i := uint32(0); i < n; i++ {
		o := start + int(i)*int(size)
		if o+16 > len(b) {
			break // the entries run past what was read; judge by those seen
		}
		read++
		t := b[o : o+16]
		switch {
		case bytes.Equal(t, guidESP):
			info.ESP = true
		case bytes.Equal(t, guidBIOSBoo):
			info.BiosBoot = true
		}
	}
	if read == 0 {
		info.Table = "unknown"
	}
	return info
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func verbDiskBoot(c *Ctx) error {
	if err := c.Need(1, 1, "disk-boot PATH"); err != nil {
		return err
	}
	r, err := c.ResolveRead(c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	st, exists, _ := r.Lstat()
	if !exists || !isRegMode(st.Mode) {
		return Fail("%s is not a regular file", c.Args[0])
	}
	format := "raw"
	if IsQcow2(r.Path) {
		format = "qcow2"
		if err := c.CheckChain(r.Path); err != nil {
			return err
		}
	}
	if !c.HasTool("qemu-img") {
		return Fail("qemu-img is not installed on this node")
	}
	fd, err := unix.MemfdCreate("virticle-disk-boot", unix.MFD_CLOEXEC)
	if err != nil {
		return Fail("no scratch memory file: %v", err)
	}
	out := os.NewFile(uintptr(fd), "scratch")
	defer out.Close()
	cmd, err := c.Command("qemu-img", "dd", "-U", "-f", format, "-O", "raw", fmt.Sprintf("bs=%d", 512), fmt.Sprintf("count=%d", bootProbeSize/512), "if="+r.Path, "of=/proc/self/fd/3")
	if err != nil {
		return err
	}
	cmd.ExtraFiles = []*os.File{out}
	if msg, err := cmd.CombinedOutput(); err != nil {
		return Fail("qemu-img dd failed: %s", string(bytes.TrimSpace(msg)))
	}
	buf := make([]byte, bootProbeSize)
	n, _ := out.ReadAt(buf, 0)
	return json.NewEncoder(c.Stdout).Encode(ParseBoot(buf[:n]))
}
