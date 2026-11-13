package api

import "testing"

func TestHasBitmap(t *testing.T) {
	one := `{"format-specific":{"type":"qcow2","data":{"bitmaps":[{"name":"virticle-1","flags":["auto"]},{"name":"mine","flags":[]}]}}}`
	many := `[` + one + `,{"format-specific":{"data":{"bitmaps":[{"name":"virticle-2"}]}}}]`
	for _, c := range []struct {
		info, name string
		want       bool
	}{
		{one, "virticle-1", true}, {one, "mine", true}, {one, "virticle-2", false},
		{many, "virticle-1", true}, {many, "virticle-2", false}, {"not json", "virticle-1", false}, {`{}`, "x", false},
	} {
		if got := hasBitmap(c.info, c.name); got != c.want {
			t.Errorf("hasBitmap(%.30q, %s) = %v, want %v", c.info, c.name, got, c.want)
		}
	}
}
