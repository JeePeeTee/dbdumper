package spool

import (
	"bytes"
	"compress/flate"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSpool(t *testing.T) (*Spool, Meta) {
	t.Helper()
	m := Meta{Tool: "dbdumper", Server: "srv", Database: "AppDb", Fingerprint: "abc-3"}
	s, err := Create(filepath.Join(t.TempDir(), "out.dbdump.part"), m)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

// spoolOne writes a table and commits it, returning what packaging would need.
func spoolOne(t *testing.T, s *Spool, identity, entry string, payload []byte) TableState {
	t.Helper()
	w, err := s.NewTable(identity, entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	st, err := w.Commit(int64(bytes.Count(payload, []byte("\n"))))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestSpooledDataRoundTrips - the whole scheme rests on the spooled bytes being
// a valid DEFLATE stream whose recorded CRC and length match, since packaging
// writes the zip entry header from those numbers rather than from the data.
func TestSpooledDataRoundTrips(t *testing.T) {
	s, _ := newSpool(t)
	payload := []byte(strings.Repeat(`{"a":1,"b":"text"}`+"\n", 1000))

	st := spoolOne(t, s, "dbo.t", "data/dbo.t.jsonl", payload)

	if st.UncompressedSize != uint64(len(payload)) {
		t.Errorf("UncompressedSize = %d, want %d", st.UncompressedSize, len(payload))
	}
	if st.CompressedSize >= uint64(len(payload)) {
		t.Errorf("CompressedSize %d did not compress %d bytes", st.CompressedSize, len(payload))
	}
	if st.Rows != 1000 {
		t.Errorf("Rows = %d, want 1000", st.Rows)
	}

	rc, err := s.OpenData(st)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(flate.NewReader(rc))
	if err != nil {
		t.Fatalf("spooled stream is not valid DEFLATE: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip changed the data: %d bytes out, %d in", len(got), len(payload))
	}
}

// TestUncommittedTableIsNotResumed - a table interrupted mid-write must be
// redone, or the archive would contain a truncated one.
func TestUncommittedTableIsNotResumed(t *testing.T) {
	s, _ := newSpool(t)
	spoolOne(t, s, "dbo.done", "data/dbo.done.jsonl", []byte("row\n"))

	// Start a second table and walk away, as a kill -9 would.
	w, err := s.NewTable("dbo.halfway", "data/dbo.halfway.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}

	done, err := s.Completed()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := done["dbo.halfway"]; ok {
		t.Error("a table with no state file must not count as finished")
	}
	if _, ok := done["dbo.done"]; !ok {
		t.Error("the committed table should be resumable")
	}
	if len(done) != 1 {
		t.Errorf("got %d completed tables, want 1", len(done))
	}

	// Release the handle. Windows refuses to delete a file that is still open,
	// which is why the export defers Abort rather than relying on process exit.
	w.Abort()
	if _, err := os.Stat(filepath.Join(s.Dir(), w.st.File)); !os.IsNotExist(err) {
		t.Errorf("Abort should remove the half-written data file: %v", err)
	}
}

// TestTruncatedDataIsNotResumed - if the state file survived but its data did
// not, the record has to be distrusted.
func TestTruncatedDataIsNotResumed(t *testing.T) {
	s, _ := newSpool(t)
	st := spoolOne(t, s, "dbo.t", "data/dbo.t.jsonl", []byte(strings.Repeat("row\n", 500)))

	f := filepath.Join(s.Dir(), st.File)
	if err := os.Truncate(f, int64(st.CompressedSize)-10); err != nil {
		t.Fatal(err)
	}
	done, err := s.Completed()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := done["dbo.t"]; ok {
		t.Error("a short data file must not count as finished")
	}
}

func TestResumeRefusesAForeignWorkDirectory(t *testing.T) {
	s, m := newSpool(t)

	if _, err := Resume(s.Dir(), m); err != nil {
		t.Errorf("resuming its own export should work: %v", err)
	}

	other := m
	other.Database = "SomethingElse"
	if _, err := Resume(s.Dir(), other); err == nil {
		t.Error("resuming a different database should be refused")
	} else if !strings.Contains(err.Error(), "--restart") {
		t.Errorf("the error should say how to proceed: %v", err)
	}

	changed := m
	changed.Fingerprint = "different-9"
	if _, err := Resume(s.Dir(), changed); err == nil {
		t.Error("resuming after a schema change should be refused")
	}
}

// TestFingerprintTracksShapeNotOrder - the fingerprint must survive a different
// ordering of the same schema, or no resume would ever be accepted, while still
// catching a column that came or went.
func TestFingerprintTracksShapeNotOrder(t *testing.T) {
	a := []TableShape{
		{Schema: "dbo", Name: "A", Columns: []string{"id", "name"}},
		{Schema: "dbo", Name: "B", Columns: []string{"x"}},
	}
	reordered := []TableShape{
		{Schema: "dbo", Name: "B", Columns: []string{"x"}},
		{Schema: "dbo", Name: "A", Columns: []string{"name", "id"}},
	}
	if Fingerprint("db", a) != Fingerprint("db", reordered) {
		t.Error("ordering of tables or columns must not change the fingerprint")
	}

	for _, changed := range [][]TableShape{
		{{Schema: "dbo", Name: "A", Columns: []string{"id", "name", "extra"}}, {Schema: "dbo", Name: "B", Columns: []string{"x"}}},
		{{Schema: "dbo", Name: "A", Columns: []string{"id"}}, {Schema: "dbo", Name: "B", Columns: []string{"x"}}},
		{{Schema: "dbo", Name: "A", Columns: []string{"id", "name"}}},
		{{Schema: "other", Name: "A", Columns: []string{"id", "name"}}, {Schema: "dbo", Name: "B", Columns: []string{"x"}}},
	} {
		if Fingerprint("db", a) == Fingerprint("db", changed) {
			t.Errorf("a schema change went unnoticed: %+v", changed)
		}
	}

	if Fingerprint("db", a) == Fingerprint("otherdb", a) {
		t.Error("the database name should be part of the fingerprint")
	}
}

func TestCreateClearsWhatWasThere(t *testing.T) {
	s, m := newSpool(t)
	spoolOne(t, s, "dbo.t", "data/dbo.t.jsonl", []byte("row\n"))

	again, err := Create(s.Dir(), m)
	if err != nil {
		t.Fatal(err)
	}
	done, err := again.Completed()
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Errorf("Create should start from nothing, found %d tables", len(done))
	}
}

func TestDiscardRemovesEverything(t *testing.T) {
	s, _ := newSpool(t)
	spoolOne(t, s, "dbo.t", "data/dbo.t.jsonl", []byte("row\n"))
	if err := s.Discard(); err != nil {
		t.Fatal(err)
	}
	if Exists(s.Dir()) {
		t.Error("the work directory should be gone")
	}
}

func TestDirForSitsBesideTheArchive(t *testing.T) {
	if got, want := DirFor("/tmp/db.dbdump"), "/tmp/db.dbdump.part"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
