package importer

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// maxParams is SQL Server's hard limit of 2100 parameters per statement, with
// a little headroom.
const maxParams = 2000

type dataHeader struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

func loadTable(ctx context.Context, db *sql.DB, ar *archive.Reader, t model.Table, opts Options) (int64, error) {
	rc, err := ar.OpenEntry(t.DataFile)
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	dec := json.NewDecoder(bufio.NewReaderSize(rc, 1<<20))
	dec.UseNumber()

	var hdr dataHeader
	if err := dec.Decode(&hdr); err != nil {
		if err == io.EOF {
			return 0, nil // empty data file
		}
		return 0, fmt.Errorf("read header: %w", err)
	}

	cols, err := resolveColumns(t, hdr.Columns)
	if err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		return 0, nil
	}

	codec := sqlsrv.NewRowCodec(cols)

	// IDENTITY_INSERT is session state, so the whole load runs on one pinned
	// connection.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if hasIdentityIn(cols) {
		if _, err := conn.ExecContext(ctx, "SET IDENTITY_INSERT "+t.QualifiedName()+" ON"); err != nil {
			return 0, fmt.Errorf("enable IDENTITY_INSERT: %w", err)
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), "SET IDENTITY_INSERT "+t.QualifiedName()+" OFF")
	}

	if !opts.NoBulk && sqlsrv.BulkSafe(cols) {
		return bulkRows(ctx, conn, dec, codec, t, cols, opts)
	}
	return insertRows(ctx, conn, dec, codec, t, cols, opts)
}

// insertRows loads a table with batched multi-row INSERT statements. It is the
// general path: it handles every type, at roughly a third of bulk copy's speed.
func insertRows(ctx context.Context, conn *sql.Conn, dec *json.Decoder, codec *sqlsrv.RowCodec,
	t model.Table, cols []model.Column, opts Options) (int64, error) {

	if len(cols) > maxParams {
		return 0, fmt.Errorf("table has %d columns, more than the %d parameters a single INSERT allows", len(cols), maxParams)
	}
	rowsPerBatch := opts.BatchRows
	if max := maxParams / len(cols); rowsPerBatch > max {
		rowsPerBatch = max
	}
	if rowsPerBatch < 1 {
		rowsPerBatch = 1
	}

	insertPrefix := buildInsertPrefix(t, cols)

	// Staged rows live in one flat buffer, so a batch is a contiguous slice of
	// it and can be passed to Exec without copying or per-row allocation.
	var (
		total   int64
		stage   = make([]any, rowsPerBatch*len(cols))
		staged  int
		tx      *sql.Tx
		sinceTx int
	)

	// Almost every batch has exactly rowsPerBatch rows, so the statement for
	// that one shape is reused rather than rebuilt.
	fullQuery := insertPrefix + valuesClause(rowsPerBatch, len(cols))
	var full *sql.Stmt

	flush := func() error {
		if staged == 0 {
			return nil
		}
		params := stage[:staged*len(cols)]
		var err error
		if staged == rowsPerBatch {
			if full == nil {
				// Prepared lazily: most tables in a dump are small and never
				// fill a batch, and an unused prepare is a wasted round trip.
				if full, err = tx.PrepareContext(ctx, fullQuery); err != nil {
					return fmt.Errorf("prepare insert: %w", err)
				}
			}
			_, err = full.ExecContext(ctx, params...)
		} else {
			// The trailing partial batch; not worth preparing.
			q := insertPrefix + valuesClause(staged, len(cols))
			_, err = tx.ExecContext(ctx, q, params...)
		}
		if err != nil {
			return fmt.Errorf("insert %d rows at offset %d: %w", staged, total-int64(staged), err)
		}
		staged = 0
		return nil
	}

	begin := func() error {
		newTx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		tx, full, sinceTx = newTx, nil, 0
		return nil
	}
	if err := begin(); err != nil {
		return 0, err
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()

	// commit closes the prepared statement first: it belongs to the
	// transaction and is invalid once that transaction ends.
	commit := func() error {
		if full != nil {
			if err := full.Close(); err != nil {
				return err
			}
		}
		err := tx.Commit()
		tx, full = nil, nil
		return err
	}

	for {
		var raw []any
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				break
			}
			return total, fmt.Errorf("row %d: %w", total+1, err)
		}
		// Decode straight into this row's slot: a zero-length, exactly-capped
		// window over the staging buffer, so the appends land in place.
		lo := staged * len(cols)
		if _, err := codec.Decode(raw, stage[lo:lo:lo+len(cols)]); err != nil {
			return total, fmt.Errorf("row %d: %w", total+1, err)
		}
		staged++
		total++
		sinceTx++

		if staged >= rowsPerBatch {
			if err := flush(); err != nil {
				return total, err
			}
		}
		if sinceTx >= opts.CommitRows {
			if err := flush(); err != nil {
				return total, err
			}
			if err := commit(); err != nil {
				return total, err
			}
			if err := begin(); err != nil {
				return total, err
			}
			opts.log("  %-50s %10d rows...", t.Schema+"."+t.Name, total)
		}
	}

	if err := flush(); err != nil {
		return total, err
	}
	if err := commit(); err != nil {
		return total, err
	}

	opts.log("  %-50s %10d rows", t.Schema+"."+t.Name, total)
	return total, nil
}

// resolveColumns matches the names recorded in the data file back to the
// manifest's column definitions, preserving file order.
func resolveColumns(t model.Table, names []string) ([]model.Column, error) {
	byName := make(map[string]model.Column, len(t.Columns))
	for _, c := range t.Columns {
		byName[strings.ToLower(c.Name)] = c
	}
	out := make([]model.Column, 0, len(names))
	for _, n := range names {
		c, ok := byName[strings.ToLower(n)]
		if !ok {
			return nil, fmt.Errorf("data file references unknown column %q", n)
		}
		out = append(out, c)
	}
	return out, nil
}

func hasIdentityIn(cols []model.Column) bool {
	for _, c := range cols {
		if c.IsIdentity {
			return true
		}
	}
	return false
}

func buildInsertPrefix(t model.Table, cols []model.Column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = model.Quote(c.Name)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES ", t.QualifiedName(), strings.Join(names, ", "))
}

// valuesClause renders "(@p1,@p2),(@p3,@p4)" for the given shape.
func valuesClause(rows, cols int) string {
	var b strings.Builder
	b.Grow(rows * cols * 6)
	n := 1
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < cols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('@')
			b.WriteByte('p')
			b.WriteString(itoa(n))
			n++
		}
		b.WriteByte(')')
	}
	return b.String()
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
