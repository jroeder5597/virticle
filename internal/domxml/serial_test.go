package domxml

import (
	"strings"
	"testing"
)

const noSerial = `<domain type='kvm'>
  <name>t</name>
  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
  </clock>
  <devices>
    <disk type='file' device='disk'><target dev='vda'/></disk>
    <channel type='unix'><target type='virtio' name='org.qemu.guest_agent.0'/></channel>
  </devices>
</domain>`

func TestAddRemoveSerial(t *testing.T) {
	out, err := AddSerial(noSerial)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<serial type='pty'><target port='0'/></serial>", "<console type='pty'><target type='serial' port='0'/></console>", "<timer name='rtc' tickpolicy='catchup'/>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	s, _ := Serials(out)
	if len(s) != 1 || s[0].Type != "pty" || s[0].Port != "0" {
		t.Errorf("serials = %+v", s)
	}
	if _, err := AddSerial(out); err == nil {
		t.Error("second add must fail")
	}
	back, n, err := RemoveSerial(out)
	if err != nil || n != 1 {
		t.Fatalf("remove: %v %d", err, n)
	}
	if back != noSerial {
		t.Errorf("round trip changed the document:\n%s", back)
	}
}

func TestAddSerialKeepsExistingConsole(t *testing.T) {
	src := strings.Replace(noSerial, "  </devices>", "    <console type='pty'><target type='virtio' port='0'/></console>\n  </devices>", 1)
	out, err := AddSerial(src)
	if err != nil {
		t.Fatal(err)
	}
	// a virtio console is not the serial alias: ours is added next to it
	if strings.Count(out, "<console") != 2 {
		t.Errorf("want 2 consoles:\n%s", out)
	}
	src = strings.Replace(noSerial, "  </devices>", "    <console type='pty'/>\n  </devices>", 1)
	out, err = AddSerial(src)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "<console") != 1 {
		t.Errorf("an existing serial console must not be duplicated:\n%s", out)
	}
}

func TestRemoveSerialLeavesOtherTypes(t *testing.T) {
	src := strings.Replace(noSerial, "  </devices>", `    <serial type='tcp'><source mode='bind' host='127.0.0.1' service='4555'/><target port='0'/></serial>
    <console type='tcp'><target type='serial' port='0'/></console>
  </devices>`, 1)
	out, n, err := RemoveSerial(src)
	if err != nil || n != 0 || out != src {
		t.Fatalf("tcp serial must be untouched: %v %d\n%s", err, n, out)
	}
	s, _ := Serials(src)
	if len(s) != 1 || s[0].Type != "tcp" {
		t.Errorf("serials = %+v", s)
	}
	if _, err := AddSerial(src); err == nil {
		t.Error("add must refuse while another serial exists")
	}
}

func TestClockOffset(t *testing.T) {
	out, err := SetClockOffset(noSerial, "localtime")
	if err != nil {
		t.Fatal(err)
	}
	if ClockOffset(out) != "localtime" || !strings.Contains(out, "<timer name='rtc' tickpolicy='catchup'/>") {
		t.Errorf("bad result:\n%s", out)
	}
	if strings.Replace(out, "localtime", "utc", 1) != noSerial {
		t.Error("only the offset may change")
	}
	if _, err := SetClockOffset(strings.Replace(noSerial, "'utc'", "'variable'", 1), "utc"); err == nil {
		t.Error("variable offset must not be overwritten")
	}
	if _, err := SetClockOffset(noSerial, "bogus"); err == nil {
		t.Error("bogus offset accepted")
	}
}
