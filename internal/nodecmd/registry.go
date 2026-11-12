package nodecmd

import (
	"fmt"
	"sort"
)

var verbs = map[string]func(*Ctx) error{}

// Register adds a verb. Each verb group calls it from an init function in its
// own file. A name registered twice is a programming error and panics.
func Register(name string, fn func(*Ctx) error) {
	if _, dup := verbs[name]; dup {
		panic(fmt.Sprintf("nodecmd: verb %q registered twice", name))
	}
	verbs[name] = fn
}

// Verbs lists the registered verb names, sorted.
func Verbs() []string {
	names := make([]string, 0, len(verbs))
	for n := range verbs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() {
	Register("version", func(c *Ctx) error {
		if err := c.Need(0, 0, "version"); err != nil {
			return err
		}
		fmt.Fprintln(c.Stdout, Version)
		return nil
	})
}
