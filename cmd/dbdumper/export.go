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
	var include, exclude, excludeData stringList
	fs.Var(&include, "include", "only these tables; glob on schema.table, repeatable")
	fs.Var(&exclude, "exclude", "omit these tables entirely, definition included; repeatable")
	fs.Var(&excludeData, "exclude-data", "keep these tables' definitions but skip their rows; repeatable")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	finishConn(fs, conn)
	if conn.DatabaseName() == "" {
		return errors.New("--database is required")
	}
	if !*force {
		if _, err := os.Stat(*out); err == nil {
			return fmt.Errorf("%s already exists (pass --force to overwrite)", *out)
		}
	}

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
	logf("\nwrote %s (%s) - %d tables, %d rows in %s",
		*out, humanBytes(size), res.Tables, res.Rows, res.Duration.Round(1e6))
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
