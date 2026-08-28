package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// bulkRows loads a table through the TDS bulk-copy protocol.
//
// This is several times faster than INSERT batches, because rows go over the
// wire in the server's own row format: no statement text, no parameter
// declarations, and so no plan compilation per batch. It is only usable for
// tables whose columns the driver's bulk encoder supports - see sqlsrv.BulkSafe.
//
// KEEP_NULLS is on: without it the server would substitute column defaults for
// NULLs, which would silently rewrite the data.
func bulkRows(ctx context.Context, conn *sql.Conn, dec *json.Decoder, codec *sqlsrv.RowCodec,
	t model.Table, cols []model.Column, opts Options) (int64, error) {

	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	copyStmt := mssql.CopyIn(t.QualifiedName(), mssql.BulkOptions{KeepNulls: true}, names...)

	var (
		total  int64
		rowBuf = make([]any, 0, len(cols))
		eof    bool
	)

	for !eof {
		n, atEOF, err := bulkChunk(ctx, conn, copyStmt, dec, codec, rowBuf, opts.CommitRows)
		total += n
		eof = atEOF
		if err != nil {
			return total, err
		}
		if n > 0 && !atEOF {
			opts.log("  %-50s %10d rows...", t.Schema+"."+t.Name, total)
		}
	}

	opts.log("  %-50s %10d rows", t.Schema+"."+t.Name, total)
	return total, nil
}

// bulkChunk streams up to limit rows in one transaction. It reports how many
// rows it wrote and whether the data file is exhausted.
func bulkChunk(ctx context.Context, conn *sql.Conn, copyStmt string, dec *json.Decoder,
	codec *sqlsrv.RowCodec, rowBuf []any, limit int) (int64, bool, error) {

	// Peek before opening a transaction, so a chunk boundary that lands exactly
	// on the end of the file does not leave an empty bulk operation behind.
	if !dec.More() {
		return 0, true, nil
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, copyStmt)
	if err != nil {
		return 0, false, fmt.Errorf("start bulk copy: %w", err)
	}

	var (
		n   int64
		eof bool
	)
	for n < int64(limit) {
		var raw []any
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				eof = true
				break
			}
			stmt.Close()
			return 0, false, fmt.Errorf("row %d of chunk: %w", n+1, err)
		}
		vals, err := codec.DecodeBulk(raw, rowBuf)
		if err != nil {
			stmt.Close()
			return 0, false, fmt.Errorf("row %d of chunk: %w", n+1, err)
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			stmt.Close()
			return 0, false, fmt.Errorf("bulk copy row %d of chunk: %w", n+1, err)
		}
		n++
	}

	// An Exec with no arguments finalizes the bulk operation and flushes it.
	if _, err := stmt.ExecContext(ctx); err != nil {
		stmt.Close()
		return 0, false, fmt.Errorf("finish bulk copy: %w", err)
	}
	if err := stmt.Close(); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return n, eof, nil
}
