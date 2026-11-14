package prepare

import (
	"strings"
	"testing"

	"virticle/internal/nodeproto"
)

func guardHostFacts(version, hook string) Plan {
	p := plan(debian13Facts)
	p.Facts.Virsh = "/usr/bin/virsh"
	p.Facts.LibvirtVersion = version
	if hook != "" {
		p.Facts.GuardSHA, p.Facts.GuardOurs = sha(hook), strings.Contains(hook, nodeproto.HookMark)
	}
	return p
}

func TestGuardRow(t *testing.T) {
	o := Options{ControllerKey: testKey}
	ours := nodeproto.HookScript(o.ServiceUser())
	cases := []struct {
		name, version, hook, status, has string
	}{
		{"libvirt 10 (Ubuntu 24.04)", "10.0.0", "", Info, "Will be installed"},
		{"libvirt 11.3 (Debian 13)", "11.3.0", "", Info, "Will be installed"},
		{"libvirt 11.10 (Rocky 10)", "11.10.0", "", Info, "Will be installed"},
		{"libvirt not installed yet", "", "", Info, "Will be installed"},
		{"too old", "6.0.0", "", Warning, "no hook directory"},
		{"current", "11.10.0", ours, OK, "Installed at " + nodeproto.HookPath},
		{"an older script of ours", "11.10.0", "#!/bin/sh\n# " + nodeproto.HookMark + "\nold\n", Info, "will be updated"},
		{"somebody else's file", "11.10.0", "#!/bin/sh\nexit 0\n", Warning, "left alone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := find(Evaluate(guardHostFacts(tc.version, tc.hook), o), "guard")
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.has) {
				t.Errorf("%+v, want %s with %q", r, tc.status, tc.has)
			}
		})
	}
}

func TestGuardHookUsesTheServiceAccountAndFailsOpen(t *testing.T) {
	s := nodeproto.HookScript("virticle-node")
	for _, must := range []string{"#!/bin/sh", `"$n" hook-qemu virticle-node "$@"`, `[ -x "$n" ] || exit 0`, "-eq 13 ] && exit 1", "exit 0"} {
		if !strings.Contains(s, must) {
			t.Errorf("the hook lacks %q:\n%s", must, s)
		}
	}
	if !nodeproto.IsGuardHook(s) || nodeproto.IsGuardHook("#!/bin/sh\nexit 0\n") {
		t.Error("IsGuardHook")
	}
}

// guardRun runs the step against a scripted host; changed says whether installing the file changed anything.
func guardRun(changed bool, probe string, f Facts) (string, bool, error, []string) {
	var log []string
	n := &fakeNode{}
	n.handlers = append(n.handlers, func(cmd, stdin string) (string, bool, error) {
		log = append(log, cmd)
		switch {
		case strings.Contains(cmd, "virticle-guard-selftest"):
			return probe, true, nil
		case strings.Contains(cmd, "dest="):
			if changed {
				return "installed\n@@changed", true, nil
			}
			return "current\n@@unchanged", true, nil
		}
		return "", true, nil
	})
	d, ch, err := stepGuard(&Runner{N: n, isRoot: true}, Plan{Facts: f}, Options{ControllerKey: testKey})
	return d, ch, err, log
}

func asStatus(err error, target **statusError) bool {
	se, ok := err.(*statusError)
	if ok {
		*target = se
	}
	return ok
}

func TestStepGuard(t *testing.T) {
	good := "result: " + nodeproto.SelftestMark + " exec=ok ssh=ok key=ok net=ok"
	if d, ch, err, log := guardRun(true, good, Facts{}); err != nil || !ch || !strings.Contains(d, "libvirt runs it") || len(log) != 3 {
		t.Errorf("good: %q %v %v (%d calls)", d, ch, err, len(log))
	}
	// nothing changed: no reload, no probe VM
	if _, ch, err, log := guardRun(false, good, Facts{}); err != nil || ch || len(log) != 1 {
		t.Errorf("unchanged: %v %v %d", ch, err, len(log))
	}
	// SELinux or AppArmor forbids the network: installed, warned
	var se *statusError
	_, _, err, _ := guardRun(true, "result: "+nodeproto.SelftestMark+" exec=ok ssh=denied key=ok net=denied", Facts{})
	if !asStatus(err, &se) || se.Status != Warning || !strings.Contains(se.Detail, "ssh denied, net denied") {
		t.Errorf("denied: %v", err)
	}
	// libvirt ignored the hook (not reloaded): kept, with the instruction
	_, _, err, log := guardRun(true, "nohook rc=0: ", Facts{})
	if !asStatus(err, &se) || !strings.Contains(se.Detail, "did not run it") || len(log) != 3 {
		t.Errorf("not run: %v", err)
	}
	// libvirt failed to run the hook: removed again
	_, _, err, log = guardRun(true, "nohook rc=1: error: Hook script execution failed", Facts{})
	if !asStatus(err, &se) || !strings.Contains(se.Detail, "was removed again") || !strings.Contains(strings.Join(log, "\n"), `rm -f "$h"`) {
		t.Errorf("broken hook: %v\n%v", err, log)
	}
	// old libvirt and a file that is not ours: nothing installed
	_, _, err, log = guardRun(true, good, Facts{Virsh: "/usr/bin/virsh", LibvirtVersion: "6.0.0"})
	if !asStatus(err, &se) || se.Status != Warning || len(log) != 0 {
		t.Errorf("old libvirt: %v %d", err, len(log))
	}
	_, _, err, log = guardRun(true, good, Facts{GuardSHA: "abc"})
	if !asStatus(err, &se) || len(log) != 0 {
		t.Errorf("foreign file: %v %d", err, len(log))
	}
}

func TestUnprepareRemovesOnlyOurHook(t *testing.T) {
	for _, must := range []string{nodeproto.HookPath, nodeproto.HookMark, "start guard removed"} {
		if !strings.Contains(unprepareScript, must) {
			t.Errorf("the removal script lacks %q", must)
		}
	}
	if titleOf("start guard removed") != "Start guard" {
		t.Error("title")
	}
}

func TestArpingIsInThePackageLists(t *testing.T) {
	for fam, want := range map[Family]string{FamilyDebian: "iputils-arping", FamilyRHEL: "iputils"} {
		found := false
		for _, p := range PackageList(fam, true) {
			found = found || p == want
		}
		if !found {
			t.Errorf("%s lacks %s", fam, want)
		}
	}
}
