package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/export"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

func runExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	conn := connFlags(fs)

	out := fs.String("out", "", "archive to write (required)")
	schemaOnly := fs.Bool("schema-only", false, "dump definitions but no rows")
	force := fs.Bool("force", false, "overwrite --out if it already exists")
	deterministic := fs.Bool("deterministic", false,
		"byte-identical archives for an unchanged database: rows in primary key order, no creation time, no range splitting")
	resume := fs.Bool("resume", false, "continue an export interrupted earlier")
	restart := fs.Bool("restart", false, "discard an interrupted export and start over")
	parallel := fs.Int("parallel", 4, "pieces of work to read concurrently")
	chunkMin := fs.Int64("chunk-min-bytes", export.DefaultChunkMinBytes,
		"split tables larger than this into ranges read in parallel; -1 disables splitting")
	var include, exclude, excludeData, where stringList
	fs.Var(&include, "include", "only these tables; glob on schema.table, repeatable")
	fs.Var(&exclude, "exclude", "omit these tables entirely, definition included; repeatable")
	fs.Var(&excludeData, "exclude-data", "keep these tables' definitions but skip their rows; repeatable")
	fs.Var(&where, "where", "restrict a table's rows: <table-glob>:<T-SQL predicate>; repeatable")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	if err := finishConn(fs, conn); err != nil {
		return err
	}
	if conn.DatabaseName() == "" {
		return errors.New("--database is required")
	}
	if *resume && *restart {
		return errors.New("--resume and --restart are mutually exclusive")
	}
	// A finished archive is only in the way when this run will actually
	// finish; an interrupted one is expected to overwrite it eventually.
	if !*force {
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", *out)
		}
	}

	// Each worker holds a connection for the length of a table, so the pool has
	// to be at least as large as the worker count or they queue behind it.
	conn.MaxConns = *parallel + 2
	logf("connecting to %s", conn.Redacted())
	db, err := sqlsrv.Open(ctx, *conn)
	if err != nil {
		return err
	}
	defer db.Close()

	interval := 5 * time.Second
	if status.IsTerminal() {
		interval = time.Second
	}

	res, err := export.Run(ctx, db, export.Options{
		ProgressInterval: interval,
		Out:              *out,
		SchemaOnly:       *schemaOnly,
		Include:          include,
		Exclude:          exclude,
		ExcludeData:      excludeData,
		Where:            where,
		Parallel:         *parallel,
		ChunkMinBytes:    *chunkMin,
		Deterministic:    *deterministic,
		Resume:           *resume,
		Restart:          *restart,
		Log:              logf,
		Progress:         showProgress,
		Warn:             warnf,
	})
	if err != nil {
		return err
	}

	size := int64(0)
	if fi, err := os.Stat(*out); err == nil {
		size = fi.Size()
	}
	logf("\nwrote %s (%s archive, %s of data) - %d tables, %d rows in %s",
		*out, humanBytes(size), humanBytes(res.DataBytes),
		res.Tables, res.Rows, res.Duration.Round(1e6))
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
