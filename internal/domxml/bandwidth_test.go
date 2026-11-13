package domxml

import (
	"strings"
	"testing"
)

const nicDoc = `<domain><devices>
    <interface type='network'>
      <mac address='52:54:00:aa:bb:cc'/>
      <source network='default'/>
      %s
      <model type='virtio'/>
    </interface>
</devices></domain>`

func nicOf(t *testing.T, bw string) (*Doc, *Node) {
	t.Helper()
	d, err := Parse(strings.Replace(nicDoc, "%s", bw, 1))
	if err != nil {
		t.Fatal(err)
	}
	return d, d.Devices("interface")[0]
}

func TestSetNICBandwidth(t *testing.T) {
	d, n := nicOf(t, "")
	out, err := d.SetNICBandwidth(n, Rate{6250, 1250}, Rate{12500, 2500})
	if err != nil {
		t.Fatal(err)
	}
	want := "<bandwidth><inbound average='6250' burst='1250'/><outbound average='12500' burst='2500'/></bandwidth>"
	if !strings.Contains(out, want) || !strings.Contains(out, "<model type='virtio'/>") {
		t.Fatalf("got %s", out)
	}
	out, _ = d.SetNICBandwidth(n, Rate{}, Rate{125, 0})
	if !strings.Contains(out, "<bandwidth><outbound average='125'/></bandwidth>") || strings.Contains(out, "inbound") {
		t.Fatalf("got %s", out)
	}
}

func TestSetNICBandwidthReplaceAndRemove(t *testing.T) {
	d, n := nicOf(t, "<bandwidth><inbound average='1' peak='2' burst='3'/></bandwidth>")
	out, err := d.SetNICBandwidth(n, Rate{500, 100}, Rate{})
	if err != nil || strings.Contains(out, "peak") || !strings.Contains(out, "<inbound average='500' burst='100'/>") {
		t.Fatalf("replace: %v %s", err, out)
	}
	out, err = d.SetNICBandwidth(n, Rate{}, Rate{})
	if err != nil || strings.Contains(out, "bandwidth") || !strings.Contains(out, "<source network='default'/>") {
		t.Fatalf("remove: %v %s", err, out)
	}
	d, n = nicOf(t, "")
	out, err = d.SetNICBandwidth(n, Rate{}, Rate{})
	if err != nil || strings.Contains(out, "bandwidth") {
		t.Fatalf("noop: %v %s", err, out)
	}
}

func TestSourceChangeKeepsBandwidth(t *testing.T) {
	d, n := nicOf(t, "<bandwidth><inbound average='125'/></bandwidth>")
	out, err := d.SetNICSource(n, "other", "", "")
	if err != nil || !strings.Contains(out, "<bandwidth><inbound average='125'/></bandwidth>") {
		t.Fatalf("%v %s", err, out)
	}
}
