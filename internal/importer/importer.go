// Package importer restores a .dbdump archive into a SQL Server database.
package importer

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/plan"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// Options controls a restore.
type Options struct {
	SchemaOnly bool
	DataOnly   bool
	Include    []string
	Exclude    []string

	// BatchRows caps how many rows go into one multi-row INSERT. The real
	// batch size is also bounded by SQL Server's 2100-parameter limit.
	BatchRows int
	// CommitRows is how many rows are loaded per transaction.
	CommitRows int
	// Parallel is how many tables are loaded concurrently.
	Parallel int
	// NoBulk forces every table through the INSERT path instead of using bulk
	// copy where the column types allow it.
	NoBulk bool

	// ContinueOnError logs and skips statements that fail instead of aborting.
	ContinueOnError bool

	Log  func(format string, args ...any)
	Warn func(format string, args ...any)
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// Result summarizes a completed restore.
type Result struct {
	Tables     int
	Rows       int64
	Failed     int
	Duration   time.Duration
	FailedList []string
}

// Run restores ar into db.
func Run(ctx context.Context, db *sql.DB, ar *archive.Reader, opts Options) (*Result, error) {
	start := time.Now()
	if opts.BatchRows <= 0 {
		opts.BatchRows = 500
	}
	if opts.CommitRows <= 0 {
		opts.CommitRows = 20000
	}
	if opts.Parallel <= 0 {
		opts.Parallel = 4
	}

	dbm := &ar.Manifest.Database
	res := &Result{}

	keep := sqlsrv.NewTableFilter(opts.Include, opts.Exclude)
	tables := make([]model.Table, 0, len(dbm.Tables))
	for _, t := range dbm.Tables {
		if keep == nil || keep(t.Schema, t.Name) {
			tables = append(tables, t)
		}
	}
	res.Tables = len(tables)

	if !opts.DataOnly {
		for _, ph := range plan.SchemaPhases(dbm) {
			if err := runPhase(ctx, db, ph, opts, res); err != nil {
				return res, err
			}
		}
	}

	if !opts.SchemaOnly {
		if ar.Manifest.Source.SchemaOnly {
			opts.warn("archive was created with --schema-only; there is no data to load")
		} else {
			if err := loadAllData(ctx, db, ar, tables, opts, res); err != nil {
				return res, err
			}
		}
	}

	if !opts.DataOnly {
		for _, ph := range plan.PostDataPhases(dbm) {
			if err := runPhase(ctx, db, ph, opts, res); err != nil {
				return res, err
			}
		}
	}

	res.Duration = time.Since(start)
	return res, nil
}

// runPhase executes a phase, retrying the statements marked Retryable until no
// further progress is made. That resolves ordering between views, functions and
// computed columns without needing a perfect dependency graph.
func runPhase(ctx context.Context, db *sql.DB, ph plan.Phase, opts Options, res *Result) error {
	if len(ph.Stmts) == 0 {
		return nil
	}
	opts.log("%s (%d)...", ph.Name, len(ph.Stmts))

	pending := ph.Stmts
	var lastErrs []error

	for round := 0; ; round++ {
		var deferred []plan.Stmt
		lastErrs = lastErrs[:0]

		for _, s := range pending {
			if _, err := db.ExecContext(ctx, s.SQL); err != nil {
				if s.Retryable {
					deferred = append(deferred, s)
					lastErrs = append(lastErrs, fmt.Errorf("%s: %w", s.Describe, err))
					continue
				}
				if !opts.ContinueOnError {
					return fmt.Errorf("%s: %w", s.Describe, err)
				}
				opts.warn("failed: %s: %v", s.Describe, err)
				res.Failed++
				res.FailedList = append(res.FailedList, s.Describe)
			}
		}

		if len(deferred) == 0 {
			return nil
		}
		if len(deferred) == len(pending) {
			// No progress this round: these really are broken.
			for i, s := range deferred {
				if !opts.ContinueOnError {
					return lastErrs[i]
				}
				opts.warn("failed: %s: %v", s.Describe, lastErrs[i])
				res.Failed++
				res.FailedList = append(res.FailedList, s.Describe)
			}
			return nil
		}
		pending = deferred
	}
}

func loadAllData(ctx context.Context, db *sql.DB, ar *archive.Reader, tables []model.Table, opts Options, res *Result) error {
	// In data-only mode the schema already exists, so constraints and triggers
	// are live. Quiet them for the duration of the load.
	if opts.DataOnly {
		for _, t := range tables {
			exec(ctx, db, "ALTER TABLE "+t.QualifiedName()+" NOCHECK CONSTRAINT ALL", opts)
			exec(ctx, db, "DISABLE TRIGGER ALL ON "+t.QualifiedName(), opts)
		}
		defer func() {
			// Deliberately detached from ctx: on Ctrl+C it is already
			// cancelled, and re-enabling through it would fail for every
			// table, leaving the target database with its constraints
			// unchecked and its triggers off - a state this function created
			// and must undo whether the load finished or not.
			restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()

			var failed []string
			for _, t := range tables {
				if !execOK(restoreCtx, db, "ENABLE TRIGGER ALL ON "+t.QualifiedName(), opts) ||
					!execOK(restoreCtx, db, "ALTER TABLE "+t.QualifiedName()+" WITH CHECK CHECK CONSTRAINT ALL", opts) {
					failed = append(failed, t.Schema+"."+t.Name)
				}
			}
			if len(failed) > 0 {
				// Loud, because the database is left in a state the user did
				// not ask for and cannot see without looking.
				opts.warn("could not re-enable triggers or constraints on %d table(s): %s",
					len(failed), strings.Join(failed, ", "))
				opts.warn("those tables are left with constraints unchecked; re-run the import or fix them by hand")
			}
		}()
	}

	// Foreign keys are created after the data, so table load order does not
	// matter and tables can go in parallel. Largest first, so the long pole
	// starts early and short tables fill in behind it.
	todo := make([]model.Table, 0, len(tables))
	for _, t := range tables {
		if t.DataFile != "" && ar.HasEntry(t.DataFile) {
			todo = append(todo, t)
		}
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].RowCount > todo[j].RowCount })

	workers := opts.Parallel
	if workers < 1 {
		workers = 1
	}
	if workers > len(todo) {
		workers = len(todo)
	}
	if workers == 0 {
		return nil
	}

	var (
		mu       sync.Mutex
		firstErr error
		next     int32 = -1
	)
	loadCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt32(&next, 1))
				if i >= len(todo) {
					return
				}
				t := todo[i]
				n, err := loadTable(loadCtx, db, ar, t, opts)

				mu.Lock()
				res.Rows += n
				if err != nil {
					if opts.ContinueOnError {
						opts.warn("failed to load %s.%s: %v", t.Schema, t.Name, err)
						res.Failed++
						res.FailedList = append(res.FailedList, fmt.Sprintf("data %s.%s", t.Schema, t.Name))
					} else if firstErr == nil {
						firstErr = fmt.Errorf("load %s.%s: %w", t.Schema, t.Name, err)
						cancel()
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func exec(ctx context.Context, db *sql.DB, q string, opts Options) {
	execOK(ctx, db, q, opts)
}

// execOK runs a best-effort statement and reports whether it succeeded.
func execOK(ctx context.Context, db *sql.DB, q string, opts Options) bool {
	if _, err := db.ExecContext(ctx, q); err != nil {
		opts.warn("%s: %v", q, err)
		return false
	}
	return true
}
