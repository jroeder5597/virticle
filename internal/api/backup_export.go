package api

// Export and import of a backup as one file: an uncompressed tar of the
// backup directory. The tar is made on the node and streamed through this
// server to the browser; an uploaded tar is vetted on the way through (see
// vetTar) and unpacked on the node into a work directory inside a pool, so
// nothing is held in memory or on the controller.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"virticle/internal/hostconn"
)

// streamOut sends the first write's headers lazily, so a failure before any
// data can still be reported as an ordinary error.
type streamOut struct {
	w        http.ResponseWriter
	filename string
	total    uint64
	n        atomic.Uint64
	progress func(float64)
	started  bool
}

func (o *streamOut) Write(p []byte) (int, error) {
	if !o.started {
		o.started = true
		h := o.w.Header()
		h.Set("Content-Type", "application/x-tar")
		h.Set("Content-Disposition", `attachment; filename="`+o.filename+`"`)
		o.w.WriteHeader(http.StatusOK)
	}
	n, err := o.w.Write(p)
	done := o.n.Add(uint64(n))
	if o.total > 0 && o.progress != nil {
		o.progress(float64(done) / float64(o.total))
	}
	return n, err
}

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.backupFromURL(r)
	if err != nil {
		fail(w, err)
		return
	}
	m, err := b.load()
	if err != nil {
		fail(w, err)
		return
	}
	var total uint64
	names := make([]string, 0, len(m.files()))
	for _, f := range m.files() {
		names = append(names, shq(f))
	}
	for _, d := range m.Disks {
		total += d.Bytes
	}
	// Explicit plain member names, so the archive can be vetted on import.
	cmd := "cd " + shq(b.dir) + " && tar -cf - --format=posix -- " + strings.Join(names, " ")
	out := &streamOut{w: w, filename: m.VM + ".vmexport.tar", total: total}
	err = s.task(r, "backup export", b.name, b.h.Name, func(progress func(float64)) error {
		out.progress = progress
		return b.h.Pipe(r.Context(), cmd, nil, out)
	})
	if err != nil {
		if !out.started {
			fail(w, err)
		} else {
			logf("export of %s failed after %d bytes: %v", b.dir, out.n.Load(), err)
		}
	}
}

// importBackup stores an uploaded export in a pool as a backup directory.
// Restoring from it is a separate step.
func (s *Server) importBackup(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.host(r)
	if err != nil {
		fail(w, err)
		return
	}
	_, pd, err := destPool(h, r.URL.Query().Get("pool"))
	if err != nil {
		fail(w, err)
		return
	}
	if !safePathRE.MatchString(pd.Dir) {
		fail(w, badRequest("unsupported characters in the pool directory "+pd.Dir))
		return
	}
	free, err := freeBytes(h, pd.Dir)
	if err != nil {
		fail(w, err)
		return
	}
	budget := free - importFreeMargin
	if budget <= 0 || (r.ContentLength > 0 && r.ContentLength > budget) {
		fail(w, badRequest(fmt.Sprintf("not enough free space in %s: %d MiB available, the upload is %d MiB", pd.Dir, free>>20, r.ContentLength>>20)))
		return
	}
	var res map[string]any
	err = s.task(r, "backup import", r.URL.Query().Get("pool"), h.Name, func(progress func(float64)) (rerr error) {
		wd, err := newWork(h, pd.Dir, "imp")
		if err != nil {
			return err
		}
		defer func() {
			if rerr != nil {
				if cerr := wd.remove(); cerr != nil {
					rerr = withNote(rerr, "work directory left behind: "+wd.path)
				}
			}
		}()
		pr, pw := io.Pipe()
		type vetResult struct {
			names []string
			err   error
		}
		vetCh := make(chan vetResult, 1)
		go func() {
			names, err := vetTar(r.Body, pw, budget)
			pw.CloseWithError(err)
			vetCh <- vetResult{names, err}
		}()
		perr := h.Pipe(r.Context(), "umask 077; cd "+shq(wd.path)+" && tar --no-same-owner --no-same-permissions -xf -", pr, nil)
		pr.CloseWithError(errors.New("extraction ended"))
		v := <-vetCh
		var br badRequest
		switch {
		case errors.As(v.err, &br):
			return v.err
		case perr != nil:
			return fmt.Errorf("unpacking the archive on %s: %w", h.Name, perr)
		case v.err != nil:
			return v.err
		}
		progress(0.8)
		return finishImport(h, pd, wd, v.names, &res)
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, res)
}

// finishImport checks the unpacked directory like a backup and moves it to
// its final name.
func finishImport(h *hostconn.Host, pd poolDir, wd *workDir, got []string, res *map[string]any) error {
	ref := &backupRef{h: h, pd: pd, dir: wd.path}
	m, err := ref.load()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, f := range m.files() {
		want[f] = true
	}
	for _, f := range got {
		if !want[f] {
			return badRequest("refusing archive: " + quoteTag(f) + " is not part of a backup (the manifest does not list it)")
		}
		delete(want, f)
	}
	if len(want) > 0 {
		return badRequest("the archive is not a complete backup: files missing")
	}
	if x, err := readLimited(h, wd.path+"/domain.xml", backupMaxXML); err != nil {
		return badRequest(err.Error())
	} else if _, err := parseMigDomain(string(x)); err != nil {
		return badRequest("domain.xml is not a valid domain definition: " + err.Error())
	}
	for _, d := range m.Disks {
		info, err := qemuInfo(h, wd.path+"/"+d.File)
		if err != nil {
			return fmt.Errorf("%s: %w", d.File, err)
		}
		if info.Format != "qcow2" {
			return badRequest(d.File + " is not a qcow2 image")
		}
	}
	name := backupDirName(m.VM, m.createdTime())
	final := pd.Dir + "/" + name
	if out, err := h.Run("test ! -e " + shq(final) + " && chmod 700 -- " + shq(wd.path) + " && mv -T -- " + shq(wd.path) + " " + shq(final)); err != nil {
		return badRequest("a backup named " + name + " already exists in this pool, or it cannot be moved into place: " + impFirstLine(out))
	}
	*res = map[string]any{"pool": pd.Name, "dir": name, "vm": m.VM, "uuid": m.UUID}
	return nil
}
