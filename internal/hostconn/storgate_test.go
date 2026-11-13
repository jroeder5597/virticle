package hostconn

import (
	"sync"
	"testing"
	"time"
)

func shortGate(t *testing.T) {
	oldW, oldC := gateWait, callLimit
	gateWait, callLimit = 30*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { gateWait, callLimit = oldW, oldC })
}

// Three storage calls may wait at once; the fourth gives up with ErrStorageBusy instead of queueing, and a slot
// that is given back is usable again.
func TestStorageGateSlots(t *testing.T) {
	shortGate(t)
	var g storGate
	for i := 0; i < storageSlots; i++ {
		if err := g.acquire(); err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := g.acquire(); err != ErrStorageBusy {
		t.Fatalf("a full gate must refuse, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("refusal took %v", time.Since(start))
	}
	g.release()
	if err := g.acquire(); err != nil {
		t.Errorf("a released slot is usable: %v", err)
	}
}

// A call that does not answer is given up on after callLimit; when three are stuck every further call fails at
// once; when one returns the gate works again.
func TestStorageGateStuckCalls(t *testing.T) {
	shortGate(t)
	var g storGate
	block := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < storageSlots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.call(func() { <-block }); err != ErrStorageBusy {
				t.Errorf("a stuck call is given up on: %v", err)
			}
		}()
	}
	wg.Wait()
	start := time.Now()
	ran := false
	if err := g.call(func() { ran = true }); err != ErrStorageBusy || ran {
		t.Fatalf("with every slot stuck a call must fail without running: %v %v", err, ran)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Errorf("with every slot stuck the refusal is immediate, took %v", time.Since(start))
	}
	close(block) // libvirt answers
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := g.call(func() {}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the gate did not recover")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStorageGateNormalCall(t *testing.T) {
	var g storGate
	n := 0
	for i := 0; i < 10; i++ {
		if err := g.call(func() { n++ }); err != nil {
			t.Fatal(err)
		}
	}
	if n != 10 {
		t.Errorf("%d", n)
	}
}
