package sqlsrv

import (
	"context"
	"fmt"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// ChunkKey describes the column a table can be split on.
type ChunkKey struct {
	Column model.Column
}

// Chunk is one slice of a table, expressed as a half-open range on the chunk
// key. Lo and Hi are driver values passed as query parameters, never rendered
// into SQL text.
//
// The first chunk has no Lo and the last no Hi, so the set covers everything
// below the first boundary and everything above the last. A separate chunk
// takes the NULL keys, which no comparison would match.
type Chunk struct {
	Index    int
	Lo, Hi   any
	HasLo    bool
	HasHi    bool
	NullKeys bool
}

// Predicate renders this chunk's range as a WHERE fragment, appending its
// parameters to args.
//
// Correctness does not depend on the boundaries being well chosen. The chunks
// partition the table for any values at all, because they are consecutive
// half-open ranges over the same ordering the server compares with. A poor
// boundary makes one chunk larger than another; it cannot lose or duplicate a
// row.
func (c Chunk) Predicate(key string, args []any) (string, []any) {
	col := model.Quote(key)
	if c.NullKeys {
		return col + " IS NULL", args
	}
	var parts []string
	if c.HasLo {
		parts = append(parts, fmt.Sprintf("%s >= @p%d", col, len(args)+1))
		args = append(args, c.Lo)
	}
	if c.HasHi {
		parts = append(parts, fmt.Sprintf("%s < @p%d", col, len(args)+1))
		args = append(args, c.Hi)
	}
	if len(parts) == 0 {
		// A single chunk covering everything; still exclude NULLs, which the
		// dedicated NULL chunk takes.
		return col + " IS NOT NULL", args
	}
	// The comparisons already exclude NULL, so no extra guard is needed.
	return strings.Join(parts, " AND "), args
}

// chunkableTypes are the system types a range scan can be built on. Excluded
// are the ones SQL Server will not order or compare: the large object types,
// the CLR types, and sql_variant, whose comparison semantics depend on the
// value's own base type.
var chunkableTypes = map[string]bool{
	"tinyint": true, "smallint": true, "int": true, "bigint": true,
	"decimal": true, "numeric": true, "money": true, "smallmoney": true,
	"uniqueidentifier": true,
	"date":             true, "datetime": true, "datetime2": true,
	"smalldatetime": true, "datetimeoffset": true, "time": true,
	"char": true, "varchar": true, "nchar": true, "nvarchar": true,
	"binary": true, "varbinary": true,
}

// ChunkKeyFor reports the column a table can be split on, if any.
//
// It has to be the single key column of the clustered index: that is what makes
// each chunk a range scan rather than a full table scan, which is the whole
// point. A heap, a composite key, or a key of a type that cannot be ordered
// means the table is read in one piece.
func (in *Introspector) ChunkKeyFor(ctx context.Context, t model.Table) (ChunkKey, bool) {
	if t.PrimaryKey == nil && len(t.Indexes) == 0 {
		return ChunkKey{}, false
	}

	// The clustered index is the one whose storage order the range scan follows.
	var keyName string
	for _, ix := range append([]model.Index{}, indexesOf(t)...) {
		if !strings.HasPrefix(ix.TypeDes, "CLUSTERED") || strings.Contains(ix.TypeDes, "COLUMNSTORE") {
			continue
		}
		var keys []string
		for _, ic := range ix.Columns {
			if !ic.IsIncluded {
				keys = append(keys, ic.Name)
			}
		}
		if len(keys) == 1 {
			keyName = keys[0]
		}
		break
	}
	if keyName == "" {
		return ChunkKey{}, false
	}

	for _, c := range t.Columns {
		if !strings.EqualFold(c.Name, keyName) {
			continue
		}
		if c.IsComputed || !chunkableTypes[c.SystemType()] {
			return ChunkKey{}, false
		}
		return ChunkKey{Column: c}, true
	}
	return ChunkKey{}, false
}

// indexesOf lists every index on a table, the primary key included: SQL Server
// stores a clustered primary key in sys.indexes like any other index, and this
// package splits them apart when building the model.
func indexesOf(t model.Table) []model.Index {
	out := make([]model.Index, 0, len(t.Indexes)+len(t.UniqueConstraints)+1)
	if t.PrimaryKey != nil {
		out = append(out, *t.PrimaryKey)
	}
	out = append(out, t.UniqueConstraints...)
	out = append(out, t.Indexes...)
	return out
}

// PlanChunks divides a table into n ranges over its chunk key.
//
// Boundaries come from the server, which sorts by its own rules - relevant for
// uniqueidentifier, whose ordering is neither byte order nor the order the text
// form suggests. They are read from a sample rather than the whole table: a
// full scan and sort to plan the read would cost as much as the read.
//
// Returns nil when the table cannot usefully be split, which the caller should
// treat as "read it in one piece" rather than as an error.
func (in *Introspector) PlanChunks(ctx context.Context, t model.Table, key ChunkKey, n int) ([]Chunk, error) {
	if n < 2 {
		return nil, nil
	}

	// Sample enough rows for the boundaries to be representative without
	// reading the table twice. TABLESAMPLE picks pages, and a clustered index
	// stores pages in key order, so the sample spans the whole range.
	const wantSampleRows = 50000
	percent := 100
	if t.EstimatedRows > wantSampleRows {
		percent = int(100 * wantSampleRows / t.EstimatedRows)
		if percent < 1 {
			percent = 1
		}
	}
	sample := t.QualifiedName()
	if percent < 100 {
		sample += fmt.Sprintf(" TABLESAMPLE SYSTEM (%d PERCENT)", percent)
	}

	col := model.Quote(key.Column.Name)
	q := fmt.Sprintf(`
SELECT MIN(%[1]s) FROM (
  SELECT %[1]s, NTILE(%[2]d) OVER (ORDER BY %[1]s) AS g
  FROM %[3]s WHERE %[1]s IS NOT NULL
) x GROUP BY g ORDER BY g`, col, n, sample)

	rows, err := in.DB.QueryContext(ctx, q)
	if err != nil {
		// Planning is an optimisation; a server that will not do it just means
		// the table is read in one piece.
		in.warn("could not plan chunks for %s.%s (%v); reading it in one piece", t.Schema, t.Name, err)
		return nil, nil
	}
	defer rows.Close()

	codec := NewRowCodec([]model.Column{key.Column})
	var bounds []any
	for rows.Next() {
		if err := rows.Scan(codec.ScanDest()...); err != nil {
			return nil, err
		}
		v := codec.Encode(nil)
		if len(v) == 1 && v[0] != nil {
			bounds = append(bounds, v[0])
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The first group's minimum is the table's minimum, which is not a
	// boundary: everything is at or above it.
	if len(bounds) > 1 {
		bounds = bounds[1:]
	} else {
		bounds = nil
	}
	bounds = dedupeBoundaries(bounds)
	if len(bounds) == 0 {
		// One distinct value, or a sample too thin to divide. Not worth
		// splitting, and splitting on nothing would produce empty chunks.
		return nil, nil
	}

	chunks := make([]Chunk, 0, len(bounds)+2)
	for i := 0; i <= len(bounds); i++ {
		c := Chunk{Index: i}
		if i > 0 {
			c.Lo, c.HasLo = bounds[i-1], true
		}
		if i < len(bounds) {
			c.Hi, c.HasHi = bounds[i], true
		}
		chunks = append(chunks, c)
	}
	// NULL keys sort outside every range, so they need a chunk of their own or
	// they would be silently dropped.
	if key.Column.IsNullable {
		chunks = append(chunks, Chunk{Index: len(chunks), NullKeys: true})
	}
	return chunks, nil
}

// dedupeBoundaries removes repeats, which a skewed key produces and which would
// otherwise yield chunks that can match no rows at all.
func dedupeBoundaries(in []any) []any {
	out := make([]any, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		k := fmt.Sprintf("%v", v)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}
