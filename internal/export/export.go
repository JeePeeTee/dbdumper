// Package export dumps a live SQL Server database into a .dbdump archive.
package export

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/plan"
	"github.com/JeePeeTee/dbdumper/internal/spool"
	"github.com/JeePeeTee/dbdumper/internal/sqlsrv"
)

// Options controls a dump.
type Options struct {
	Out        string
	SchemaOnly bool
	Include    []string // glob patterns, schema.table; empty means everything
	Exclude    []string
	// Where holds "<table-glob>:<predicate>" specifications restricting which
	// rows of a table are dumped.
	Where []string
	// ExcludeData names tables whose definition is kept but whose rows are
	// skipped. Unlike Exclude, the table, its indexes and its constraints are
	// still created on restore - it just comes back empty.
	ExcludeData []string
	// Parallel is how many pieces of work are read concurrently.
	Parallel int
	// ChunkMinBytes is the source size above which a table is split into
	// ranges that can be read at the same time. Zero uses the default; a
	// negative value turns splitting off.
	ChunkMinBytes int64
	// SchemaDir, when set, additionally writes the DDL as one file per object
	// into that directory, deleting the files of objects that no longer exist.
	// Intended for keeping a schema in version control, where a change to one
	// object should be a change to one file.
	SchemaDir string
	// Deterministic makes two exports of an unchanged database produce
	// byte-identical archives: rows are read in primary key order, the
	// manifest records no creation time, and tables are not split into ranges,
	// since range boundaries come from a live sample and would fall in
	// different places on a second run.
	//
	// It costs a sorted read of every table, which is why it is not the
	// default. Reading tables concurrently is unaffected and stays on.
	Deterministic bool
	// Resume continues an interrupted export from its work directory instead
	// of starting over. Restart discards that directory. With neither, an
	// existing work directory is an error rather than a silent choice.
	Resume  bool
	Restart bool

	// ProgressInterval is how often a long-running table reports progress.
	// Zero means the 5s default; negative disables per-table progress.
	ProgressInterval time.Duration
	// Log receives permanent lines. Optional.
	Log func(format string, args ...any)
	// Progress receives transient status updates that the caller is free to
	// render in place and overwrite. Optional. It is handed structured data
	// rather than a formatted string so that all display decisions - bar
	// width, truncation, terminal or not - stay with the caller.
	Progress func(Progress)
	// Warn receives non-fatal problems. Optional.
	Warn func(format string, args ...any)
}

func (o Options) log(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Progress describes how far the current table has got.
type Progress struct {
	Table      string // "schema.table"
	TableIndex int    // 1-based position in the dump
	TableCount int
	Rows       int64
	// EstimatedRows is the row count expected for this table, or 0 when the
	// server would not tell us. Progress can exceed it: the estimate is read
	// before the dump and rows can be inserted while it runs.
	EstimatedRows int64
	// TableBytes is this table's source size, the share of TotalBytes it
	// represents.
	TableBytes int64
	Bytes      int64
	Elapsed    time.Duration
	// InFlight is how many tables are being read at this moment. Above one,
	// Table names the one running longest - what the run is waiting on.
	InFlight int

	// The export as a whole. Work is counted in the source bytes reported by
	// the engine, not in rows and not in the bytes written: rows per second
	// varies tenfold between a table of integers and a table of blobs, and the
	// bytes written differ from the bytes read by a ratio that depends on the
	// column types. Source bytes per second is the one measure that stays
	// roughly comparable across tables, and it calibrates itself as the run
	// proceeds.
	//
	// TotalBytes covers only what this run will read: tables resumed from an
	// earlier run cost nothing now and are excluded from both totals.
	TotalBytes   int64
	DoneBytes    int64
	TotalElapsed time.Duration
}

// OverallFraction returns how far the whole export has got, in 0..1, and
// whether that is meaningful.
func (p Progress) OverallFraction() (float64, bool) {
	if p.TotalBytes <= 0 {
		return 0, false
	}
	done := float64(p.DoneBytes)
	// Credit the table in flight for the share of itself it has read, so the
	// figure moves during a long table instead of jumping when it finishes.
	if f, ok := p.Fraction(); ok {
		done += f * float64(p.currentTableBytes())
	}
	frac := done / float64(p.TotalBytes)
	if frac > 1 {
		frac = 1
	}
	return frac, true
}

// currentTableBytes is the source size of the table being read, which is the
// part of TotalBytes that DoneBytes does not yet account for.
func (p Progress) currentTableBytes() int64 { return p.TableBytes }

// OverallETA extrapolates the whole export from the rate achieved so far.
func (p Progress) OverallETA() (time.Duration, bool) {
	frac, ok := p.OverallFraction()
	if !ok || frac <= 0 || p.TotalElapsed < 2*time.Second {
		return 0, false
	}
	rate := frac / p.TotalElapsed.Seconds() // fraction per second
	if rate <= 0 {
		return 0, false
	}
	remaining := (1 - frac) / rate
	return time.Duration(remaining) * time.Second, true
}

// Fraction returns how far along this table is, in 0..1, and whether that is
// meaningful at all.
func (p Progress) Fraction() (float64, bool) {
	if p.EstimatedRows <= 0 {
		return 0, false
	}
	f := float64(p.Rows) / float64(p.EstimatedRows)
	if f > 1 {
		f = 1
	}
	return f, true
}

// RowsPerSecond is the average rate over this table so far.
func (p Progress) RowsPerSecond() float64 {
	if p.Elapsed <= 0 {
		return 0
	}
	return float64(p.Rows) / p.Elapsed.Seconds()
}

// ETA estimates the time left on this table by extrapolating the average rate.
// It reports false when there is no estimate to work from, or too little
// history for the extrapolation to mean anything.
func (p Progress) ETA() (time.Duration, bool) {
	if p.EstimatedRows <= 0 || p.Rows <= 0 || p.Elapsed < time.Second {
		return 0, false
	}
	remaining := p.EstimatedRows - p.Rows
	if remaining <= 0 {
		return 0, true
	}
	rate := p.RowsPerSecond()
	if rate <= 0 {
		return 0, false
	}
	return time.Duration(float64(remaining)/rate) * time.Second, true
}

func (o Options) progress(p Progress) {
	if o.Progress != nil {
		o.Progress(p)
	}
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// Result summarizes a completed dump.
type Result struct {
	Tables int
	Rows   int64
	// DataBytes is the uncompressed size of the JSONL written across all
	// tables. It is not the archive's size on disk: the archive is deflated
	// and also holds the manifest and the DDL scripts.
	DataBytes int64
	// ResumedTables counts tables taken from an interrupted run's work
	// directory rather than read from the database again.
	ResumedTables int
	// SchemaFilesWritten and SchemaFilesRemoved report what --schema-dir did.
	// Written counts only files whose contents changed.
	SchemaFilesWritten int
	SchemaFilesRemoved int
	Duration           time.Duration
}

// Run performs the dump.
func Run(ctx context.Context, db *sql.DB, opts Options) (*Result, error) {
	start := time.Now()

	if opts.Deterministic {
		// Range boundaries are drawn from a sample of live data, so a second
		// run would split the same table in different places and the entries
		// would differ even though the rows did not.
		opts.ChunkMinBytes = -1
	}

	in := &sqlsrv.Introspector{
		DB:     db,
		Filter: sqlsrv.NewTableFilter(opts.Include, opts.Exclude),
		Warn:   opts.warn,
	}

	opts.log("reading schema...")
	src, err := in.Source(ctx)
	if err != nil {
		return nil, err
	}
	src.SchemaOnly = opts.SchemaOnly

	dbm, err := in.Database(ctx)
	if err != nil {
		return nil, err
	}
	opts.log("found %d schemas, %d tables, %d modules, %d sequences, %d user types",
		len(dbm.Schemas), len(dbm.Tables), len(dbm.Modules), len(dbm.Sequences), len(dbm.UserTypes))

	rowFilter, err := sqlsrv.NewRowFilter(opts.Where, opts.warn)
	if err != nil {
		return nil, err
	}
	// Recorded on the model before the fingerprint is taken, so that changing a
	// --where between runs makes a resume refuse rather than mix two filters.
	if rowFilter != nil {
		for i := range dbm.Tables {
			t := &dbm.Tables[i]
			t.RowFilter = rowFilter(t.Schema, t.Name)
		}
	}

	res := &Result{Tables: len(dbm.Tables)}

	// Rows go to a work directory first. archive/zip cannot append to a
	// finished file, so writing them straight into the archive would leave an
	// interrupted run with nothing a later one could continue from.
	sp, states, err := openSpool(dbm, src, opts)
	if err != nil {
		return nil, err
	}

	if !opts.SchemaOnly {
		if err := spoolTables(ctx, db, sp, states, dbm, res, opts); err != nil {
			return nil, err
		}
	}

	warnUntrustedKeys(dbm, opts)

	if opts.SchemaDir != "" {
		sd, err := writeSchemaDir(opts.SchemaDir, dbm, opts)
		if err != nil {
			return nil, fmt.Errorf("write schema directory %s: %w", opts.SchemaDir, err)
		}
		res.SchemaFilesWritten, res.SchemaFilesRemoved = sd.Written, sd.Removed
		opts.log("schema directory %s: %d file(s) written, %d removed",
			opts.SchemaDir, sd.Written, sd.Removed)
	}

	if err := packageArchive(sp, states, dbm, src, opts); err != nil {
		return nil, err
	}
	if err := sp.Discard(); err != nil {
		opts.warn("could not remove the work directory %s: %v", sp.Dir(), err)
	}

	res.Duration = time.Since(start)
	return res, nil
}

// openSpool creates or resumes the work directory and reports what an earlier
// run already finished.
func openSpool(dbm *model.Database, src model.Source, opts Options) (*spool.Spool, map[string]spool.TableState, error) {
	dir := spool.DirFor(opts.Out)
	meta := spool.Meta{
		Tool:        "dbdumper",
		StartedAt:   time.Now().UTC(),
		Server:      src.Server,
		Database:    src.Database,
		Fingerprint: fingerprint(src.Database, dbm),
	}

	if spool.Exists(dir) && !opts.Restart {
		if !opts.Resume {
			return nil, nil, fmt.Errorf("an interrupted export is present in %s; pass --resume to continue it, or --restart to discard it", dir)
		}
		sp, err := spool.Resume(dir, meta)
		if err != nil {
			return nil, nil, err
		}
		states, err := sp.Completed()
		if err != nil {
			return nil, nil, err
		}
		if len(states) > 0 {
			opts.log("resuming: %d table(s) already dumped in %s", len(states), dir)
		}
		return sp, states, nil
	}

	if opts.Resume && !spool.Exists(dir) {
		opts.warn("--resume was given but there is no interrupted export in %s; starting fresh", dir)
	}
	sp, err := spool.Create(dir, meta)
	if err != nil {
		return nil, nil, err
	}
	return sp, map[string]spool.TableState{}, nil
}

// spoolTables writes every table not already present in the work directory.
// DefaultChunkMinBytes is the source size above which a table is worth reading
// in several ranges at once. Below it the planning query and the extra
// connections cost more than the concurrency wins.
const DefaultChunkMinBytes = 128 << 20 // 128 MB

func spoolTables(ctx context.Context, db *sql.DB, sp *spool.Spool, states map[string]spool.TableState,
	dbm *model.Database, res *Result, opts Options) error {

	skipData := sqlsrv.NewTableFilter(opts.ExcludeData, nil)
	in := &sqlsrv.Introspector{DB: db, Warn: opts.warn}

	workers := opts.Parallel
	if workers < 1 {
		workers = 1
	}

	// Decide what this run has to do, and how big it is. Skipped tables cost
	// nothing and resumed ones already did, so neither belongs in the total.
	var pieces []piece
	var totalBytes int64

	for i := range dbm.Tables {
		t := &dbm.Tables[i]
		if skipData != nil && skipData(t.Schema, t.Name) {
			t.DataSkipped = true
			opts.log("  %-50s %10s", t.Schema+"."+t.Name, "(data skipped)")
			continue
		}

		chunks, key := planFor(ctx, in, sp, t, workers, opts)

		if len(chunks) == 0 {
			p := piece{tableIndex: i, pos: i + 1, t: t,
				estRows: t.EstimatedRows, estBytes: progressBytes(t), schedBytes: t.EstimatedBytes}
			if st, done := states[strings.ToLower(p.label())]; done {
				adoptResumed(t, st, res, opts)
				continue
			}
			pieces = append(pieces, p)
			totalBytes += p.estBytes
			continue
		}

		// A split table's rows arrive as several entries, in key order.
		t.DataFiles = make([]string, len(chunks))
		for k := range chunks {
			p := piece{
				tableIndex: i, pos: i + 1, t: t,
				chunk: &chunks[k], key: key.Column.Name,
				estRows:    t.EstimatedRows / int64(len(chunks)),
				estBytes:   progressBytes(t) / int64(len(chunks)),
				schedBytes: t.EstimatedBytes / int64(len(chunks)),
			}
			t.DataFiles[k] = p.entry()
			if st, done := states[strings.ToLower(p.label())]; done {
				t.RowCount += st.Rows
				res.Rows += st.Rows
				res.DataBytes += int64(st.UncompressedSize)
				continue
			}
			pieces = append(pieces, p)
			totalBytes += p.estBytes
		}
		if t.RowCount > 0 && len(pieces) == 0 {
			res.ResumedTables++
		}
	}
	if len(pieces) == 0 {
		return nil
	}

	// Largest first. The run cannot finish before its biggest piece does, so
	// starting that one last would leave the other workers idle. This orders by
	// the table's real size rather than by its progress weight: a filtered table
	// carries no weight but can still take a full scan to read, and scheduling
	// it last would leave exactly the long tail this is meant to avoid.
	sort.SliceStable(pieces, func(a, b int) bool {
		return pieces[a].schedBytes > pieces[b].schedBytes
	})
	if workers > len(pieces) {
		workers = len(pieces)
	}

	tr := newTracker(totalBytes)
	stop := make(chan struct{})
	interval := opts.ProgressInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	go tr.report(opts, len(dbm.Tables), interval, stop)
	defer close(stop)

	var (
		mu       sync.Mutex // guards states, res, the model and firstErr
		firstErr error
		next     int64 = -1
	)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(pieces) {
					return
				}
				p := pieces[i]

				st, err := dumpTable(runCtx, db, sp, p, opts, len(dbm.Tables), tr)

				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("dump %s: %w", p.label(), err)
						cancel()
					}
					mu.Unlock()
					continue
				}
				if st.Entry != "" {
					states[strings.ToLower(p.label())] = st
					// Chunks of one table share its element, so this is under
					// the same lock as everything else that touches the model.
					if p.chunk == nil {
						p.t.DataFile = st.Entry
					}
					p.t.RowCount += st.Rows
					res.Rows += st.Rows
					res.DataBytes += int64(st.UncompressedSize)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// adoptResumed takes a table already present in the work directory.
func adoptResumed(t *model.Table, st spool.TableState, res *Result, opts Options) {
	t.RowCount, t.DataFile = st.Rows, st.Entry
	res.Rows += st.Rows
	res.DataBytes += int64(st.UncompressedSize)
	res.ResumedTables++
	opts.log("  %-50s %10d rows  %10s (resumed)",
		t.Schema+"."+t.Name, st.Rows, humanBytes(int64(st.UncompressedSize)))
}

// planFor decides how a table is read, reusing an earlier run's division when
// one is on record.
//
// The boundaries come from a sample of live data, so recomputing them on a
// resume would shift the ranges: rows between the old boundary and the new one
// would fall into no chunk at all, or into two. The plan is therefore saved
// with the work directory and read back rather than recomputed.
func planFor(ctx context.Context, in *sqlsrv.Introspector, sp *spool.Spool,
	t *model.Table, workers int, opts Options) ([]sqlsrv.Chunk, sqlsrv.ChunkKey) {

	planKey := strings.ToLower(t.Schema + "." + t.Name)
	var saved savedPlan
	if ok, err := sp.LoadPlan(planKey, &saved); err != nil {
		opts.warn("could not read the chunk plan for %s.%s (%v); reading it in one piece",
			t.Schema, t.Name, err)
		return nil, sqlsrv.ChunkKey{}
	} else if ok {
		key, found := in.ChunkKeyFor(ctx, *t)
		if !found || !strings.EqualFold(key.Column.Name, saved.Key) {
			return nil, sqlsrv.ChunkKey{}
		}
		chunks, err := saved.chunks(key)
		if err != nil {
			opts.warn("could not restore the chunk plan for %s.%s (%v); reading it in one piece",
				t.Schema, t.Name, err)
			return nil, sqlsrv.ChunkKey{}
		}
		return chunks, key
	}

	minBytes := opts.ChunkMinBytes
	if minBytes == 0 {
		minBytes = DefaultChunkMinBytes
	}
	if minBytes < 0 || workers < 2 || t.EstimatedBytes < minBytes {
		return nil, sqlsrv.ChunkKey{}
	}
	key, found := in.ChunkKeyFor(ctx, *t)
	if !found {
		return nil, sqlsrv.ChunkKey{}
	}

	// Enough pieces to keep every worker busy on this table alone, since a
	// table big enough to split is usually the one everything waits for.
	n := workers * 2
	chunks, err := in.PlanChunks(ctx, *t, key, n)
	if err != nil || len(chunks) == 0 {
		return nil, sqlsrv.ChunkKey{}
	}
	if err := sp.SavePlan(planKey, newSavedPlan(key, chunks)); err != nil {
		opts.warn("could not record the chunk plan for %s.%s (%v); a resume will read it in one piece",
			t.Schema, t.Name, err)
	}
	opts.log("  %-50s split into %d ranges on [%s]", t.Schema+"."+t.Name, len(chunks), key.Column.Name)
	return chunks, key
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// packageArchive assembles the finished archive from the work directory.
//
// Spooled data is spliced in already compressed, and each table's spool file is
// dropped as soon as it is safely inside, so two full copies of the data never
// exist at once.
func packageArchive(sp *spool.Spool, states map[string]spool.TableState,
	dbm *model.Database, src model.Source, opts Options) error {

	opts.log("packaging %s...", opts.Out)
	w, err := archive.Create(opts.Out)
	if err != nil {
		return err
	}
	defer w.Close()

	for i := range dbm.Tables {
		t := &dbm.Tables[i]
		// Iterate on what this run decided to include, not on what the work
		// directory happens to hold: a table excluded or emptied by this run
		// may still have data spooled by an earlier one.
		for _, entry := range t.DataEntries() {
			st, ok := states[strings.ToLower(entryLabel(t, entry))]
			if !ok {
				return fmt.Errorf("package %s.%s: no spooled data for %s", t.Schema, t.Name, entry)
			}
			if err := copySpooled(w, sp, st); err != nil {
				return fmt.Errorf("package %s: %w", st.Identity, err)
			}
			sp.DropData(st)
		}
	}

	for _, ph := range plan.AllPhases(dbm) {
		if err := w.AddText(ph.File, plan.ScriptFor(ph)); err != nil {
			return err
		}
	}

	manifest := model.Manifest{
		FormatVersion: model.FormatVersion,
		Tool:          "dbdumper",
		CreatedAt:     time.Now().UTC(),
		Source:        src,
		Database:      *dbm,
	}
	if opts.Deterministic {
		// The one field that changes on every run whatever the database does.
		manifest.CreatedAt = time.Time{}
	}
	if err := w.AddJSON(archive.ManifestName, manifest); err != nil {
		return err
	}
	if err := w.AddText("README.txt", readme); err != nil {
		return err
	}
	return w.Close()
}

// entryLabel recovers the spool identity that produced an archive entry.
func entryLabel(t *model.Table, entry string) string {
	for k, e := range t.DataFiles {
		if e == entry {
			return fmt.Sprintf("%s.%s#%d", t.Schema, t.Name, k)
		}
	}
	return t.Schema + "." + t.Name
}

func copySpooled(w *archive.Writer, sp *spool.Spool, st spool.TableState) error {
	src, err := sp.OpenData(st)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := w.AddRaw(archive.RawEntry{
		Name:             st.Entry,
		UncompressedSize: st.UncompressedSize,
		CompressedSize:   st.CompressedSize,
		CRC32:            st.CRC32,
	})
	if err != nil {
		return err
	}
	n, err := io.Copy(dst, src)
	if err != nil {
		return err
	}
	if uint64(n) != st.CompressedSize {
		return fmt.Errorf("spooled data for %s is %d bytes, expected %d", st.Entry, n, st.CompressedSize)
	}
	return nil
}

// fingerprint summarises the source schema so a resume can refuse a database
// whose shape has changed underneath it.
func fingerprint(database string, dbm *model.Database) string {
	shapes := make([]spool.TableShape, 0, len(dbm.Tables))
	for _, t := range dbm.Tables {
		cols := make([]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, c.Name)
		}
		if t.RowFilter != "" {
			// Part of the shape for resume purposes: rows already spooled under
			// one predicate must not be mixed with rows read under another.
			cols = append(cols, "!where="+t.RowFilter)
		}
		shapes = append(shapes, spool.TableShape{Schema: t.Schema, Name: t.Name, Columns: cols})
	}
	return spool.Fingerprint(database, shapes)
}

// warnUntrustedKeys reports the foreign keys that --exclude-data has made
// unverifiable. Restoring them WITH NOCHECK is the only way the restore can
// succeed, but it leaves the target database referentially inconsistent, so it
// should never be a silent consequence.
func warnUntrustedKeys(dbm *model.Database, opts Options) {
	// The same rule the DDL is generated from, rather than a second copy of it:
	// a warning that disagreed with what the restore does would be worse than
	// no warning at all.
	affected := plan.UntrustedForeignKeys(dbm)
	if len(affected) == 0 {
		return
	}
	opts.warn("%d foreign key(s) point at a table that was skipped or filtered; they will be created", len(affected))
	opts.warn("WITH NOCHECK and left untrusted, so the restored database will have dangling references:")
	for i, a := range affected {
		if i >= 10 {
			opts.warn("  ... and %d more", len(affected)-i)
			break
		}
		opts.warn("  %s", a)
	}
	opts.warn("exclude the referencing tables' data too if you need referential integrity")
}

// dumpTable writes one table's rows and reports how many, and how many
// uncompressed bytes they took.
// piece is one unit of reading: a whole table, or one range of a split table.
type piece struct {
	tableIndex int // position in dbm.Tables
	pos        int // 1-based table position, for display
	t          *model.Table
	chunk      *sqlsrv.Chunk // nil for a table read in one piece
	key        string        // chunk key column name
	estRows    int64
	// estBytes is this piece's share of the whole-export progress, and
	// schedBytes its size for ordering. They differ only for a filtered table:
	// see progressBytes.
	estBytes   int64
	schedBytes int64
}

// progressBytes is the share of the whole-export estimate a table represents.
//
// For a filtered table that is zero, not its size. --where can select one row
// in a million, so the table's source size says nothing about how much of it
// will be written, and counting it in full made the bar crawl through the rest
// of the run and then leap when the filtered table finished. There is no cheap
// way to learn the true figure - it would take a COUNT over the predicate,
// which is most of the work of reading the table - so the honest move is to
// leave filtered tables out of the estimate rather than to guess at them.
//
// Both sides of the fraction drop the same tables, so the percentage stays
// truthful about the work it covers. What suffers is the ETA, which runs long
// because time spent on filtered tables is not represented in the numerator.
// A pessimistic ETA is a better failure than a bar that moves backwards.
func progressBytes(t *model.Table) int64 {
	if t.RowFilter != "" {
		return 0
	}
	return t.EstimatedBytes
}

// label names a piece for the log and the spool.
func (p piece) label() string {
	if p.chunk == nil {
		return p.t.Schema + "." + p.t.Name
	}
	return fmt.Sprintf("%s.%s#%d", p.t.Schema, p.t.Name, p.chunk.Index)
}

func (p piece) entry() string {
	if p.chunk == nil {
		return archive.DataPath(p.t.Schema, p.t.Name)
	}
	return archive.ChunkDataPath(p.t.Schema, p.t.Name, p.chunk.Index)
}

func dumpTable(ctx context.Context, db *sql.DB, sp *spool.Spool, pc piece,
	opts Options, total int, tr *tracker) (spool.TableState, error) {

	t, pos := pc.t, pc.pos

	var none spool.TableState
	cols := dataColumns(*t)
	if len(cols) == 0 {
		opts.warn("%s.%s has no insertable columns; skipping data", t.Schema, t.Name)
		return none, nil
	}
	codec := sqlsrv.NewRowCodec(cols)

	entry := pc.entry()
	tw, err := sp.NewTable(strings.ToLower(pc.label()), entry)
	if err != nil {
		return none, err
	}
	// Anything short of Commit leaves the table unfinished, so a resume will
	// simply do it again.
	defer tw.Abort()

	bw := bufio.NewWriterSize(tw, 1<<20)
	cw := &countingWriter{w: bw}
	enc := json.NewEncoder(cw)
	enc.SetEscapeHTML(false)

	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	header := struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
	}{t.Schema + "." + t.Name, names}
	if err := enc.Encode(header); err != nil {
		return none, err
	}

	query := "SELECT " + codec.SelectList() + " FROM " + t.QualifiedName()
	// The predicate is raw T-SQL from the caller's own command line, spliced in
	// as written. There is nothing to escape: it is an expression, not a value,
	// and the caller already controls the connection string.
	estimated := pc.estRows
	var where []string
	var args []any
	if t.RowFilter != "" {
		where = append(where, "("+t.RowFilter+")")
		// The engine's row count is for the whole table, so it would make the
		// bar and the ETA lie. Better no percentage than a wrong one.
		estimated = 0
	}
	if pc.chunk != nil {
		var pred string
		pred, args = pc.chunk.Predicate(pc.key, args)
		where = append(where, pred)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if opts.Deterministic {
		// Without a key there is no total order to ask for, and ordering by
		// every column instead would fail outright on the types SQL Server
		// will not sort. Such a table is left in whatever order it comes back
		// in, and the caller is told which ones those are.
		if order := t.OrderByKey(); order != "" {
			query += " ORDER BY " + order
		} else {
			opts.warn("%s.%s has no primary key, so its row order is not reproducible", t.Schema, t.Name)
		}
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return none, fmt.Errorf("%s: %w", query, err)
	}
	defer rows.Close()

	// Counters the reporter goroutine reads; it decides when to draw, so this
	// loop only has to keep them current.
	run := &tableRun{
		table: pc.label(), index: pos,
		estRows: estimated, estBytes: pc.estBytes,
	}
	tr.begin(run)
	defer tr.finish(run)

	var n int64
	buf := make([]any, 0, len(cols))
	for rows.Next() {
		if err := rows.Scan(codec.ScanDest()...); err != nil {
			return none, err
		}
		if err := enc.Encode(codec.Encode(buf)); err != nil {
			return none, err
		}
		n++

		// Publishing every row would be wasteful and pointless: the reporter
		// only looks every few seconds.
		if n%64 == 0 {
			run.rows.Store(n)
			run.bytes.Store(cw.n)
		}
	}
	run.rows.Store(n)
	run.bytes.Store(cw.n)
	if err := rows.Err(); err != nil {
		return none, err
	}
	if err := bw.Flush(); err != nil {
		return none, err
	}

	st, err := tw.Commit(n)
	if err != nil {
		return none, err
	}
	// A permanent line here replaces whatever transient line was last drawn.
	suffix := ""
	if t.RowFilter != "" {
		suffix = " (filtered)"
	}
	opts.log("  %-50s %10d rows  %10s%s", pc.label(), n, humanBytes(cw.n), suffix)
	return st, nil
}

// countingWriter tallies the uncompressed bytes handed to the archive, so
// progress can be reported in terms a user can compare against the source
// table's size.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
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

func dataColumns(t model.Table) []model.Column {
	out := make([]model.Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if c.Insertable() {
			out = append(out, c)
		}
	}
	return out
}

const readme = `dbdumper archive
================

Layout
  manifest.json          full schema model; this is the authoritative source
  schema/*.sql           the same DDL, rendered for sqlcmd/SSMS (informational)
  data/<schema>.<table>.jsonl
                         first line: {"table":..,"columns":[..]}
                         remaining lines: one JSON array per row, column order
                         matching the header

Value encoding
  bit                    true / false
  integer types          JSON number
  float, real            JSON number
  decimal, numeric,
    money, smallmoney    decimal string, e.g. "12.3400"
  uniqueidentifier       "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
  binary, varbinary,
    image, geography,
    geometry, hierarchyid  base64 string
  vector                 the server's own JSON text, e.g.
                         "[1.5000000e+000,-2.0000000e+000]"
  date                   "2006-01-02"
  time                   "15:04:05.1234567"
  datetime,
    smalldatetime        "2006-01-02T15:04:05.999"
  datetime2              "2006-01-02T15:04:05.1234567"
  datetimeoffset         "2006-01-02T15:04:05.1234567+02:00"
  everything else        JSON string
  NULL                   null

Computed columns and rowversion/timestamp columns are not stored: they are
regenerated by the server on import.

An interrupted export leaves a <archive>.part work directory beside the output.
Re-run the same command with --resume to continue it, or --restart to discard it.

Restore with:
  dbdumper import --server <host> --database <newdb> --create-database --in <this file>
`
