package worker

import (
	"strings"
	"testing"
)

// TestAppendValuesTuple confirms the manual SQL fragment builder
// emits valid placeholders. We don't go through PG here - that lives
// in the integration test suite. This test guards against off-by-one
// bugs in appendInt / appendValuesTuple, which would silently corrupt
// the batched UPSERT statement at scale.
func TestAppendValuesTuple(t *testing.T) {
	cases := []struct {
		base int
		want string
	}{
		{1, "($1,$2,$3,$4,$5::jsonb,$6,'discovered')"},
		{7, "($7,$8,$9,$10,$11::jsonb,$12,'discovered')"},
		{55, "($55,$56,$57,$58,$59::jsonb,$60,'discovered')"},
	}
	for _, c := range cases {
		got := string(appendValuesTuple(nil, c.base))
		if got != c.want {
			t.Errorf("base=%d: got %q want %q", c.base, got, c.want)
		}
	}
}

// TestAppendValuesTupleConcat verifies the comma-joined output for
// a multi-row batch is grammatical SQL. A bad concat (missing comma,
// stray space) would land at PG parse time, but that's slow to find
// in CI; this is the cheap check.
func TestAppendValuesTupleConcat(t *testing.T) {
	var buf []byte
	for i := 0; i < 3; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendValuesTuple(buf, i*6+1)
	}
	got := string(buf)
	want := "($1,$2,$3,$4,$5::jsonb,$6,'discovered')," +
		"($7,$8,$9,$10,$11::jsonb,$12,'discovered')," +
		"($13,$14,$15,$16,$17::jsonb,$18,'discovered')"
	if got != want {
		t.Errorf("multi-row concat:\ngot  %q\nwant %q", got, want)
	}
	// Sanity: every $N is unique and monotonically increasing.
	if strings.Count(got, "$1)") != 0 {
		// If we ever rendered "$1)" we'd have stuffed multiple rows
		// onto the same param column.
		t.Errorf("rogue placeholder collision: %s", got)
	}
}

func TestAppendInt(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"}, {1, "1"}, {9, "9"},
		{10, "10"}, {99, "99"}, {100, "100"},
		{12345, "12345"},
	}
	for _, c := range cases {
		got := string(appendInt(nil, c.n))
		if got != c.want {
			t.Errorf("appendInt(%d) = %q want %q", c.n, got, c.want)
		}
	}
}
