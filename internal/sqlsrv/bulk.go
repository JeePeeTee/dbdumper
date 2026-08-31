package sqlsrv

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// BulkSafe reports whether every column can be written through TDS bulk copy.
//
// The driver's bulk encoder covers the common types but has no case for XML,
// sql_variant or the CLR types (geography, geometry, hierarchyid); a table
// containing one of those has to go through ordinary INSERTs.
func BulkSafe(cols []model.Column) bool {
	for _, c := range cols {
		switch kindOf(c) {
		case kindVariant:
			return false
		}
		// The fallback below is meant to catch types a newer server has added,
		// but it cannot see vector: SystemType resolves that to varbinary,
		// which is on the accepted list.
		if c.IsVector() {
			return false
		}
		switch c.SystemType() {
		case "xml", "geography", "geometry", "hierarchyid":
			return false
		case "bit", "tinyint", "smallint", "int", "bigint",
			"real", "float", "decimal", "numeric", "money", "smallmoney",
			"char", "varchar", "text", "nchar", "nvarchar", "ntext", "sysname",
			"binary", "varbinary", "image", "uniqueidentifier",
			"date", "time", "datetime", "smalldatetime", "datetime2", "datetimeoffset":
		default:
			// Anything unrecognised (including types added by a newer server)
			// takes the safe path.
			return false
		}
	}
	return true
}

// DecodeBulk converts one JSON row into the value types the driver's bulk
// encoder accepts. It differs from Decode in two places: uniqueidentifier
// wants the raw 16 bytes rather than a string, and the datetime family wants
// a time.Time rather than an ISO string.
func (rc *RowCodec) DecodeBulk(row []any, out []any) ([]any, error) {
	if len(row) != len(rc.cols) {
		return nil, fmt.Errorf("row has %d values, table has %d columns", len(row), len(rc.cols))
	}
	out = out[:0]
	for i, raw := range row {
		v, err := rc.decodeBulkOne(i, raw)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", rc.cols[i].Name, err)
		}
		out = append(out, v)
	}
	return out, nil
}

func (rc *RowCodec) decodeBulkOne(i int, raw any) (any, error) {
	if raw == nil {
		// The bulk encoder writes a properly typed NULL for a nil of any kind,
		// because it takes the type from the destination column metadata.
		return nil, nil
	}
	switch rc.kinds[i] {
	case kindGUID:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected a GUID string, got %T", raw)
		}
		u, err := parseUUID(s)
		if err != nil {
			return nil, err
		}
		b, err := u.Value()
		if err != nil {
			return nil, err
		}
		return b, nil

	case kindDateTime, kindDateTime2, kindDateTimeOffset:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected a timestamp string, got %T", raw)
		}
		return time.Parse(timeLayout(rc.kinds[i]), s)

	default:
		// kindDate and kindTimeOfDay already use the layouts the bulk encoder
		// parses, and every other kind matches the INSERT path exactly.
		return rc.decodeOne(i, raw)
	}
}

// parseUUID accepts the canonical 8-4-4-4-12 form written by the exporter.
func parseUUID(s string) (mssql.UniqueIdentifier, error) {
	var u mssql.UniqueIdentifier
	clean := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}"), "-", "")
	if len(clean) != 32 {
		return u, fmt.Errorf("invalid uniqueidentifier %q", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return u, fmt.Errorf("invalid uniqueidentifier %q: %w", s, err)
	}
	copy(u[:], b)
	return u, nil
}
