package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"virticle/internal/secretbox"
)

// Notifications: alerts are sent to the destinations listed on the
// Notifications page. Each destination has its own queue and goroutine, so a
// slow or broken one delays nothing else, and the alert cycle never waits.

const (
	notifyQueue   = 64
	notifyTimeout = 10 * time.Second
	// More than this many messages for one destination in one cycle become one summary.
	notifyBurst = 5
	// A Retry-After longer than this is not waited for in one go.
	maxRetryAfter = 5 * time.Minute
)

// notifyBackoff are the waits before the 2nd, 3rd and 4th try of a message.
var notifyBackoff = []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}

var destKinds = map[string]string{"slack": "Slack", "mattermost": "Mattermost", "discord": "Discord", "webhook": "Webhook"}

// notifyClock lets tests run the waits without waiting.
type notifyClock interface {
	Now() time.Time
	// Sleep waits d and reports false when ctx ended first.
	Sleep(ctx context.Context, d time.Duration) bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// notifyDest is a destination with its secrets in the clear, as the worker uses it.
type notifyDest struct {
	ID       string // the database id, or "flag" for the -alert-webhook flag
	Kind     string
	Name     string
	URL      string
	Severity string // warning: warnings and critical | critical
	Cleared  bool
	Secret   string
	ReadOnly bool
	Broken   string // set when the stored address could not be decrypted
}

// destStatus is what the page shows next to a destination.
type destStatus struct {
	Failing bool
	Error   string
	At      time.Time // of the last failure
	LastOK  time.Time
}

type destWorker struct {
	n      *notifier
	cancel context.CancelFunc
	queue  chan notifyMsg
	mu     sync.Mutex
	d      notifyDest
	st     destStatus
}

type notifier struct {
	mu      sync.Mutex
	workers map[string]*destWorker
	clock   notifyClock
	// load returns the configured destinations.
	load func() []notifyDest
	// testClient replaces the HTTP clients (tests only).
	testClient *http.Client
	clients    map[bool]*http.Client
}

func newNotifier(load func() []notifyDest) *notifier {
	return &notifier{workers: map[string]*destWorker{}, clock: realClock{}, load: load, clients: map[bool]*http.Client{}}
}

// sync makes the workers match the configured destinations.
func (n *notifier) sync() {
	list := n.load()
	n.mu.Lock()
	defer n.mu.Unlock()
	seen := map[string]bool{}
	for _, d := range list {
		seen[d.ID] = true
		w := n.workers[d.ID]
		if w == nil {
			ctx, cancel := context.WithCancel(context.Background())
			w = &destWorker{n: n, cancel: cancel, queue: make(chan notifyMsg, notifyQueue), d: d}
			n.workers[d.ID] = w
			go w.run(ctx)
			continue
		}
		w.mu.Lock()
		w.d = d
		w.mu.Unlock()
	}
	for id, w := range n.workers {
		if !seen[id] {
			w.cancel()
			delete(n.workers, id)
		}
	}
}

func (n *notifier) worker(id string) *destWorker {
	n.sync()
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.workers[id]
}

func (w *destWorker) dest() notifyDest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.d
}

func (w *destWorker) status() destStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st
}

// allows says whether an alert of this severity is for this destination.
func (d notifyDest) allows(sev string) bool { return d.Severity != "critical" || sev == "critical" }

// dispatch queues the raised and cleared alerts of one cycle for every
// destination. It returns at once.
func (n *notifier) dispatch(raised, cleared []Alert, base string, nodes map[int64]string) {
	if len(raised)+len(cleared) == 0 {
		return
	}
	n.sync()
	now := n.clock.Now().Unix()
	n.mu.Lock()
	ws := make([]*destWorker, 0, len(n.workers))
	for _, w := range n.workers {
		ws = append(ws, w)
	}
	n.mu.Unlock()
	for _, w := range ws {
		d := w.dest()
		var r, c []Alert
		for _, a := range raised {
			if d.allows(a.Severity) {
				r = append(r, a)
			}
		}
		if d.Cleared {
			for _, a := range cleared {
				if d.allows(a.Severity) {
					c = append(c, a)
				}
			}
		}
		for _, grp := range []struct {
			event  string
			alerts []Alert
		}{{"raise", r}, {"clear", c}} {
			if len(grp.alerts) == 0 {
				continue
			}
			mk := func(as []Alert, sum bool) notifyMsg {
				return notifyMsg{Event: grp.event, Alerts: as, Summary: sum, Time: now, Base: base, Nodes: nodes}
			}
			// The raw webhook is read by programs that expect one alert per
			// message, as it always was: it is never summarised.
			if len(grp.alerts) > notifyBurst && d.Kind != "webhook" {
				w.enqueue(mk(grp.alerts, true))
				continue
			}
			for _, a := range grp.alerts {
				w.enqueue(mk([]Alert{a}, false))
			}
		}
	}
}

func (w *destWorker) enqueue(m notifyMsg) {
	select {
	case w.queue <- m:
	default:
		w.fail("too many messages are waiting; some were dropped", w.n.clock.Now())
	}
}

func (w *destWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-w.queue:
			w.deliver(ctx, m)
		}
	}
}

func (w *destWorker) fail(reason string, at time.Time) {
	w.mu.Lock()
	w.st.Failing, w.st.Error, w.st.At = true, reason, at
	w.mu.Unlock()
}

func (w *destWorker) succeed(at time.Time) {
	w.mu.Lock()
	w.st = destStatus{LastOK: at}
	w.mu.Unlock()
}

// sendResult is the outcome of one try.
type sendResult struct {
	OK         bool
	Status     int
	Message    string        // plain words
	Permanent  bool          // retrying cannot help
	RetryAfter time.Duration // from a 429
}

// deliver sends m, retrying on failures that may pass: the first try, then
// after 5 s, 30 s and 2 min. A 429 waits as long as the service asks instead.
func (w *destWorker) deliver(ctx context.Context, m notifyMsg) {
	for try := 0; ; try++ {
		res := w.send(ctx, m)
		if ctx.Err() != nil {
			return
		}
		if res.OK {
			w.succeed(w.n.clock.Now())
			return
		}
		if res.Permanent || try >= len(notifyBackoff) {
			w.fail(res.Message, w.n.clock.Now())
			log.Printf("notify: giving up on a message to %q: %s", w.dest().Name, res.Message)
			return
		}
		wait := notifyBackoff[try]
		if res.RetryAfter > 0 {
			wait = min(res.RetryAfter, maxRetryAfter)
		}
		w.fail(res.Message, w.n.clock.Now()) // shown while retries are waiting
		if !w.n.clock.Sleep(ctx, wait) {
			return
		}
	}
}

// send makes one HTTP try with the destination as it is now.
func (w *destWorker) send(ctx context.Context, m notifyMsg) sendResult {
	d := w.dest()
	if d.Broken != "" {
		return sendResult{Message: d.Broken, Permanent: true}
	}
	body := render(d.Kind, m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return sendResult{Message: "The address is not valid.", Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "virticle-alerts")
	if d.Kind == "webhook" && d.Secret != "" {
		req.Header.Set("X-Virticle-Signature", signBody(d.Secret, body))
	}
	resp, err := w.n.client(d.Kind).Do(req)
	if err != nil {
		return sendResult{Message: describeError(err)}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return describeStatus(resp.StatusCode, resp.Header.Get("Retry-After"), string(b))
}

// sendTest sends the test message once, without retries, and returns the result.
func (w *destWorker) sendTest(ctx context.Context) sendResult {
	d := w.dest()
	m := notifyMsg{Event: "test", Alerts: []Alert{testAlert(d.Name)}, Time: w.n.clock.Now().Unix()}
	res := w.send(ctx, m)
	if res.OK {
		w.succeed(w.n.clock.Now())
	} else {
		w.fail(res.Message, w.n.clock.Now())
	}
	return res
}

// describeStatus turns an HTTP answer into words a person can act on.
func describeStatus(code int, retryAfter, body string) sendResult {
	r := sendResult{Status: code}
	if code >= 200 && code <= 299 {
		r.OK = true
		r.Message = fmt.Sprintf("The service accepted the message (HTTP %d).", code)
		return r
	}
	switch {
	case code == 429:
		r.Message = "The service is limiting how many messages it takes (HTTP 429). Virticle tries again later."
		if f, err := strconv.ParseFloat(strings.TrimSpace(retryAfter), 64); err == nil && f > 0 {
			r.RetryAfter = time.Duration(f * float64(time.Second))
		}
	case code == 404 || code == 410:
		r.Message = fmt.Sprintf("The service does not know this address (HTTP %d). It may be wrong, or the webhook may have been deleted.", code)
		r.Permanent = true
	case code == 401 || code == 403:
		r.Message = fmt.Sprintf("The service refused the message (HTTP %d). The webhook may have been revoked.", code)
		r.Permanent = true
	case code >= 500:
		r.Message = fmt.Sprintf("The service had an error (HTTP %d).", code)
	case code == 408:
		r.Message = "The service took too long to answer (HTTP 408)."
	default:
		r.Message = fmt.Sprintf("The service did not accept the message (HTTP %d).", code)
		r.Permanent = true
	}
	if s := oneLine(body, 100); s != "" && code != 429 && code < 500 {
		r.Message += " It said: " + s
	}
	return r
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if strings.HasPrefix(s, "<") { // an HTML page says nothing useful
		return ""
	}
	return shorten(s, n)
}

var errForbiddenAddr = errors.New("this address points to a machine Virticle is not allowed to send to")

// describeError words a transport error. It never includes the address that
// was called: the net/http error repeats the whole URL, and the URL is a secret.
func describeError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.Is(err, errForbiddenAddr):
		return "Virticle may not send to this address: it points to the controller itself or to a link-local address."
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "No answer within 10 seconds."
	case errors.Is(err, context.Canceled):
		return "Stopped."
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "The name " + dns.Name + " cannot be found."
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "Cannot connect: " + shorten(trimTo(op.Err.Error()), 100) + "."
	}
	return "Cannot send: " + shorten(trimTo(err.Error()), 120) + "."
}

func trimTo(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }

// --- where Virticle may send ---

// Destinations are on the internet or the LAN, and the controller sits next to
// its nodes, so a webhook address is also a way to make the controller call
// something on its own side. The rules:
//
//   - Slack, Mattermost and Discord: https only; the address must not point to
//     this machine (loopback), the unspecified address or a link-local address.
//     Private (LAN) addresses are allowed, because a self-hosted Mattermost is
//     on the LAN.
//   - Raw webhook: http or https, and whatever the administrator enters, except
//     link-local addresses (169.254.0.0/16, fe80::/10). That range holds the
//     cloud metadata services, the usual target for stealing credentials through
//     a server that makes requests for others. A receiver on loopback or the LAN
//     is a normal setup for a raw webhook (a script on the same machine).
//
// The check is made when the address is saved (for a clear message) and again at
// connection time on the address actually connected to, so a name that later
// resolves somewhere else (DNS rebinding) is refused as well.
func forbiddenAddr(kind string, ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if kind == "webhook" {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast()
}

// lookupIPs is replaced in tests.
var lookupIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// checkDestURL validates the address for a type and returns what the page may
// show of it (the host with its scheme, the last characters).
func checkDestURL(kind, raw string) (host, tail string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || u.Host == "" || u.Hostname() == "" {
		return "", "", badRequest("This is not a web address. It should look like https://example.com/path")
	}
	if u.User != nil {
		return "", "", badRequest("The address must not contain a user name or password.")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && kind == "webhook":
	case u.Scheme == "http":
		return "", "", badRequest(destKinds[kind] + " addresses start with https://")
	default:
		return "", "", badRequest("The address must start with https://")
	}
	ips, lerr := resolveHost(u.Hostname())
	if lerr != nil {
		return "", "", badRequest("The name " + u.Hostname() + " cannot be found.")
	}
	for _, ip := range ips {
		if forbiddenAddr(kind, ip) {
			if kind == "webhook" {
				return "", "", badRequest("Link-local addresses (169.254.0.0/16, fe80::/10) are not allowed.")
			}
			return "", "", badRequest("This address points to this machine or to a link-local address. Use the address of the service.")
		}
	}
	if len(raw) < 4 {
		return "", "", badRequest("The address is too short.")
	}
	return u.Scheme + "://" + u.Host, raw[len(raw)-4:], nil
}

func resolveHost(h string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return []netip.Addr{ip}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := lookupIPs(ctx, h)
	if err == nil && len(ips) == 0 {
		err = errors.New("no addresses")
	}
	return ips, err
}

// client returns the HTTP client for a destination type: no redirects (the
// receiver is not trusted to send us elsewhere), no proxy from the environment
// (the address check must see the real peer), 10 s for the whole exchange.
func (n *notifier) client(kind string) *http.Client {
	if n.testClient != nil {
		return n.testClient
	}
	strict := kind != "webhook"
	n.mu.Lock()
	defer n.mu.Unlock()
	if c := n.clients[strict]; c != nil {
		return c
	}
	k := kind
	dialer := &net.Dialer{Timeout: notifyTimeout, Control: func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip, err := netip.ParseAddr(host); err == nil && forbiddenAddr(k, ip) {
			return errForbiddenAddr
		}
		return nil
	}}
	c := &http.Client{
		Timeout:       notifyTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: notifyTimeout, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	n.clients[strict] = c
	return c
}

// --- the controller's destinations ---

func (s *Server) secrets() (*secretbox.Box, error) {
	s.boxOnce.Do(func() {
		path := ""
		if s.DataDir != "" {
			path = filepath.Join(s.DataDir, "settings.key")
		}
		s.box, s.boxErr = secretbox.OpenFile(path)
	})
	return s.box, s.boxErr
}

// sealURL is the form in which the store keeps an address.
func (s *Server) sealURL(kind, raw string) (enc, host, tail string, err error) {
	host, tail, err = checkDestURL(kind, raw)
	if err != nil {
		return
	}
	box, err := s.secrets()
	if err != nil {
		return
	}
	enc, err = box.Seal(raw)
	return
}

// legacyWebhookKey is where earlier versions kept the one webhook address.
const legacyWebhookKey = "alert.webhook"

// notifier returns the controller's notifier, set up on first use: the
// webhook of earlier versions becomes a destination before anything is sent.
func (s *Server) notifier() *notifier {
	s.notifyOnce.Do(func() {
		if s.Store != nil {
			seal := func(u string) (string, string, string, error) {
				box, err := s.secrets()
				if err != nil {
					return "", "", "", err
				}
				enc, err := box.Seal(u)
				host, tail := legacyParts(u)
				return enc, host, tail, err
			}
			if moved, err := s.Store.MoveLegacyWebhook(legacyWebhookKey, seal, time.Now().Unix()); err != nil {
				log.Printf("notifications: moving the old webhook address: %v", err)
			} else if moved {
				log.Printf("notifications: the webhook address of earlier versions is now the destination \"Webhook\"")
			}
		}
		s.notify = newNotifier(s.loadDests)
	})
	return s.notify
}

// legacyParts are the host and tail of an address that was accepted before the
// new rules existed; they are not checked again here.
func legacyParts(raw string) (host, tail string) {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host = u.Scheme + "://" + u.Host
	}
	if len(raw) >= 4 {
		tail = raw[len(raw)-4:]
	}
	return
}

// loadDests reads the stored destinations and the -alert-webhook flag.
func (s *Server) loadDests() []notifyDest {
	var out []notifyDest
	if s.Store != nil {
		rows, err := s.Store.NotifyDests()
		if err != nil {
			log.Printf("notifications: reading destinations: %v", err)
		}
		box, boxErr := s.secrets()
		for _, r := range rows {
			d := notifyDest{ID: strconv.FormatInt(r.ID, 10), Kind: r.Kind, Name: r.Name, Severity: r.Severity, Cleared: r.Cleared}
			var e1, e2 error
			if boxErr == nil {
				d.URL, e1 = box.Open(r.URLEnc)
				d.Secret, e2 = box.Open(r.SecretEnc)
			}
			if boxErr != nil || e1 != nil || e2 != nil {
				d.Broken = "The stored address cannot be read. Enter it again."
			}
			out = append(out, d)
		}
	}
	if s.Alerts.Webhook != "" {
		out = append(out, notifyDest{ID: "flag", Kind: "webhook", Name: "Webhook (-alert-webhook flag)", URL: s.Alerts.Webhook, Severity: "warning", Cleared: true, ReadOnly: true})
	}
	return out
}

// dispatchAlerts hands one cycle's changes to the notifier.
func (s *Server) dispatchAlerts(raised, cleared []Alert) {
	nodes := map[int64]string{}
	if s.Nodes != nil {
		for _, n := range s.Nodes.Status() {
			name := n.Hostname
			if name == "" {
				name = n.Addr
			}
			nodes[n.ID] = name
		}
	}
	s.notifier().dispatch(raised, cleared, s.publicBase(), nodes)
}
