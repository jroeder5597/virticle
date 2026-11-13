package api

import (
	"reflect"
	"testing"
)

func TestStaleBitmapsOf(t *testing.T) {
	info := `{"filename":"/p/a.qcow2","format":"qcow2","virtual-size":1073741824,
	"format-specific":{"type":"qcow2","data":{"compat":"1.1","bitmaps":[
	 {"flags":["in-use","auto"],"name":"virticle-1700000000","granularity":65536},
	 {"flags":["auto"],"name":"virticle-1700000001","granularity":65536},
	 {"flags":["in-use","auto"],"name":"backup-vda","granularity":65536},
	 {"flags":["in-use","auto"],"name":"mine","granularity":65536}]}}}`
	want := []string{"virticle-1700000000", "backup-vda"}
	if got := staleBitmapsOf(info); !reflect.DeepEqual(got, want) {
		t.Errorf("object: %v, want %v", got, want)
	}
	// A backing chain is a list: the active image is first.
	chain := "[" + info + `,{"filename":"/p/base.qcow2","format":"qcow2","format-specific":{"type":"qcow2","data":{"bitmaps":[{"flags":["in-use"],"name":"virticle-9"}]}}}]`
	if got := staleBitmapsOf(chain); !reflect.DeepEqual(got, want) {
		t.Errorf("chain: %v, want %v", got, want)
	}
	for _, in := range []string{"", "{", `{"format":"raw"}`, `{"format-specific":{"type":"qcow2","data":{"compat":"1.1"}}}`, "[]"} {
		if got := staleBitmapsOf(in); len(got) != 0 {
			t.Errorf("%q: %v", in, got)
		}
	}
	if !ownBitmap("virticle-5") || !ownBitmap("backup-vdb") || ownBitmap("user-bitmap") || ownBitmap("") {
		t.Error("ownBitmap")
	}
}
