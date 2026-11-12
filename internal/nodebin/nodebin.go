// Package nodebin carries the node command (cmd/virticle-node) for the
// architectures a node can have, so "virticle node prepare" can upload it. The
// binaries are built by "make nodebin" into dist/ and are not committed. A
// checkout that has not run it still compiles; asking for a binary then gives a
// clear error at run time.
package nodebin

import (
	"embed"
	"fmt"
)

// dist holds virticle-node_linux_amd64 and virticle-node_linux_arm64. The
// pattern also matches the committed placeholder .gitkeep.
//
//go:embed dist/*
var dist embed.FS

// GoArch maps the machine name `uname -m` prints to a Go architecture name.
func GoArch(uname string) (string, bool) {
	switch uname {
	case "x86_64", "amd64":
		return "amd64", true
	case "aarch64", "arm64":
		return "arm64", true
	}
	return "", false
}

// Binary returns the node command for the machine name uname (`uname -m`).
func Binary(uname string) ([]byte, error) {
	arch, ok := GoArch(uname)
	if !ok {
		return nil, fmt.Errorf("there is no node command for the architecture %q (x86_64 and aarch64 are built)", uname)
	}
	b, err := dist.ReadFile("dist/virticle-node_linux_" + arch)
	if err != nil {
		return nil, fmt.Errorf("this controller was built without the node command for %s; build it with \"make nodebin\" and rebuild", arch)
	}
	return b, nil
}
