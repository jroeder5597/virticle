// Package buildinfo holds the version of this build. The release build sets
// Version, Commit and Date with -ldflags -X; a plain go build reports "dev"
// and takes the commit from the VCS data the Go toolchain embeds.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is what GET /api/version returns.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

// Get returns the build information, filling in what the linker did not.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date, Go: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && i.Commit == "":
				i.Commit = s.Value
			case s.Key == "vcs.time" && i.Date == "":
				i.Date = s.Value
			}
		}
	}
	if len(i.Commit) > 12 {
		i.Commit = i.Commit[:12]
	}
	if i.Commit == "" {
		i.Commit = "unknown"
	}
	if i.Date == "" {
		i.Date = "unknown"
	}
	return i
}

// String is the one-line form printed by virticle version.
func (i Info) String() string {
	return fmt.Sprintf("virticle %s (commit %s, built %s, %s, %s)", i.Version, i.Commit, i.Date, i.Go, i.Platform)
}
