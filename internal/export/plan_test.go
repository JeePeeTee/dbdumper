package export

import (
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// TestSavedPlanRoundTrip - the boundaries have to survive the work directory
// unchanged. A resume that recomputed them, or decoded them into a different
// type, would shift the ranges and leave rows in no chunk at all.
func TestSavedPlanRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		col    model.Column
		lo, hi any
	}{
		{
			// The case the feature exists for: XPO's clustered GUID key.
			name: "uniqueidentifier",
			col:  model.Column{Name: "Oid", TypeName: "uniqueidentifier"},
			lo:   "704134D0-404A-408F-8FE0-0019758A4154",
			hi:   "CB4BB20B-2C2C-4740-BD0D-3D7C6D246C06",
		},
		{
			// Beyond 2^53: a plain JSON number would lose the low bits here.
			name: "bigint past the float53 boundary",
			col:  model.Column{Name: "Id", TypeName: "bigint"},
			lo:   int64(9007199254740993),
			hi:   int64(9223372036854775807),
		},
		{
			name: "int",
			col:  model.Column{Name: "Id", TypeName: "int"},
			lo:   int64(1000), hi: int64(2000),
		},
		{
			name: "datetime2",
			col:  model.Column{Name: "At", TypeName: "datetime2", Scale: 7},
			lo:   "2026-01-01T00:00:00.0000001",
			hi:   "2026-08-27T13:45:00.1234567",
		},
		{
			name: "decimal as a string, so no precision is lost",
			col:  model.Column{Name: "Amount", TypeName: "decimal", Precision: 38, Scale: 10},
			lo:   "-1234567890123456789012345678.1234567890",
			hi:   "12345678901234.5678",
		},
	}

	for _, c := range cases {
		key := sqlsrv.ChunkKey{Column: c.col}
		in := []sqlsrv.Chunk{
			{Index: 0, Hi: c.hi, HasHi: true},
			{Index: 1, Lo: c.lo, HasLo: true, Hi: c.hi, HasHi: true},
			{Index: 2, Lo: c.lo, HasLo: true},
			{Index: 3, NullKeys: true},
		}

		out, err := newSavedPlan(key, in).chunks(key)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(out) != len(in) {
			t.Errorf("%s: got %d ranges, want %d", c.name, len(out), len(in))
			continue
		}
		for i := range in {
			if out[i].HasLo != in[i].HasLo || out[i].HasHi != in[i].HasHi ||
				out[i].NullKeys != in[i].NullKeys || out[i].Index != in[i].Index {
				t.Errorf("%s range %d: shape changed: %+v vs %+v", c.name, i, out[i], in[i])
			}
		}
		// The boundary must come back as a value a comparison can use, and must
		// still describe the same point in the ordering.
		if got := out[1].Lo; !sameBoundary(got, c.lo) {
			t.Errorf("%s: lower bound came back as %#v, want %#v", c.name, got, c.lo)
		}
		if got := out[1].Hi; !sameBoundary(got, c.hi) {
			t.Errorf("%s: upper bound came back as %#v, want %#v", c.name, got, c.hi)
		}
	}
}

// sameBoundary compares what went in with what came out. The codec turns some
// types into their string form on the way to the server, which is the point -
// what matters is that no information was lost.
func sameBoundary(got, want any) bool {
	switch w := want.(type) {
	case int64:
		g, ok := got.(int64)
		return ok && g == w
	default:
		return got == want
	}
}

func TestSavedPlanRejectsAMangledBoundary(t *testing.T) {
	key := sqlsrv.ChunkKey{Column: model.Column{Name: "Id", TypeName: "int"}}
	p := savedPlan{Key: "Id", Column: key.Column, Ranges: []savedRange{
		{Index: 0, Lo: "not a number", HasLo: true},
	}}
	if _, err := p.chunks(key); err == nil {
		t.Error("a boundary that is not of the key's type should be refused, not guessed at")
	}
}
