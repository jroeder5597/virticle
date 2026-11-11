package api

import "testing"

func tagPool(p poolInfo, id int64, name string) nodePool { return nodePool{p, id, name} }

func TestMergeNodes(t *testing.T) {
	res := []nodeResult[poolInfo]{
		{Node: 2, Name: "n2", Items: []poolInfo{{Name: "default"}, {Name: "zz"}}},
		{Node: 3, Name: "n3", Err: "ssh: connection refused"},
		{Node: 1, Name: "n1", Items: []poolInfo{{Name: "default"}}},
		{Node: 4, Name: "n4", Items: nil},
	}
	items, errs := mergeNodes(res, tagPool)
	if len(items) != 3 {
		t.Fatalf("want 3 pools, got %d", len(items))
	}
	want := []struct {
		node int64
		name string
	}{{1, "default"}, {2, "default"}, {2, "zz"}}
	for i, w := range want {
		if items[i].Node != w.node || items[i].Name != w.name || items[i].NodeName == "" {
			t.Errorf("item %d = %+v", i, items[i])
		}
	}
	if len(errs) != 1 || errs[0].Node != 3 || errs[0].NodeName != "n3" || errs[0].Error == "" {
		t.Errorf("errors = %+v", errs)
	}
}

func TestMergeNodesEmpty(t *testing.T) {
	items, errs := mergeNodes(nil, tagPool)
	if items == nil || errs == nil || len(items) != 0 || len(errs) != 0 {
		t.Errorf("want empty non-nil slices (they are JSON arrays), got %v %v", items, errs)
	}
}

func TestMergeNodesOrderByNameThenID(t *testing.T) {
	res := []nodeResult[poolInfo]{
		{Node: 9, Name: "b", Items: []poolInfo{{Name: "x"}}},
		{Node: 5, Name: "a", Items: []poolInfo{{Name: "x"}}},
		{Node: 3, Name: "a", Items: []poolInfo{{Name: "x"}}},
	}
	items, _ := mergeNodes(res, tagPool)
	if items[0].Node != 3 || items[1].Node != 5 || items[2].Node != 9 {
		t.Errorf("order %v %v %v", items[0].Node, items[1].Node, items[2].Node)
	}
}
