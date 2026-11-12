// Command virticle-node is the program a node lets the controller run as root.
// It is installed at /usr/local/libexec/virticle-node and started on demand
// (through sudo for the service account, or directly over SSH as root). It has
// no daemon and no listener. All of its code is in internal/nodecmd.
package main

import (
	"os"

	"virticle/internal/nodecmd"
)

func main() {
	os.Exit(nodecmd.Main(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
