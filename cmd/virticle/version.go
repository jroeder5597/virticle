package main

import (
	"flag"
	"fmt"

	"virticle/internal/buildinfo"
)

var showVersion = flag.Bool("version", false, "print the version and exit")

// versionRequested prints the version and reports true for "virticle version"
// and "virticle -version"; call it right after flag.Parse.
func versionRequested() bool {
	if *showVersion || flag.Arg(0) == "version" {
		fmt.Println(buildinfo.Get())
		return true
	}
	return false
}
