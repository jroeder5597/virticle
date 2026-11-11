package api

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"virticle/internal/snapsched"
)

// fakeMeta behaves like libvirt for one domain: one element per namespace,
// children returned with the namespace prefix.
type fakeMeta struct {
	els     map[string]string // ns -> element as libvirt returns it
	failOld bool              // removing the legacy element fails
	sets    []string
}

func (f *fakeMeta) get(ns string) (string, error) { return f.els[ns], nil }

func (f *fakeMeta) set(ns, prefix, value string) error {
	f.sets = append(f.sets, ns)
	if ns == snapsched.LegacyNS && f.failOld {
		return errors.New("boom")
	}
	if value == "" {
		delete(f.els, ns)
		return nil
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(value, "<meta>"), "</meta>")
	inner = regexp.MustCompile(`<(/?)([a-z])`).ReplaceAllString(inner, "<${1}"+prefix+":${2}")
	f.els[ns] = "<" + prefix + ":meta xmlns:" + prefix + `="` + ns + `">` + inner + "</" + prefix + ":meta>"
	return nil
}

const (
	oldXML = `<vp:meta xmlns:vp="https://virt-proj.invalid/ns/1"><vp:template sealed="linux"/><vp:tags><vp:tag>db</vp:tag></vp:tags>` +
		`<vp:snapshot-schedule enabled="yes" mode="interval" every-minutes="60" keep="3" prefix="auto" memory="no" quiesce="no"/></vp:meta>`
	newXML = `<virticle:meta xmlns:virticle="https://virticle.invalid/ns/1"><virticle:tags><virticle:tag>web</virticle:tag></virticle:tags></virticle:meta>`
)

func TestReadMeta(t *testing.T) {
	// old only: everything is still there
	got, err := readMeta(&fakeMeta{els: map[string]string{snapsched.LegacyNS: oldXML}})
	if err != nil || got != oldXML {
		t.Fatalf("old only: %q %v", got, err)
	}
	mark, tags := parseAppMeta(got)
	if !mark.Template || mark.Sealed != "linux" || len(tags) != 1 || tags[0] != "db" {
		t.Fatalf("old only parsed: %+v %v", mark, tags)
	}
	if sch, err := snapsched.Parse(got); err != nil || sch == nil || !sch.Enabled || sch.Keep != 3 {
		t.Fatalf("old schedule: %+v %v", sch, err)
	}

	// new only
	got, _ = readMeta(&fakeMeta{els: map[string]string{snapsched.NS: newXML}})
	if _, tags = parseAppMeta(got); len(tags) != 1 || tags[0] != "web" {
		t.Fatalf("new only: %v", tags)
	}

	// both: the new one wins and the old content is not mixed in
	got, _ = readMeta(&fakeMeta{els: map[string]string{snapsched.NS: newXML, snapsched.LegacyNS: oldXML}})
	mark, tags = parseAppMeta(got)
	if got != newXML || mark.Template || len(tags) != 1 || tags[0] != "web" {
		t.Fatalf("both: %q %+v %v", got, mark, tags)
	}

	// neither
	if got, err = readMeta(&fakeMeta{els: map[string]string{}}); got != "" || err != nil {
		t.Fatalf("neither: %q %v", got, err)
	}
}

// A write after reading the old element carries all three kinds of data into
// the new namespace and deletes the old element.
func TestWriteMetaMigratesOnWrite(t *testing.T) {
	f := &fakeMeta{els: map[string]string{snapsched.LegacyNS: oldXML}}
	old, _ := readMeta(f)
	meta, err := metaSetChild(old, "tags", tagsElem([]string{"db", "prod"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(f, meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.els[snapsched.LegacyNS]; ok {
		t.Fatal("old element still present")
	}
	got, _ := readMeta(f)
	if !strings.HasPrefix(got, "<virticle:meta") || strings.Contains(got, "vp:") {
		t.Fatalf("not the new namespace: %s", got)
	}
	mark, tags := parseAppMeta(got)
	if !mark.Template || mark.Sealed != "linux" || strings.Join(tags, ",") != "db,prod" {
		t.Fatalf("mark/tags lost: %+v %v", mark, tags)
	}
	if sch, err := snapsched.Parse(got); err != nil || sch == nil || sch.Keep != 3 || sch.Prefix != "auto" {
		t.Fatalf("schedule lost: %+v %v", sch, err)
	}

	// the scheduler's own writer goes through the same path
	upd, err := snapsched.Update(got, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(f, upd); err != nil {
		t.Fatal(err)
	}
	got, _ = readMeta(f)
	if sch, _ := snapsched.Parse(got); sch != nil {
		t.Fatal("schedule not removed")
	}
	if mark, tags = parseAppMeta(got); !mark.Template || len(tags) != 2 {
		t.Fatalf("other children lost: %+v %v", mark, tags)
	}
}

func TestWriteMetaRemovalFailureIsNotAnError(t *testing.T) {
	f := &fakeMeta{els: map[string]string{snapsched.LegacyNS: oldXML}, failOld: true}
	meta, _ := metaSetChild(oldXML, "tags", tagsElem([]string{"x"}))
	if err := writeMeta(f, meta); err != nil {
		t.Fatalf("surfaced: %v", err)
	}
	// both copies exist now; the new one wins on read
	got, _ := readMeta(f)
	if _, tags := parseAppMeta(got); len(tags) != 1 || tags[0] != "x" {
		t.Fatalf("new one does not win: %v", tags)
	}
}

func TestWriteMetaEmptyRemovesBoth(t *testing.T) {
	f := &fakeMeta{els: map[string]string{snapsched.LegacyNS: oldXML, snapsched.NS: newXML}}
	if err := writeMeta(f, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.els) != 0 {
		t.Fatalf("left behind: %v", f.els)
	}
}

func TestCloneStripsBothNamespaces(t *testing.T) {
	for _, meta := range []string{oldXML, newXML, newXML + "\n  " + oldXML} {
		src := "<domain type='kvm'>\n  <name>t</name>\n  <metadata>\n    " + meta + "\n  </metadata>\n</domain>\n"
		out, err := cloneDomainXML(src, cloneEdit{Name: "c"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "meta") || strings.Contains(out, "invalid/ns") {
			t.Fatalf("app metadata inherited:\n%s", out)
		}
	}
}

func TestBackupScheduleReadsBothPrefixes(t *testing.T) {
	for _, pfx := range []string{"vp", "virticle"} {
		meta := `<` + pfx + `:meta xmlns:` + pfx + `="x"><` + pfx + `:backup-schedule enabled="yes" mode="interval" every-minutes="60" target-node="1" pool="p" keep-last="2"/></` + pfx + `:meta>`
		b, err := parseBackupSchedule(meta)
		if err != nil || b == nil || !b.Enabled || b.KeepLast != 2 {
			t.Fatalf("%s: %+v %v", pfx, b, err)
		}
	}
}
