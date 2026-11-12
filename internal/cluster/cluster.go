// Package cluster keeps one live connection per node, reconnects when a node
// drops, and fans libvirt events out to subscribers.
package cluster

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"virticle/internal/hostconn"
	"virticle/internal/store"
)

// Event is pushed to every subscriber (the web UI's WebSocket).
type Event struct {
	Type   string `json:"type"`             // node | domain | network | pool | task
	Node   int64  `json:"node,omitempty"`   // node id
	Name   string `json:"name,omitempty"`   // domain, network or pool name
	UUID   string `json:"uuid,omitempty"`   // domain uuid
	Detail string `json:"detail,omitempty"` // lifecycle event name, node state
	Task   any    `json:"task,omitempty"`
}

var lifecycle = []string{"defined", "undefined", "started", "suspended", "resumed", "stopped", "shutdown", "pmsuspended", "crashed"}

// NodeStatus is the externally visible state of one node.
type NodeStatus struct {
	store.Node
	State    string `json:"state"` // connecting | online | offline
	Error    string `json:"error,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// What the node itself reported on the last connection: how its login
	// user gets root (privilege.go), in words, and the privilege that was
	// detected, which can differ from the configured one.
	DetectedPrivilege string `json:"detectedPrivilege,omitempty"`
	PrivilegeNote     string `json:"privilegeNote,omitempty"`
}

// Persister stores what a connection learns about a node.
type Persister interface {
	SetNodePrivilege(id int64, p string) error
	SetNodeHostUUID(id int64, u string) error
}

type node struct {
	cfg    store.Node
	cancel context.CancelFunc

	mu       sync.Mutex
	host     *hostconn.Host
	state    string
	err      string
	hostname string
	detected string
	privNote string
}

type Manager struct {
	// Persist, when set, receives the privilege and host UUID found on the
	// first connection of a node.
	Persist Persister

	keyFile, knownHosts   string
	wdInterval, wdTimeout time.Duration // connection watchdog, see watchdog.go

	mu    sync.Mutex
	nodes map[int64]*node
	subs  map[chan Event]struct{}
}

func NewManager(keyFile, knownHosts string) *Manager {
	return &Manager{keyFile: keyFile, knownHosts: knownHosts, wdInterval: watchInterval, wdTimeout: watchTimeout, nodes: map[int64]*node{}, subs: map[chan Event]struct{}{}}
}

// Target builds the SSH target for a node record. A jump host authenticates
// with the controller key, or with VIRTICLE_JUMP_PASSWORD if that is set.
func (m *Manager) Target(n store.Node) (hostconn.Target, error) {
	t := hostconn.Target{Addr: n.Addr, User: n.User, KeyFile: m.keyFile}
	if n.Jump != "" {
		u, a, ok := strings.Cut(n.Jump, "@")
		if !ok {
			return t, fmt.Errorf("jump host must be user@address")
		}
		t.Jump = &hostconn.Target{Addr: a, User: u, KeyFile: m.keyFile, Password: os.Getenv("VIRTICLE_JUMP_PASSWORD")}
	}
	return t, nil
}

// Add starts managing a node. It returns immediately; the connection is
// established and kept alive in the background.
func (m *Manager) Add(cfg store.Node) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{cfg: cfg, cancel: cancel, state: "connecting"}
	m.mu.Lock()
	m.nodes[cfg.ID] = n
	m.mu.Unlock()
	go m.run(ctx, n)
}

// Replace swaps a node's configuration: the old connection is torn down and a
// new one is made from cfg. The node keeps its id, so everything keyed to it
// stays.
func (m *Manager) Replace(cfg store.Node) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{cfg: cfg, cancel: cancel, state: "connecting"}
	m.mu.Lock()
	old := m.nodes[cfg.ID]
	m.nodes[cfg.ID] = n
	m.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	go m.run(ctx, n)
	m.Publish(Event{Type: "node", Node: cfg.ID, Detail: "connecting"})
}

// Config returns the configuration the manager currently runs a node with.
func (m *Manager) Config(id int64) (store.Node, bool) {
	m.mu.Lock()
	n := m.nodes[id]
	m.mu.Unlock()
	if n == nil {
		return store.Node{}, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg, true
}

// ConfigOfHost finds the configuration of the node a live connection belongs to.
func (m *Manager) ConfigOfHost(h *hostconn.Host) (store.Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.nodes {
		n.mu.Lock()
		same := n.host == h
		cfg := n.cfg
		n.mu.Unlock()
		if same {
			return cfg, true
		}
	}
	return store.Node{}, false
}

// Reconnect drops a node's connection and makes a new one with the same
// configuration.
func (m *Manager) Reconnect(id int64) {
	if cfg, ok := m.Config(id); ok {
		m.Replace(cfg)
	}
}

func (m *Manager) Remove(id int64) {
	m.mu.Lock()
	n := m.nodes[id]
	delete(m.nodes, id)
	m.mu.Unlock()
	if n != nil {
		n.cancel()
	}
}

// SetMaintenance updates the in-memory copy of a node's maintenance flag
// (the persistent copy is the store's).
func (m *Manager) SetMaintenance(id int64, on bool) {
	m.mu.Lock()
	n := m.nodes[id]
	m.mu.Unlock()
	if n != nil {
		n.mu.Lock()
		n.cfg.Maintenance = on
		n.mu.Unlock()
	}
}

func (m *Manager) run(ctx context.Context, n *node) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := m.session(ctx, n)
		if ctx.Err() != nil {
			return
		}
		msg, state := "connection lost", "offline"
		if err != nil {
			msg = err.Error()
		} else {
			backoff = time.Second
		}
		if errors.Is(err, errWedged) {
			// The transport was fine, so retry at once rather than backing off.
			state, backoff = "connecting", time.Second
			msg += ", reconnecting"
			log.Printf("node %s: %s", n.cfg.Addr, msg)
		}
		n.set(nil, state, msg)
		m.Publish(Event{Type: "node", Node: n.cfg.ID, Detail: state})
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session holds one connection until it drops. A nil return means it was
// established and later lost; an error means it could not be established.
func (m *Manager) session(ctx context.Context, n *node) error {
	t, err := m.Target(n.cfg)
	if err != nil {
		return err
	}
	h, err := hostconn.Dial(t, m.knownHosts)
	if err != nil {
		return err
	}
	defer h.Close()
	if name, err := h.ConnectGetHostname(); err == nil {
		h.Name = name
	}
	n.mu.Lock()
	cfg := n.cfg
	n.mu.Unlock()
	// A stored value that is not a current setting (empty, or "sudo" from an
	// older version) is detected again and stored; so is "none",
	// so that preparing a node takes effect without editing it.
	priv := hostconn.ParsePrivilege(cfg.Privilege)
	if priv == hostconn.PrivNone {
		priv = ""
	}
	if det, note, err := h.DetectPrivilege(); err == nil {
		n.mu.Lock()
		n.detected, n.privNote = string(det), note
		n.mu.Unlock()
		if !priv.Valid() {
			priv = det
			n.mu.Lock()
			n.cfg.Privilege = string(det)
			n.mu.Unlock()
			// "none" is not stored: a node that is not prepared yet is
			// detected again on its next connection, so preparing it takes
			// effect without anyone editing the node.
			if m.Persist != nil && det != hostconn.PrivNone {
				if err := m.Persist.SetNodePrivilege(cfg.ID, string(det)); err != nil {
					log.Printf("node %s: storing the detected privilege: %v", cfg.Addr, err)
				}
			}
		} else if det == hostconn.PrivNone {
			// The stored setting says the node command is there but it does
			// not answer: nothing privileged can run.
			priv = hostconn.PrivNone
		}
	}
	if !priv.Valid() {
		priv = hostconn.PrivNone
	}
	h.SetPrivilege(priv)
	if u, err := h.HostUUID(); err == nil && u != cfg.HostUUID {
		n.mu.Lock()
		n.cfg.HostUUID = u
		n.mu.Unlock()
		if m.Persist != nil {
			if err := m.Persist.SetNodeHostUUID(cfg.ID, u); err != nil {
				log.Printf("node %s: storing the host uuid: %v", cfg.Addr, err)
			}
		}
	}
	evctx, stop := context.WithCancel(ctx)
	defer stop()
	events, err := h.LifecycleEvents(evctx)
	if err != nil {
		return fmt.Errorf("subscribe to events: %w", err)
	}
	n.mu.Lock()
	n.hostname = h.Name
	n.mu.Unlock()
	go m.objectEvents(evctx, n.cfg.ID, h)
	wedged := make(chan error, 1)
	go func() {
		wedged <- watch(evctx, func() error { _, err := h.ConnectGetLibVersion(); return err }, m.wdInterval, m.wdTimeout)
	}()
	n.set(h, "online", "")
	m.Publish(Event{Type: "node", Node: n.cfg.ID, Detail: "online"})

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-h.Disconnected():
			return nil
		case err := <-wedged:
			if err != nil && h.IsConnected() {
				return err
			}
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			detail := fmt.Sprint(ev.Event)
			if int(ev.Event) < len(lifecycle) {
				detail = lifecycle[ev.Event]
			}
			m.Publish(Event{Type: "domain", Node: n.cfg.ID, Name: ev.Dom.Name, UUID: FormatUUID(ev.Dom.UUID), Detail: detail})
		}
	}
}

func (n *node) set(h *hostconn.Host, state, err string) {
	n.mu.Lock()
	n.host, n.state, n.err = h, state, err
	n.mu.Unlock()
}

var ErrOffline = errors.New("node is offline")

// Host returns the live connection to a node.
func (m *Manager) Host(id int64) (*hostconn.Host, error) {
	m.mu.Lock()
	n := m.nodes[id]
	m.mu.Unlock()
	if n == nil {
		return nil, store.ErrNotFound
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.host == nil {
		if n.err != "" {
			return nil, fmt.Errorf("%w: %s", ErrOffline, n.err)
		}
		return nil, ErrOffline
	}
	return n.host, nil
}

// Status lists every node with its connection state, ordered by id.
func (m *Manager) Status() []NodeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NodeStatus, 0, len(m.nodes))
	for _, n := range m.nodes {
		n.mu.Lock()
		out = append(out, NodeStatus{Node: n.cfg, State: n.state, Error: n.err, Hostname: n.hostname, DetectedPrivilege: n.detected, PrivilegeNote: n.privNote})
		n.mu.Unlock()
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Subscribe returns a channel of events and a function to stop receiving.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.subs, ch)
		m.mu.Unlock()
	}
}

// Publish delivers an event to all subscribers; a subscriber that is not
// keeping up misses the event rather than blocking everyone else.
func (m *Manager) Publish(e Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// FormatUUID renders libvirt's 16 raw bytes in the canonical dashed form.
func FormatUUID(u [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// ParseUUID is the inverse of FormatUUID.
func ParseUUID(s string) (u [16]byte, err error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(b) != 16 {
		return u, fmt.Errorf("invalid UUID %q", s)
	}
	copy(u[:], b)
	return u, nil
}
