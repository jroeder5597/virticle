package nodecmd

import (
	"fmt"
	"regexp"
	"sort"

	"golang.org/x/sys/unix"
)

// work-ls POOLDIR: "NAME|MTIME_SEC" of every replication work directory (.virticle-rp.XXXXXX, a real directory,
// not a link) directly in a pool directory. The controller uses it to find the directories that a controller
// which was stopped in the middle of a run left behind: it knows which of them belong to a replica.

func init() {
	Register("work-ls", verbWorkLs)
}

var replWorkRE = regexp.MustCompile(`^\.virticle-rp\.[A-Za-z0-9]{4,16}$`)

func verbWorkLs(c *Ctx) error {
	if err := c.Need(1, 1, "work-ls POOLDIR"); err != nil {
		return err
	}
	r, fd, err := openDirArg(c, c.Args[0])
	if err != nil {
		return err
	}
	defer r.Close()
	defer unix.Close(fd)
	names, _ := listDir(fd)
	sort.Strings(names)
	for _, n := range names {
		if !replWorkRE.MatchString(n) {
			continue
		}
		st, err := fstatat(fd, n)
		if err != nil || !isDirMode(st.Mode) {
			continue
		}
		fmt.Fprintf(c.Stdout, "%s|%d\n", n, st.Mtim.Sec)
	}
	return nil
}
