package api

import (
	"context"
	"encoding/xml"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"virt-proj/internal/hostconn"
)

// events streams node, domain and task events to the UI.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, wsOptions(r))
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := c.CloseRead(r.Context())
	ch, stop := s.Nodes.Subscribe()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			if err := wsjson.Write(ctx, c, ev); err != nil {
				return
			}
		}
	}
}

// vncEndpoint finds where a running VM's VNC server listens on its host.
func vncEndpoint(h *hostconn.Host, liveXML string) (network, addr string, err error) {
	var x domainXML
	if err := xml.Unmarshal([]byte(liveXML), &x); err != nil {
		return "", "", err
	}
	for _, g := range x.Devices.Graphics {
		if g.Type != "vnc" {
			continue
		}
		if g.Socket != "" {
			return "unix", g.Socket, nil
		}
		if g.Port <= 0 {
			return "", "", badRequest("the VM is not running")
		}
		host := g.Listen
		if host == "" || host == "0.0.0.0" {
			host = "127.0.0.1"
		} else if host == "::" {
			host = "::1"
		}
		return "tcp", net.JoinHostPort(host, strconv.Itoa(g.Port)), nil
	}
	return "", "", badRequest("this VM has no VNC display")
}

// vmVNC bridges a WebSocket (noVNC in the browser) to the VM's VNC server.
// The connection to the VNC port is made from the node itself, through the
// existing SSH connection, so VNC can stay bound to the node's loopback.
func (s *Server) vmVNC(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	raw, err := h.DomainGetXMLDesc(dom, 0)
	if err != nil {
		fail(w, err)
		return
	}
	network, addr, err := vncEndpoint(h, raw)
	if err != nil {
		fail(w, err)
		return
	}
	backend, err := h.DialNet(network, addr)
	if err != nil {
		fail(w, err)
		return
	}
	defer backend.Close()
	c, err := websocket.Accept(w, r, wsOptions(r, "binary"))
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(-1)
	bridge(r.Context(), c, backend)
}

// bridge copies bytes both ways until either side closes.
func bridge(ctx context.Context, c *websocket.Conn, backend io.ReadWriteCloser) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	go func() {
		io.Copy(backend, ws)
		cancel()
		backend.Close()
	}()
	io.Copy(ws, backend)
	c.Close(websocket.StatusNormalClosure, "")
}

type wsWriter struct {
	ctx context.Context
	c   *websocket.Conn
}

func (w wsWriter) Write(p []byte) (int, error) {
	if err := w.c.Write(w.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// vmSerial bridges a WebSocket (xterm.js) to the VM's first serial console
// using libvirt's console stream.
func (s *Server) vmSerial(w http.ResponseWriter, r *http.Request) {
	h, dom, err := s.domain(r)
	if err != nil {
		fail(w, err)
		return
	}
	state, _, _, _, _, err := h.DomainGetInfo(dom)
	if err != nil {
		fail(w, err)
		return
	}
	if stateName(state) != "running" && stateName(state) != "paused" {
		fail(w, badRequest("the VM is not running"))
		return
	}
	c, err := websocket.Accept(w, r, wsOptions(r))
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Browser keystrokes -> pipe -> libvirt stream.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if _, err := pw.Write(data); err != nil {
				return
			}
		}
	}()
	// Blocks until the console closes or the browser goes away.
	err = h.DomainOpenConsoleBidirectional(dom, nil, pr, wsWriter{ctx, c}, 0)
	pr.Close()
	if err != nil && ctx.Err() == nil {
		c.Close(websocket.StatusInternalError, truncate(err.Error(), 120))
		return
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
