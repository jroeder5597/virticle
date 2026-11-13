package metrics

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	nameRe  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parseExposition is a strict reader for the text format: every sample must
// follow HELP and TYPE of its family, families must not repeat, label values
// must be escaped, label sets must be unique and counters must end in _total.
func parseExposition(t *testing.T, text string) map[string][]string {
	t.Helper()
	if !strings.HasSuffix(text, "\n") {
		t.Fatal("output must end with a line feed")
	}
	types := map[string]string{}
	helped := map[string]bool{}
	seenSample := map[string]bool{}
	samples := map[string][]string{}
	current := ""
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(f string, a ...any) { t.Fatalf("line %d %q: %s", n+1, line, fmt.Sprintf(f, a...)) }
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !nameRe.MatchString(name) || helped[name] {
				fail("bad or repeated HELP")
			}
			helped[name], current = true, name
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(f) != 2 || f[0] != current || types[f[0]] != "" {
				fail("TYPE must follow HELP of the same family, once")
			}
			if f[1] != "counter" && f[1] != "gauge" {
				fail("unexpected type")
			}
			if f[1] == "counter" && !strings.HasSuffix(f[0], "_total") {
				fail("counter without _total")
			}
			types[f[0]] = f[1]
		case strings.HasPrefix(line, "#"):
			fail("unexpected comment")
		default:
			name := line
			rest := ""
			labels := map[string]string{}
			if i := strings.IndexAny(line, "{ "); i >= 0 {
				name = line[:i]
				rest = line[i:]
			}
			if !nameRe.MatchString(name) || !strings.HasPrefix(name, "virticle_") {
				fail("bad metric name")
			}
			if name != current || types[name] == "" {
				fail("sample outside its family")
			}
			if strings.HasPrefix(rest, "{") {
				i := 1
				for rest[i] != '}' {
					j := strings.IndexByte(rest[i:], '=')
					if j < 0 || rest[i+j+1] != '"' {
						fail("bad label")
					}
					key := rest[i : i+j]
					if !labelRe.MatchString(key) {
						fail("bad label name %q", key)
					}
					k := i + j + 2
					var val strings.Builder
					for ; ; k++ {
						if k >= len(rest) {
							fail("unterminated label value")
						}
						c := rest[k]
						if c == '"' {
							break
						}
						if c == '\\' {
							k++
							switch rest[k] {
							case '\\', '"':
								val.WriteByte(rest[k])
							case 'n':
								val.WriteByte('\n')
							default:
								fail("bad escape")
							}
							continue
						}
						val.WriteByte(c)
					}
					labels[key] = val.String()
					i = k + 1
					if rest[i] == ',' {
						i++
					}
				}
				rest = rest[i+1:]
			}
			if !strings.HasPrefix(rest, " ") {
				fail("no value")
			}
			v, err := strconv.ParseFloat(strings.TrimPrefix(rest, " "), 64)
			if err != nil {
				fail("bad value")
			}
			id := name + fmt.Sprint(labels)
			if seenSample[id] {
				fail("duplicate sample")
			}
			seenSample[id] = true
			samples[name] = append(samples[name], fmt.Sprintf("%v=%v", labels, v))
		}
	}
	return samples
}

func sampleTracker() *NodeTracker {
	tr := NewNodeTracker(1, `n"1`)
	at := t0
	nr := ParseNodeRead(at, nodeOut)
	nr.Pools = []Pool{{Name: "default", Capacity: 1000, Allocation: 400, Avail: 600}}
	doms := []Dom{
		{UUID: "11111111-1111-1111-1111-111111111111", Name: "web\nline", State: "running", Raw: dom(at, 5e9, 100, 5, 7e6, 50)},
		{UUID: "22222222-2222-2222-2222-222222222222", Name: "off", State: "shut off"},
	}
	tr.Collect(at, doms, true, &nr)
	return tr
}

func TestPrometheusText(t *testing.T) {
	var buf bytes.Buffer
	nodes := []PromNode{{Name: `n"1`, Up: true, Tracker: sampleTracker()}, {Name: "gone", Up: false}}
	if err := WriteProm(&buf, nodes, "1.2.3", "go1.26"); err != nil {
		t.Fatal(err)
	}
	s := parseExposition(t, buf.String())
	for _, want := range []string{"virticle_build_info", "virticle_node_up", "virticle_node_cpu_seconds_total", "virticle_node_load1",
		"virticle_node_network_receive_bytes_total", "virticle_node_disk_read_bytes_total", "virticle_node_storage_pool_capacity_bytes",
		"virticle_node_gpu_utilization_ratio", "virticle_vm_state", "virticle_vm_cpu_seconds_total", "virticle_vm_disk_read_bytes_total",
		"virticle_vm_network_receive_bytes_total", "virticle_vm_memory_used_bytes"} {
		if len(s[want]) == 0 {
			t.Errorf("missing %s", want)
		}
	}
	if len(s["virticle_node_up"]) != 2 {
		t.Errorf("node_up: %v", s["virticle_node_up"])
	}
	if len(s["virticle_vm_state"]) != 2 || len(s["virticle_vm_cpu_seconds_total"]) != 1 {
		t.Errorf("a stopped VM reports its state only: %v %v", s["virticle_vm_state"], s["virticle_vm_cpu_seconds_total"])
	}
	// Temperature was [N/A]: no sample rather than NaN.
	if len(s["virticle_node_gpu_temperature_celsius"]) != 0 {
		t.Errorf("unknown value was written: %v", s["virticle_node_gpu_temperature_celsius"])
	}
	if !strings.Contains(buf.String(), `node="n\"1"`) || !strings.Contains(buf.String(), `vm="web\nline"`) {
		t.Errorf("labels are not escaped:\n%s", buf.String())
	}
}

func TestPrometheusGolden(t *testing.T) {
	tr := NewNodeTracker(1, "n1")
	nr := NodeRaw{At: t0, HasLoad: true, Load: [3]float64{0.5, 0.25, 0.1}, Nets: []NodeNet{{Name: "eth0", Physical: true, RxBytes: 10, TxBytes: 20}}}
	tr.Collect(t0.Add(time.Second), nil, false, &nr)
	var buf bytes.Buffer
	WriteProm(&buf, []PromNode{{Name: "n1", Up: true, Tracker: tr}}, "dev", "go")
	want := `# HELP virticle_build_info Version of the controller; the value is always 1.
# TYPE virticle_build_info gauge
virticle_build_info{version="dev",goversion="go"} 1
# HELP virticle_node_up 1 when the controller is connected to the node.
# TYPE virticle_node_up gauge
virticle_node_up{node="n1"} 1
# HELP virticle_node_vms_running Virtual machines running or paused on the node.
# TYPE virticle_node_vms_running gauge
virticle_node_vms_running{node="n1"} 0
# HELP virticle_node_load1 Load average over 1 minute.
# TYPE virticle_node_load1 gauge
virticle_node_load1{node="n1"} 0.5
# HELP virticle_node_load5 Load average over 5 minutes.
# TYPE virticle_node_load5 gauge
virticle_node_load5{node="n1"} 0.25
# HELP virticle_node_load15 Load average over 15 minutes.
# TYPE virticle_node_load15 gauge
virticle_node_load15{node="n1"} 0.1
# HELP virticle_node_network_receive_bytes_total Bytes received by a network card or bridge.
# TYPE virticle_node_network_receive_bytes_total counter
virticle_node_network_receive_bytes_total{node="n1",device="eth0"} 10
# HELP virticle_node_network_transmit_bytes_total Bytes sent by a network card or bridge.
# TYPE virticle_node_network_transmit_bytes_total counter
virticle_node_network_transmit_bytes_total{node="n1",device="eth0"} 20
# HELP virticle_node_network_receive_errors_total Receive errors.
# TYPE virticle_node_network_receive_errors_total counter
virticle_node_network_receive_errors_total{node="n1",device="eth0"} 0
# HELP virticle_node_network_transmit_errors_total Transmit errors.
# TYPE virticle_node_network_transmit_errors_total counter
virticle_node_network_transmit_errors_total{node="n1",device="eth0"} 0
# HELP virticle_node_network_receive_drops_total Received packets dropped.
# TYPE virticle_node_network_receive_drops_total counter
virticle_node_network_receive_drops_total{node="n1",device="eth0"} 0
# HELP virticle_node_network_transmit_drops_total Packets to send that were dropped.
# TYPE virticle_node_network_transmit_drops_total counter
virticle_node_network_transmit_drops_total{node="n1",device="eth0"} 0
`
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}
