package export

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/spool"
)

// TestFailedPackagingLeavesTheOutputAlone - the archive used to be written in
// place, so a packaging failure had already truncated the archive --force was
// replacing, and left a torn file that then blocked a --resume.
func TestFailedPackagingLeavesTheOutputAlone(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "db.dbdump")
	previous := []byte("the archive from last night")
	if err := os.WriteFile(out, previous, 0o644); err != nil {
		t.Fatal(err)
	}

	sp, err := spool.Create(spool.DirFor(out), spool.Meta{Database: "db"})
	if err != nil {
		t.Fatal(err)
	}
	// A table the model says has rows, with nothing spooled for it: packaging
	// fails part way, after the archive has been opened.
	dbm := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "T", DataFile: "data/dbo.T.jsonl"},
	}}
	if err := packageArchive(sp, map[string]spool.TableState{}, dbm, model.Source{}, Options{Out: out}); err == nil {
		t.Fatal("packaging with no spooled data should fail")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the previous archive is gone: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("the previous archive was overwritten: now %d bytes", len(got))
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the half-built archive was left behind: %v", err)
	}
}
