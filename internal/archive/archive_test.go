package archive

import "testing"

// TestDataPathIsInjective - two different tables must never map to the same
// archive entry. The zip writer accepts duplicate names without complaint and
// the reader keeps only the last, so a collision silently discards one table's
// rows while the manifest still reports the original row count.
func TestDataPathIsInjective(t *testing.T) {
	// Every pair here collided under the old "replace awkward characters with
	// underscore, join with a dot" scheme.
	pairs := [][2][2]string{
		{{"dbo", "a.b"}, {"dbo.a", "b"}},
		{{"a/b", "t"}, {"a_b", "t"}},
		{{"a", "b/c"}, {"a", "b\\c"}},
		{{"a:b", "t"}, {"a?b", "t"}},
		{{"a", "b"}, {"a.b", ""}},
	}
	for _, p := range pairs {
		x := DataPath(p[0][0], p[0][1])
		y := DataPath(p[1][0], p[1][1])
		if x == y {
			t.Errorf("%q.%q and %q.%q both map to %s",
				p[0][0], p[0][1], p[1][0], p[1][1], x)
		}
	}

	// And distinctness has to hold across a whole set, not just pairwise.
	names := [][2]string{
		{"dbo", "Customer"}, {"dbo", "a.b"}, {"dbo.a", "b"}, {"a/b", "t"},
		{"a_b", "t"}, {"a%2Eb", "t"}, {"a.b", "t"}, {"sales", "Order Line"},
		{"odd name", `Weird "Table"`}, {"dbo", "Ünïcödé"},
	}
	seen := map[string][2]string{}
	for _, n := range names {
		p := DataPath(n[0], n[1])
		if prev, dup := seen[p]; dup {
			t.Errorf("%q.%q collides with %q.%q at %s", n[0], n[1], prev[0], prev[1], p)
		}
		seen[p] = n
	}
}

func TestDataPathStaysReadable(t *testing.T) {
	// The common case must not become unreadable for the sake of the rare one.
	if got, want := DataPath("dbo", "Customer"), "data/dbo.Customer.jsonl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := DataPath("sales", "Order Line"), "data/sales.Order Line.jsonl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Awkward characters are encoded rather than lost.
	if got, want := DataPath("dbo", "a/b"), "data/dbo.a%2Fb.jsonl"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
