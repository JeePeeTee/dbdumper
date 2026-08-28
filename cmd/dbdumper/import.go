package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/importer"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	conn := connFlags(fs)

	in := fs.String("in", "", "archive to restore (required)")
	createDB := fs.Bool("create-database", false, "create --database if it does not exist")
	dropExisting := fs.Bool("drop-existing", false, "DROP and recreate --database if it exists (destroys data)")
	collation := fs.String("collation", "", "collation for a newly created database (default: the source database's)")
	schemaOnly := fs.Bool("schema-only", false, "create objects but load no rows")
	dataOnly := fs.Bool("data-only", false, "load rows into an existing, matching schema")
	batchRows := fs.Int("batch-rows", 500, "rows per INSERT statement")
	commitRows := fs.Int("commit-rows", 20000, "rows per transaction")
	parallel := fs.Int("parallel", 4, "tables to load concurrently")
	noBulk := fs.Bool("no-bulk", false, "load every table with INSERT statements instead of bulk copy")
	continueOnError := fs.Bool("continue-on-error", false, "log and skip failing statements instead of aborting")
	var include, exclude stringList
	fs.Var(&include, "include", "only these tables; glob on schema.table, repeatable")
	fs.Var(&exclude, "exclude", "skip these tables; glob on schema.table, repeatable")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("--in is required")
	}
	if *schemaOnly && *dataOnly {
		return errors.New("--schema-only and --data-only are mutually exclusive")
	}
	if err := finishConn(fs, conn); err != nil {
		return err
	}
	target := conn.DatabaseName()
	if target == "" {
		return errors.New("--database is required")
	}

	ar, err := archive.Open(*in)
	if err != nil {
		return err
	}
	defer ar.Close()

	m := ar.Manifest
	logf("archive %s", *in)
	logf("  source     %s / %s (%s)", m.Source.Server, m.Source.Database, m.Source.ServerVersion)
	logf("  created    %s", m.CreatedAt.Local().Format("2006-01-02 15:04:05"))
	logf("  contents   %d tables, %d modules", len(m.Database.Tables), len(m.Database.Modules))

	if *createDB || *dropExisting {
		coll := *collation
		if coll == "" {
			coll = m.Database.Collation
		}
		if err := ensureDatabase(ctx, *conn, target, coll, *dropExisting); err != nil {
			return err
		}
	}

	conn.MaxConns = *parallel + 2
	logf("connecting to %s", conn.Redacted())
	db, err := sqlsrv.Open(ctx, *conn)
	if err != nil {
		return err
	}
	defer db.Close()

	res, err := importer.Run(ctx, db, ar, importer.Options{
		SchemaOnly:      *schemaOnly,
		DataOnly:        *dataOnly,
		Include:         include,
		Exclude:         exclude,
		BatchRows:       *batchRows,
		CommitRows:      *commitRows,
		Parallel:        *parallel,
		NoBulk:          *noBulk,
		ContinueOnError: *continueOnError,
		Log:             logf,
		Warn:            warnf,
	})
	if res != nil {
		logf("\nrestored %d tables, %d rows into %s in %s",
			res.Tables, res.Rows, target, res.Duration.Round(1e6))
		if res.Failed > 0 {
			logf("%d objects failed:", res.Failed)
			for _, f := range res.FailedList {
				logf("  - %s", f)
			}
		}
	}
	return err
}

// ensureDatabase creates the target database, optionally dropping it first.
// It connects to master, which is where CREATE DATABASE must run.
func ensureDatabase(ctx context.Context, conn sqlsrv.ConnConfig, name, collation string, drop bool) error {
	master, err := sqlsrv.Open(ctx, conn.WithDatabase("master"))
	if err != nil {
		return fmt.Errorf("connect to master: %w", err)
	}
	defer master.Close()

	azure := conn.IsAzureSQL()

	// sys.databases rather than DB_ID(): on Azure SQL Database, DB_ID() from
	// master does not resolve the names of sibling databases.
	exists, err := databaseExists(ctx, master, name)
	if err != nil {
		return err
	}

	if exists && drop {
		logf("dropping existing database %s", name)
		if !azure {
			// Azure SQL Database has no SINGLE_USER mode; elsewhere this is
			// what lets the DROP through while sessions are still connected.
			q := fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE", model.Quote(name))
			if _, err := master.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("take %s offline: %w", name, err)
			}
		}
		if _, err := master.ExecContext(ctx, "DROP DATABASE "+model.Quote(name)); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		exists = false
	}

	if exists {
		logf("database %s already exists; restoring into it", name)
		return nil
	}

	q := "CREATE DATABASE " + model.Quote(name)
	if collation != "" {
		q += " COLLATE " + collation
	}
	logf("creating database %s", name)
	if _, err := master.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	// CREATE DATABASE returns before the database is usable on Azure SQL,
	// where provisioning is asynchronous.
	return waitForOnline(ctx, master, name, azure)
}

func databaseExists(ctx context.Context, master *sql.DB, name string) (bool, error) {
	var n int
	err := master.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sys.databases WHERE name = @p1", name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("look up database %s: %w", name, err)
	}
	return n > 0, nil
}

func waitForOnline(ctx context.Context, master *sql.DB, name string, azure bool) error {
	deadline := time.Now().Add(3 * time.Minute)
	reported := false
	for {
		var state string
		err := master.QueryRowContext(ctx,
			"SELECT state_desc FROM sys.databases WHERE name = @p1", name).Scan(&state)
		if err == nil && state == "ONLINE" {
			return nil
		}
		if !azure {
			// A local server creates the database synchronously; anything else
			// is a real problem rather than something to wait out.
			if err != nil {
				return fmt.Errorf("database %s was created but cannot be read back: %w", name, err)
			}
			return fmt.Errorf("database %s is %s, not ONLINE", name, state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database %s did not come online within 3 minutes", name)
		}
		if !reported {
			logf("waiting for %s to finish provisioning...", name)
			reported = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
