package sqlsrv

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// kind selects how a column's values move between TDS and JSON.
type kind int

const (
	kindString kind = iota
	kindBool
	kindInt
	kindFloat
	kindNumeric // decimal/numeric/money: carried as a decimal string
	kindBytes   // binary families, CLR types: carried as base64
	kindGUID
	kindDate
	kindTimeOfDay
	kindDateTime  // datetime / smalldatetime: millisecond resolution
	kindDateTime2 // date + time up to 100ns
	kindDateTimeOffset
	kindVariant
)

// Layouts are chosen so every rendered string is language-neutral for SQL
// Server's implicit string-to-date conversions (the "T" separator matters).
const (
	layoutDate           = "2006-01-02"
	layoutTimeOfDay      = "15:04:05.9999999"
	layoutDateTime       = "2006-01-02T15:04:05.999"
	layoutDateTime2      = "2006-01-02T15:04:05.9999999"
	layoutDateTimeOffset = "2006-01-02T15:04:05.9999999Z07:00"
)

func kindOf(c model.Column) kind {
	switch c.SystemType() {
	case "bit":
		return kindBool
	case "tinyint", "smallint", "int", "bigint":
		return kindInt
	case "real", "float":
		return kindFloat
	case "decimal", "numeric", "money", "smallmoney":
		return kindNumeric
	case "uniqueidentifier":
		return kindGUID
	case "binary", "varbinary", "image", "timestamp", "rowversion",
		"geography", "geometry", "hierarchyid":
		return kindBytes
	case "date":
		return kindDate
	case "time":
		return kindTimeOfDay
	case "datetime", "smalldatetime":
		return kindDateTime
	case "datetime2":
		return kindDateTime2
	case "datetimeoffset":
		return kindDateTimeOffset
	case "sql_variant":
		return kindVariant
	default:
		return kindString
	}
}

// RowCodec converts one table's rows between driver values and JSON values.
// It is not safe for concurrent use; make one per reader/writer.
type RowCodec struct {
	cols  []model.Column
	kinds []kind
	dest  []any
}

// NewRowCodec builds a codec for the given columns, in file order.
func NewRowCodec(cols []model.Column) *RowCodec {
	rc := &RowCodec{cols: cols, kinds: make([]kind, len(cols)), dest: make([]any, len(cols))}
	for i, c := range cols {
		rc.kinds[i] = kindOf(c)
		switch rc.kinds[i] {
		case kindBool:
			rc.dest[i] = new(sql.NullBool)
		case kindInt:
			rc.dest[i] = new(sql.NullInt64)
		case kindFloat:
			rc.dest[i] = new(sql.NullFloat64)
		case kindNumeric, kindString:
			rc.dest[i] = new(sql.NullString)
		case kindBytes:
			rc.dest[i] = new([]byte)
		case kindGUID:
			rc.dest[i] = new(mssql.NullUniqueIdentifier)
		case kindDate, kindTimeOfDay, kindDateTime, kindDateTime2, kindDateTimeOffset:
			rc.dest[i] = new(sql.NullTime)
		default:
			rc.dest[i] = new(any)
		}
	}
	return rc
}

// ScanDest returns the pointers to hand to sql.Rows.Scan. The backing values
// are reused on every call, so encode before scanning the next row.
func (rc *RowCodec) ScanDest() []any { return rc.dest }

// SelectList renders the column list for the SELECT that feeds ScanDest.
func (rc *RowCodec) SelectList() string {
	parts := make([]string, len(rc.cols))
	for i, c := range rc.cols {
		parts[i] = model.Quote(c.Name)
	}
	return strings.Join(parts, ", ")
}

// Encode turns the most recently scanned row into JSON-marshalable values.
func (rc *RowCodec) Encode(out []any) []any {
	out = out[:0]
	for i := range rc.cols {
		out = append(out, rc.encodeOne(i))
	}
	return out
}

func (rc *RowCodec) encodeOne(i int) any {
	switch rc.kinds[i] {
	case kindBool:
		v := rc.dest[i].(*sql.NullBool)
		if !v.Valid {
			return nil
		}
		return v.Bool
	case kindInt:
		v := rc.dest[i].(*sql.NullInt64)
		if !v.Valid {
			return nil
		}
		return v.Int64
	case kindFloat:
		v := rc.dest[i].(*sql.NullFloat64)
		if !v.Valid {
			return nil
		}
		return v.Float64
	case kindNumeric, kindString:
		v := rc.dest[i].(*sql.NullString)
		if !v.Valid {
			return nil
		}
		return v.String
	case kindBytes:
		v := *rc.dest[i].(*[]byte)
		if v == nil {
			return nil
		}
		return base64.StdEncoding.EncodeToString(v)
	case kindGUID:
		v := rc.dest[i].(*mssql.NullUniqueIdentifier)
		if !v.Valid {
			return nil
		}
		return v.UUID.String()
	case kindDate, kindTimeOfDay, kindDateTime, kindDateTime2, kindDateTimeOffset:
		v := rc.dest[i].(*sql.NullTime)
		if !v.Valid {
			return nil
		}
		return v.Time.Format(timeLayout(rc.kinds[i]))
	default:
		v := *rc.dest[i].(*any)
		switch x := v.(type) {
		case nil:
			return nil
		case []byte:
			return base64.StdEncoding.EncodeToString(x)
		case time.Time:
			return x.Format(layoutDateTimeOffset)
		default:
			return x
		}
	}
}

func timeLayout(k kind) string {
	switch k {
	case kindDate:
		return layoutDate
	case kindTimeOfDay:
		return layoutTimeOfDay
	case kindDateTime:
		return layoutDateTime
	case kindDateTimeOffset:
		return layoutDateTimeOffset
	default:
		return layoutDateTime2
	}
}

// Decode turns one JSON row (as produced by Encode, unmarshalled with
// json.Decoder.UseNumber) into parameters for an INSERT.
func (rc *RowCodec) Decode(row []any, out []any) ([]any, error) {
	if len(row) != len(rc.cols) {
		return nil, fmt.Errorf("row has %d values, table has %d columns", len(row), len(rc.cols))
	}
	out = out[:0]
	for i, raw := range row {
		v, err := rc.decodeOne(i, raw)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", rc.cols[i].Name, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func (rc *RowCodec) decodeOne(i int, raw any) (any, error) {
	if raw == nil {
		// An untyped nil reaches the server as a NULL of type nvarchar, and
		// nvarchar has no implicit conversion to the binary types. A nil
		// []byte is sent as a varbinary NULL, which they do accept. Every
		// other type takes the untyped NULL happily (a nil []byte, in turn,
		// is rejected by the date/time, float and sql_variant types), so the
		// choice has to be made per column.
		if rc.kinds[i] == kindBytes {
			return []byte(nil), nil
		}
		return nil, nil
	}
	switch rc.kinds[i] {
	case kindBool:
		switch x := raw.(type) {
		case bool:
			return x, nil
		case json.Number:
			return x.String() != "0", nil
		}
		return nil, fmt.Errorf("expected bool, got %T", raw)

	case kindInt:
		n, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("expected number, got %T", raw)
		}
		return n.Int64()

	case kindFloat:
		n, ok := raw.(json.Number)
		if !ok {
			return nil, fmt.Errorf("expected number, got %T", raw)
		}
		return n.Float64()

	case kindBytes:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected base64 string, got %T", raw)
		}
		return base64.StdEncoding.DecodeString(s)

	default:
		// Numeric, GUID, all date/time kinds and plain strings all travel as
		// strings; SQL Server converts them to the column type on insert.
		switch x := raw.(type) {
		case string:
			return x, nil
		case json.Number:
			return x.String(), nil
		case bool:
			return strconv.FormatBool(x), nil
		}
		return nil, fmt.Errorf("expected string, got %T", raw)
	}
}
