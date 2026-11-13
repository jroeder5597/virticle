package nodecmd

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// disk builds a synthetic start of a disk: an MBR (boot code and partition types) and, when gptTypes is not nil,
// a GPT with sector size ss whose entries have those type GUIDs.
func disk(ss int, code bool, mbrTypes []byte, gptTypes [][]byte) []byte {
	b := make([]byte, bootProbeSize)
	if code {
		copy(b, []byte{0xEB, 0x63, 0x90, 0x10, 0x8E, 0xD0})
	}
	for i, t := range mbrTypes {
		b[446+16*i+4] = t
	}
	if mbrTypes != nil {
		b[510], b[511] = 0x55, 0xAA
	}
	if gptTypes != nil {
		h := b[ss:]
		copy(h, "EFI PART")
		binary.LittleEndian.PutUint64(h[72:], 2)
		binary.LittleEndian.PutUint32(h[80:], 128)
		binary.LittleEndian.PutUint32(h[84:], 128)
		for i, g := range gptTypes {
			copy(b[2*ss+128*i:], g)
			b[2*ss+128*i+16] = 1 // a unique GUID, so the entry counts as used
		}
	}
	return b
}

var linuxFS = []byte{0xAF, 0x3D, 0xC6, 0x0F, 0x83, 0x84, 0x72, 0x47, 0x8E, 0x79, 0x3D, 0x69, 0xD8, 0x47, 0x7D, 0xE4}

func TestParseBoot(t *testing.T) {
	rows := []struct {
		name string
		b    []byte
		want BootInfo
	}{
		{"MBR with boot code, no ESP", disk(512, true, []byte{0x83}, nil), BootInfo{Table: "mbr", BootCode: true}},
		{"MBR with an EFI partition", disk(512, false, []byte{0xEF, 0x83}, nil), BootInfo{Table: "mbr", ESP: true}},
		{"GPT with the ESP only", disk(512, false, []byte{0xEE}, [][]byte{linuxFS, guidESP}), BootInfo{Table: "gpt", ESP: true}},
		{"GPT with ESP and BIOS boot partition (cloud image)", disk(512, true, []byte{0xEE}, [][]byte{linuxFS, guidBIOSBoo, guidESP}), BootInfo{Table: "gpt", ESP: true, BiosBoot: true, BootCode: true}},
		{"GPT without ESP", disk(512, true, []byte{0xEE}, [][]byte{linuxFS}), BootInfo{Table: "gpt", BootCode: true}},
		{"GPT with 4096 byte sectors and an ESP", disk(4096, false, []byte{0xEE}, [][]byte{guidESP}), BootInfo{Table: "gpt", ESP: true}},
		{"empty disk", make([]byte, bootProbeSize), BootInfo{Table: "none"}},
		{"garbage", []byte(strings.Repeat("hello world ", 5000)), BootInfo{Table: "none", BootCode: true}},
		{"too short", []byte{1, 2, 3}, BootInfo{Table: "none"}},
		{"boot sector without partitions", disk(512, true, []byte{}, nil), BootInfo{Table: "none", BootCode: true}},
		{"protective MBR but no GPT header", disk(512, false, []byte{0xEE}, nil), BootInfo{Table: "unknown"}},
	}
	rows[9].b[510], rows[9].b[511] = 0x55, 0xAA
	for _, r := range rows {
		if got := ParseBoot(r.b); got != r.want {
			t.Errorf("%s: %+v, want %+v", r.name, got, r.want)
		}
	}
	// A GPT whose entry table is damaged is unknown, not a guess.
	bad := disk(512, false, []byte{0xEE}, [][]byte{guidESP})
	binary.LittleEndian.PutUint32(bad[512+84:], 7)
	if got := ParseBoot(bad); got.Table != "unknown" {
		t.Errorf("damaged GPT: %+v", got)
	}
}

func TestDiskBootVerb(t *testing.T) {
	n := newNode(t)
	n.put("pools/default/a.img", string(disk(512, false, []byte{0xEE}, [][]byte{guidESP})))
	// A fake qemu-img dd: copies the first count blocks of if= to of=.
	fake := n.abs("fake-qemu-img")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nfor a; do case $a in if=*) i=${a#if=};; of=*) o=${a#of=};; esac; done\nhead -c 65536 \"$i\" > \"$o\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n.cfg.Tools["qemu-img"] = fake
	var got BootInfo
	if err := json.Unmarshal([]byte(n.ok("", "disk-boot", n.pool("a.img"))), &got); err != nil || !got.ESP || got.Table != "gpt" {
		t.Errorf("%+v %v", got, err)
	}
	for _, p := range badPaths(n.abs("pools/default")) {
		n.denied("disk-boot "+p, "", "disk-boot", p)
	}
	n.denied("outside the pools", "", "disk-boot", n.abs("outside/x"))
}
