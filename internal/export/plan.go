package export

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// savedPlan is how a table's division is recorded in the work directory, so a
// resume reads the remaining ranges with the boundaries the first run used.
//
// Boundaries travel through the same encoding as row values. Writing them as
// plain JSON would not do: a bigint beyond 2^53 loses precision as a JSON
// number, and a uniqueidentifier or a datetime has no JSON form at all. The row
// codec already knows how to put each type on paper and take it back off.
type savedPlan struct {
	Key string `json:"key"`
	// Column carries the key's type, so the boundaries can be decoded back into
	// the driver values a comparison needs.
	Column model.Column `json:"column"`
	Ranges []savedRange `json:"ranges"`
}

type savedRange struct {
	Index    int  `json:"index"`
	Lo       any  `json:"lo,omitempty"`
	Hi       any  `json:"hi,omitempty"`
	HasLo    bool `json:"hasLo,omitempty"`
	HasHi    bool `json:"hasHi,omitempty"`
	NullKeys bool `json:"nullKeys,omitempty"`
}

func newSavedPlan(key sqlsrv.ChunkKey, chunks []sqlsrv.Chunk) savedPlan {
	p := savedPlan{Key: key.Column.Name, Column: key.Column}
	for _, c := range chunks {
		p.Ranges = append(p.Ranges, savedRange{
			Index: c.Index, Lo: c.Lo, Hi: c.Hi,
			HasLo: c.HasLo, HasHi: c.HasHi, NullKeys: c.NullKeys,
		})
	}
	return p
}

// chunks turns a recorded plan back into ranges whose boundaries are the driver
// values a query parameter needs.
func (p savedPlan) chunks(key sqlsrv.ChunkKey) ([]sqlsrv.Chunk, error) {
	codec := sqlsrv.NewRowCodec([]model.Column{key.Column})
	decode := func(v any) (any, error) {
		// Round-trip through the decoder the same way a row value would, so a
		// number keeps its precision and a GUID or timestamp keeps its type.
		raw, err := json.Marshal([]any{v})
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var row []any
		if err := dec.Decode(&row); err != nil {
			return nil, err
		}
		out, err := codec.Decode(row, nil)
		if err != nil {
			return nil, err
		}
		if len(out) != 1 {
			return nil, fmt.Errorf("decoded %d values, want 1", len(out))
		}
		return out[0], nil
	}

	chunks := make([]sqlsrv.Chunk, 0, len(p.Ranges))
	for _, r := range p.Ranges {
		c := sqlsrv.Chunk{Index: r.Index, HasLo: r.HasLo, HasHi: r.HasHi, NullKeys: r.NullKeys}
		if r.HasLo {
			v, err := decode(r.Lo)
			if err != nil {
				return nil, fmt.Errorf("range %d lower bound: %w", r.Index, err)
			}
			c.Lo = v
		}
		if r.HasHi {
			v, err := decode(r.Hi)
			if err != nil {
				return nil, fmt.Errorf("range %d upper bound: %w", r.Index, err)
			}
			c.Hi = v
		}
		chunks = append(chunks, c)
	}
	return chunks, nil
}
