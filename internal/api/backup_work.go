package api

// Node-side helpers of the backup code. They call the typed operations of
// internal/hostconn, so they work for root, broad sudo and the scoped helper
// account alike.

import (
	"fmt"
	"strings"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// bkWork is a private scratch directory inside a pool directory, so the final
// rename never crosses a file system.
type bkWork struct {
	h    *hostconn.Host
	path string
	// stop, when set, is polled by convert; once it returns true convert gives
	// up (the caller then calls remove).
	stop func() bool
}

func newWork(h *hostconn.Host, dir, tag string) (*bkWork, error) {
	p, err := h.MkWorkDir(dir, tag)
	if err != nil {
		return nil, err
	}
	return &bkWork{h: h, path: p}, nil
}

func (w *bkWork) file(n string) string { return w.path + "/" + n }

// remove kills a conversion still running from this directory and deletes it.
func (w *bkWork) remove() error { return w.h.RemoveWork(w.path) }

func (w *bkWork) convert(src, srcFmt, dest string, progress func(float64)) error {
	return w.h.Convert(w.path, src, srcFmt, dest, w.stop, progress)
}

// readLimited returns a node file's content; larger files are refused.
func readLimited(h *hostconn.Host, file string, max int) ([]byte, error) {
	b, err := h.ReadLimited(file, max)
	if err != nil {
		return nil, fmt.Errorf("%s", impFirstLine(err.Error()))
	}
	return b, nil
}

// writeNodeFile creates a new file with data; an existing file is not replaced.
func writeNodeFile(h *hostconn.Host, file string, data []byte) error {
	return h.WriteFile(file, data, len(data), "600")
}

// bkQemuInfo is qemuInfo through the typed operation.
func bkQemuInfo(h *hostconn.Host, file string) (importImgInfo, error) {
	out, err := h.QemuInfo(file, false)
	if err != nil {
		return importImgInfo{}, badRequest("qemu-img could not read the image: " + impFirstLine(out))
	}
	return parseImgInfo(out, file)
}

func bkFreeBytes(h *hostconn.Host, dir string) (int64, error) { return h.FreeBytes(dir) }

// checkBackupTransfer prepares and checks how files get from src to dst for a
// backup or restore, and returns how src reaches dst. Root and sudo pairs are
// checked as before (checkSSHTrust); two helper-mode nodes get their peer
// trust set up (ensurePeerTrust) and send the data straight to each other.
func (s *Server) checkBackupTransfer(src, dst *hostconn.Host, n store.Node) (migEndpoint, error) {
	var c checkList
	if !anyHelper(src, dst) {
		s.checkSSHTrust(src, dst, n, &c)
		for _, k := range c {
			if !k.OK && k.Severity == "error" {
				return migEndpoint{}, badRequest(k.Message)
			}
		}
		ep, err := endpointFor(n)
		if err != nil {
			return ep, badRequest(err.Error())
		}
		return ep, nil
	}
	ep, err := endpointFor(n)
	if err != nil {
		return ep, badRequest(err.Error())
	}
	if !bothHelper(src, dst) {
		return ep, badRequest(fmt.Sprintf("files cannot be copied between %s and %s: one node uses the scoped helper account and the other does not. Prepare both nodes the same way", src.Name, dst.Name))
	}
	if _, err := src.Run(reachCmd(ep.Host, ep.port())); err != nil {
		return ep, badRequest(fmt.Sprintf("%s cannot open a TCP connection to %s port %s", src.Name, ep.Host, ep.port()))
	}
	if err := s.ensurePeerTrust(src, dst, n, ep); err != nil {
		return ep, badRequest(err.Error())
	}
	return ep, nil
}

// copyFile copies a file from one node to the other (directly between them).
func (m *bkRun) copyFile(src, dst string, progress func(done uint64)) error {
	return copyToNode(m.ctx, m.src, m.dst, m.ep, src, dst, "600", progress)
}

func missingTools(h *hostconn.Host, names ...string) error {
	miss, err := h.MissingTools(names...)
	if err != nil || len(miss) > 0 {
		return badRequest("missing on " + h.Name + ": " + strings.Join(miss, ", "))
	}
	return nil
}
