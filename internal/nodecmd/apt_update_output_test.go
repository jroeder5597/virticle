package nodecmd

import (
	"strings"
	"testing"
)

// A flat repository without Translation files makes apt print "Err:" for each compression it tries and then
// "Ign:" for the same download; apt-get update exits with 0 and nothing is wrong (seen on Debian 13).
const aptIgnoredErr = `Get:1 file:/srv/dv-repo ./ InRelease
Ign:1 file:/srv/dv-repo ./ InRelease
Get:2 file:/srv/dv-repo ./ Packages
Err:10 file:/srv/dv-repo ./ Translation-en
  File not found - /srv/dv-repo/./en.bz2 (2: No such file or directory)
Err:10 file:/srv/dv-repo ./ Translation-en
  File not found - /srv/dv-repo/./en.gz (2: No such file or directory)
Ign:10 file:/srv/dv-repo ./ Translation-en
Hit:4 https://deb.debian.org/debian trixie InRelease
Reading package lists...`

const aptRealErr = `Ign:1 http://127.0.0.1:9/dv-broken stable InRelease
Err:1 http://127.0.0.1:9/dv-broken stable InRelease
  Unable to connect to 127.0.0.1:9:
Reading package lists...
W: Failed to fetch http://127.0.0.1:9/dv-broken/dists/stable/InRelease  Unable to connect to 127.0.0.1:9:
W: Some index files failed to download. They have been ignored, or old ones used instead.`

func TestAptUpdateOutput(t *testing.T) {
	if aptUpdateFailed(aptIgnoredErr) {
		t.Error("an Err line for a download apt then ignored counts as a failure")
	}
	if !aptUpdateFailed(aptRealErr) {
		t.Error("an unreachable repository is not a failure")
	}
	if got := aptFailure(aptRealErr); !strings.Contains(got, "Err:1 http://127.0.0.1:9/dv-broken stable InRelease") {
		t.Errorf("the message lacks the Err line: %q", got)
	}
	// the same Err line three times is shown once
	rep := strings.Repeat("Err:3 http://x/ stable InRelease\n", 3)
	if got := aptFailure(rep); strings.Count(got, "Err:3") != 1 {
		t.Errorf("repeated lines: %q", got)
	}
}
