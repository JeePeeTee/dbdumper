// Package spool holds the partially finished state of an export.
//
// A dump goes to a work directory first and is packaged into the archive only
// once every table is present. That is what makes an interrupted export
// resumable: archive/zip cannot append to a finished file, so writing tables
// straight into it leaves nothing that a later run can build on.
//
// Table data is spooled as a raw DEFLATE stream together with the CRC and sizes
// a zip entry header needs, so packaging splices the bytes in as they are.
// Nothing is compressed twice, and the work directory is about the size of the
// archive it will become rather than the size of the uncompressed rows.
package spool

import (
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FormatVersion guards against a work directory left by an incompatible build.
//
// 2: spool files are named after the piece they hold rather than by position.
// A version 1 directory cannot be resumed, because its names mean something
// else and adopting them would mix two numbering schemes in one directory.
const FormatVersion = 2

// Meta identifies the export a work directory belongs to.
type Meta struct {
	FormatVersion int       `json:"formatVersion"`
	Tool          string    `json:"tool"`
	StartedAt     time.Time `json:"startedAt"`
	Server        string    `json:"server"`
	Database      string    `json:"database"`
	// Fingerprint covers the source schema and the row filters in force. A
	// resume against a database whose shape has changed, or under a different
	// --where, would mix rows selected two different ways, so it is refused.
	Fingerprint string `json:"fingerprint"`
}

// TableState is what packaging needs to turn a spooled table into a zip entry.
type TableState struct {
	// Identity is "schema.table", lower-cased; it is how a resume recognises
	// work already done, independent of the order tables are visited in.
	Identity string `json:"identity"`
	// Entry is the name the rows will have inside the archive.
	Entry string `json:"entry"`
	// File is the spooled DEFLATE stream, relative to the work directory.
	File             string `json:"file"`
	Rows             int64  `json:"rows"`
	UncompressedSize uint64 `json:"uncompressedSize"`
	CompressedSize   uint64 `json:"compressedSize"`
	CRC32            uint32 `json:"crc32"`
}

// Spool is a work directory.
type Spool struct {
	dir string
}

const (
	metaName    = "meta.json"
	tablesDir   = "tables"
	dataSuffix  = ".deflate"
	stateSuffix = ".json"
)

// DirFor returns the work directory that belongs beside an output archive.
func DirFor(out string) string { return out + ".part" }

// Exists reports whether a work directory is present.
func Exists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// Create prepares an empty work directory, replacing anything already there.
func Create(dir string, m Meta) (*Spool, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, tablesDir), 0o755); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir}
	m.FormatVersion = FormatVersion
	if err := writeJSONAtomic(filepath.Join(dir, metaName), m); err != nil {
		return nil, err
	}
	return s, nil
}

// Resume opens an existing work directory, refusing it if it does not belong to
// the export about to run.
func Resume(dir string, want Meta) (*Spool, error) {
	var have Meta
	if err := readJSON(filepath.Join(dir, metaName), &have); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, metaName), err)
	}
	switch {
	case have.FormatVersion != FormatVersion:
		return nil, fmt.Errorf("the interrupted export in %s was written by an incompatible version (%d, not %d); use --restart",
			dir, have.FormatVersion, FormatVersion)
	case !strings.EqualFold(have.Database, want.Database):
		return nil, fmt.Errorf("the interrupted export in %s is of database %q, not %q; use --restart",
			dir, have.Database, want.Database)
	case have.Fingerprint != want.Fingerprint:
		return nil, fmt.Errorf("%q no longer matches the interrupted export in %s: its schema has changed, or the --where filters differ from the ones that run used; use --restart",
			want.Database, dir)
	}
	return &Spool{dir: dir}, nil
}

// Completed lists the tables already spooled, keyed by identity.
//
// A table counts as done only once its state file exists, and that is written
// after its data has been flushed, so a run killed mid-table simply redoes it.
func (s *Spool) Completed() (map[string]TableState, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, tablesDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]TableState{}, nil
		}
		return nil, err
	}

	out := map[string]TableState{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), stateSuffix) ||
			strings.HasSuffix(e.Name(), ".plan"+stateSuffix) {
			continue
		}
		var st TableState
		if err := readJSON(filepath.Join(s.dir, tablesDir, e.Name()), &st); err != nil {
			// A truncated state file means the run died while writing it; the
			// table is simply not done.
			continue
		}
		// Trust the record only if its data is still there and the right size.
		fi, err := os.Stat(filepath.Join(s.dir, st.File))
		if err != nil || uint64(fi.Size()) != st.CompressedSize {
			continue
		}
		out[st.Identity] = st
	}
	return out, nil
}

// SavePlan records how a table was divided, so that a resume reads the
// remaining chunks with the boundaries the first run used. Boundaries come from
// a sample of live data, so recomputing them would shift the ranges and leave
// gaps between chunks written before the interruption and chunks written after.
func (s *Spool) SavePlan(identity string, v any) error {
	return writeJSONAtomic(s.planPath(identity), v)
}

// LoadPlan reads back a plan saved earlier, reporting whether there was one.
func (s *Spool) LoadPlan(identity string, v any) (bool, error) {
	err := readJSON(s.planPath(identity), v)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Spool) planPath(identity string) string {
	return filepath.Join(s.dir, tablesDir, fileBase(identity)+".plan"+stateSuffix)
}

// fileBase turns a piece's identity into the stem of its files.
//
// Naming files after what they hold, rather than after the position of the
// table in a run's list, is what makes a work directory safe to resume under
// different options. Position is not a property of the data: excluding one
// table's rows, or splitting a table into a different number of ranges, shifts
// every position after it, and the next run then writes over files the last one
// committed under a name that no longer means the same thing.
//
// The digest is truncated to 64 bits, which over any realistic number of tables
// is far below the chance of the disk lying about the write.
func fileBase(identity string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(identity)))
	return hex.EncodeToString(sum[:8])
}

// TableWriter accumulates one table's rows.
type TableWriter struct {
	s        *Spool
	st       TableState
	f        *os.File
	fw       *flate.Writer
	crc      uint32
	raw      int64
	finished bool
}

// NewTable starts spooling a piece. Its files are named after its identity, so
// pieces may be visited in any order and a resume under different options
// cannot land on a file that belongs to something else.
func (s *Spool) NewTable(identity, entry string) (*TableWriter, error) {
	rel := filepath.Join(tablesDir, fileBase(identity)+dataSuffix)
	f, err := os.Create(filepath.Join(s.dir, rel))
	if err != nil {
		return nil, err
	}
	fw, err := flate.NewWriter(f, flate.DefaultCompression)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &TableWriter{
		s:  s,
		st: TableState{Identity: identity, Entry: entry, File: rel},
		f:  f,
		fw: fw,
	}, nil
}

// Write takes uncompressed bytes.
func (w *TableWriter) Write(p []byte) (int, error) {
	n, err := w.fw.Write(p)
	// The CRC and the uncompressed length belong to the zip entry header, and
	// have to be computed from what went in, not what came out.
	w.crc = crc32.Update(w.crc, crc32.IEEETable, p[:n])
	w.raw += int64(n)
	return n, err
}

// Commit finishes the table and records it as done. Until this returns, a
// resume will redo the table from the beginning.
func (w *TableWriter) Commit(rows int64) (TableState, error) {
	if err := w.fw.Close(); err != nil {
		w.f.Close()
		return w.st, err
	}
	// Flush to the platter before the state file claims the data is there;
	// otherwise a power cut leaves a state file pointing at a short stream.
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return w.st, err
	}
	size, err := w.f.Seek(0, io.SeekCurrent)
	if err != nil {
		w.f.Close()
		return w.st, err
	}
	if err := w.f.Close(); err != nil {
		return w.st, err
	}
	w.finished = true

	w.st.Rows = rows
	w.st.CRC32 = w.crc
	w.st.UncompressedSize = uint64(w.raw)
	w.st.CompressedSize = uint64(size)

	stateFile := strings.TrimSuffix(filepath.Join(w.s.dir, w.st.File), dataSuffix) + stateSuffix
	if err := writeJSONAtomic(stateFile, w.st); err != nil {
		return w.st, err
	}
	return w.st, nil
}

// Abort discards a table left half-written.
func (w *TableWriter) Abort() {
	if w.finished {
		return
	}
	w.f.Close()
	os.Remove(filepath.Join(w.s.dir, w.st.File))
}

// OpenData opens a spooled table's DEFLATE stream for packaging.
func (s *Spool) OpenData(st TableState) (io.ReadCloser, error) {
	return os.Open(filepath.Join(s.dir, st.File))
}

// DropData removes a spooled table once it is safely inside the archive, so
// that packaging does not need room for a second full copy of the data.
func (s *Spool) DropData(st TableState) {
	os.Remove(filepath.Join(s.dir, st.File))
}

// Discard removes the whole work directory.
func (s *Spool) Discard() error { return os.RemoveAll(s.dir) }

// Dir is the work directory's path, for messages.
func (s *Spool) Dir() string { return s.dir }

// TableShape is the part of a table that the fingerprint covers.
type TableShape struct {
	Schema, Name string
	Columns      []string
}

// Fingerprint summarises the shape of what is being dumped: the tables and
// their columns, in a stable order. It deliberately ignores row counts, which
// change constantly and would make every resume impossible.
func Fingerprint(database string, tables []TableShape) string {
	lines := make([]string, 0, len(tables))
	for _, t := range tables {
		cols := append([]string(nil), t.Columns...)
		sort.Strings(cols)
		lines = append(lines, strings.ToLower(t.Schema+"."+t.Name)+":"+strings.Join(cols, ","))
	}
	sort.Strings(lines)
	h := crc32.NewIEEE()
	io.WriteString(h, strings.ToLower(database)+"\n")
	for _, l := range lines {
		io.WriteString(h, l+"\n")
	}
	return fmt.Sprintf("%08x-%d", h.Sum32(), len(lines))
}

func writeJSONAtomic(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}
