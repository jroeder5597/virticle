package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"virticle/internal/catalog"
	"virticle/internal/hostconn"
)

// Image catalogue downloads. The node fetches everything itself (curl) and
// computes the digest itself (sha256sum / sha512sum); the controller only
// sends commands and compares two hex strings. URLs come from the catalogue,
// never from the request.
//
// What the checksum is worth: it is fetched from the same project (usually the
// same server) as the image. It detects a corrupted or truncated download. It
// does NOT protect against a compromised mirror or origin, which can serve a
// matching image and checksum. Where a distro publishes a GPG-signed checksum
// file (Debian SHA512SUMS.sign, Ubuntu SHA256SUMS.gpg, Fedora's clear-signed
// CHECKSUM) verifying that signature would close the gap; that is a possible
// later improvement and is not implemented.

// catalogue returns the current image list (built-in plus <data>/images.json).
func (s *Server) catalogue() *catalog.Source {
	s.catMu.Lock()
	defer s.catMu.Unlock()
	if s.cat == nil {
		s.cat = &catalog.Source{DataDir: s.DataDir, Logf: log.Printf}
	}
	return s.cat
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"images": s.catalogue().Images()})
}

type imageState struct {
	catalog.Image
	Present     bool `json:"present"`     // a volume with the suggested name exists in the pool
	Downloading bool `json:"downloading"` // a catalogue download into this pool is running
}

// listPoolImages is the catalogue with a mark on the images already in the pool.
func (s *Server) listPoolImages(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	h.StoragePoolRefresh(pool, 0) // pick up files placed outside libvirt; best effort
	have := map[string]bool{}
	if vols, _, err := h.StoragePoolListAllVolumes(pool, 1, 0); err == nil {
		for _, v := range vols {
			have[v.Name] = true
		}
	}
	nodeID := r.PathValue("node")
	out := []imageState{}
	for _, i := range s.catalogue().Images() {
		out = append(out, imageState{Image: i, Present: have[i.VolumeName], Downloading: imageRuns.get(runKey(nodeID, pool.Name, i.VolumeName)) != nil})
	}
	_, dirErr := poolDirectory(h, pool)
	resp := map[string]any{"images": out, "downloadable": dirErr == nil}
	if dirErr != nil {
		resp["reason"] = dirErr.Error()
	}
	// Names being downloaded under a custom volume name.
	resp["running"] = imageRuns.names(nodeID, pool.Name)
	writeJSON(w, resp)
}

// --- running downloads ---

type imageRun struct {
	mu        sync.Mutex
	cancelled bool
	pidFile   string // on the node; holds the pid of the running curl / sha*sum
	h         *hostconn.Host
}

type runTable struct {
	mu sync.Mutex
	m  map[string]*imageRun
}

var imageRuns = &runTable{m: map[string]*imageRun{}}

func runKey(node, pool, vol string) string { return node + "\x00" + pool + "\x00" + vol }

func (t *runTable) get(k string) *imageRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[k]
}

func (t *runTable) add(k string, r *imageRun) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m[k] != nil {
		return false
	}
	t.m[k] = r
	return true
}

func (t *runTable) remove(k string) {
	t.mu.Lock()
	delete(t.m, k)
	t.mu.Unlock()
}

func (t *runTable) names(node, pool string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []string{}
	prefix := node + "\x00" + pool + "\x00"
	for k := range t.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	sort.Strings(out)
	return out
}

var errImageCancelled = errors.New("cancelled")

func (r *imageRun) isCancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cancelled
}

// runTracked runs a script on the node in which one background process is
// started with `& echo $! > $PIDFILE; wait $!`, so that cancel can kill it.
func (r *imageRun) runTracked(script string) (string, error) {
	if r.isCancelled() {
		return "", errImageCancelled
	}
	out, err := r.h.RunRoot(script)
	if r.isCancelled() {
		return out, errImageCancelled
	}
	return out, err
}

// cancel kills the process the current step started. If the step has not
// written its pid file yet it tries again for a moment; the flag stops the
// next step from starting in any case.
func (r *imageRun) cancel() {
	r.mu.Lock()
	r.cancelled = true
	pf := r.pidFile
	r.mu.Unlock()
	for i := 0; i < 8; i++ {
		if _, err := r.h.RunRoot("kill $(cat -- " + shQuote(pf) + ") 2>/dev/null"); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// digestMismatch is the message for a download whose digest differs.
func digestMismatch(algorithm, want, got string) error {
	return fmt.Errorf("checksum mismatch: expected %s %s, the downloaded file has %s; the file was deleted", algorithm, want, got)
}

func compareDigest(algorithm, want, got string) error {
	if !strings.EqualFold(want, got) {
		return digestMismatch(algorithm, want, got)
	}
	return nil
}

func randHex() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// curlOpts are the options for every fetch. -f: an HTTP error status is a
// failure, not a saved error page.
const curlOpts = "curl -q -fsSL --proto '=http,https' --proto-redir '=http,https' --max-redirs 10 --connect-timeout 20"

func (s *Server) downloadImage(w http.ResponseWriter, r *http.Request) {
	h, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	img, ok := s.catalogue().Find(req.ID)
	if !ok {
		fail(w, badRequest("unknown catalogue image "+req.ID))
		return
	}
	name := req.Name
	if name == "" {
		name = img.VolumeName
	}
	if !volNameRE.MatchString(name) {
		fail(w, badRequest("volume name must start with a letter or digit and contain only letters, digits, dot, dash, plus and underscore"))
		return
	}
	dir, err := poolDirectory(h, pool)
	if err != nil {
		fail(w, err)
		return
	}
	if _, err := h.StorageVolLookupByName(pool, name); err == nil {
		fail(w, badRequest("a volume named "+name+" already exists in this pool"))
		return
	}
	node := r.PathValue("node")
	key := runKey(node, pool.Name, name)
	run := &imageRun{h: h, pidFile: "/tmp/virticle-img-" + randHex() + ".pid"}
	if !imageRuns.add(key, run) {
		fail(w, badRequest("this image is already being downloaded as "+name))
		return
	}
	dest := dir + "/" + name
	// A dot file: libvirt's directory pools do not list it, so a half-written
	// image never shows up as a volume (it cannot clash with a real volume
	// either, those names start with a letter or digit).
	part := dir + "/." + name + ".part"

	// A leftover or foreign file with either name stops us before anything is written.
	if out, err := h.RunRoot(fmt.Sprintf("if [ -e %[1]s ] || [ -e %[2]s ]; then echo 'a file named %[3]s (or .%[3]s.part) already exists in the pool directory'; exit 1; fi", shQuote(dest), shQuote(part), name)); err != nil {
		imageRuns.remove(key)
		if out == "" {
			out = err.Error()
		}
		fail(w, badRequest(out))
		return
	}

	// The download takes minutes: answer now, the task log shows progress and the result.
	go func() {
		defer imageRuns.remove(key)
		err := s.task(r, "image", pool.Name+"/"+name, h.Name, func(progress func(float64)) error {
			err := s.fetchImage(run, img, pool.Name, part, dest, progress)
			if err != nil && !errors.Is(err, errImageCancelled) {
				return err
			}
			if err != nil {
				return errors.New("cancelled by the user; the partial file was deleted")
			}
			return h.StoragePoolRefresh(pool, 0)
		})
		if err != nil {
			log.Printf("image %s into %s/%s on %s: %v", img.ID, pool.Name, name, h.Name, err)
		}
	}()
	writeJSON(w, map[string]any{"ok": true, "volume": name})
}

// fetchImage does the work on the node: checksum file, download to a .part
// name, hash, compare, and only then move into place. Whatever happens, the
// .part file is gone afterwards unless it was moved.
func (s *Server) fetchImage(run *imageRun, img catalog.Image, pool, part, dest string, progress func(float64)) (err error) {
	defer func() {
		if _, rmErr := run.h.RunRoot("rm -f -- " + shQuote(part)); rmErr != nil && err != nil {
			err = fmt.Errorf("%w (and the partial file %s could not be removed: %v)", err, part, rmErr)
		}
		run.h.RunRoot("rm -f -- " + shQuote(run.pidFile))
	}()

	want := strings.ToLower(img.Checksum)
	if want == "" {
		out, err := run.runTracked(fmt.Sprintf("t=$(mktemp) || exit 1; if %s --max-time 60 -o \"$t\" -- %s; then head -c 2000000 \"$t\"; rc=0; else rc=$?; fi; rm -f \"$t\"; exit $rc",
			curlOpts, shQuote(img.ChecksumURL)))
		if err != nil {
			if errors.Is(err, errImageCancelled) {
				return err
			}
			return fmt.Errorf("could not fetch the checksum file %s: %s", img.ChecksumURL, orErr(out, err))
		}
		if want, err = catalog.ParseChecksum(out, img.FileName(), img.Algorithm); err != nil {
			return fmt.Errorf("%w (checksum file %s)", err, img.ChecksumURL)
		}
	}

	defer watchPartSize(run.h, part, firstPositive(probeSize(run.h, img.URL), img.SizeBytes), progress)()
	script := fmt.Sprintf("umask 077; %s -o %s -- %s & echo $! > %s; wait $!",
		curlOpts, shQuote(part), shQuote(img.URL), shQuote(run.pidFile))
	if out, err := run.runTracked(script); err != nil {
		if errors.Is(err, errImageCancelled) {
			return err
		}
		return fmt.Errorf("download failed: %s", orErr(out, err))
	}

	sum := img.Algorithm + "sum"
	out, err := run.runTracked(fmt.Sprintf("%s -- %s & echo $! > %s; wait $!", sum, shQuote(part), shQuote(run.pidFile)))
	if err != nil {
		if errors.Is(err, errImageCancelled) {
			return err
		}
		return fmt.Errorf("computing the %s on the node failed: %s", img.Algorithm, orErr(out, err))
	}
	got, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	if len(got) != catalog.HashLen(img.Algorithm) {
		return fmt.Errorf("unexpected output from %s: %q", sum, out)
	}
	if err := compareDigest(img.Algorithm, want, got); err != nil {
		return err
	}
	if run.isCancelled() {
		return errImageCancelled
	}
	// mv -n never overwrites; if the name appeared meanwhile it exits 0 and
	// leaves the source behind, which is what the check after it detects.
	if out, err := run.h.RunRoot(fmt.Sprintf("mv -n -- %[1]s %[2]s && if [ -e %[1]s ]; then echo 'a file with this name appeared in the pool directory; nothing was overwritten'; exit 1; fi", shQuote(part), shQuote(dest))); err != nil {
		return fmt.Errorf("could not move the image into place: %s", orErr(out, err))
	}
	return nil
}

func orErr(out string, err error) string {
	if out != "" {
		return out
	}
	return err.Error()
}

func firstPositive(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// cancelImage stops a running catalogue download; the task ends as failed
// with "cancelled" and the partial file is removed.
func (s *Server) cancelImage(w http.ResponseWriter, r *http.Request) {
	_, pool, err := s.pool(r)
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	run := imageRuns.get(runKey(r.PathValue("node"), pool.Name, req.Name))
	if run == nil {
		fail(w, badRequest("no catalogue download of "+req.Name+" is running in this pool"))
		return
	}
	run.cancel()
	writeJSON(w, map[string]bool{"ok": true})
}
