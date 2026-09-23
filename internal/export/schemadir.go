package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
	"github.com/JeePeeTee/dbdumper/internal/plan"
)

// SchemaDirResult reports what a schema directory write changed.
type SchemaDirResult struct {
	Written int
	// Removed counts files deleted because the object they described is no
	// longer in the database.
	Removed int
}

// writeSchemaDir lays the schema out as one file per object, and deletes the
// files of objects that have gone.
//
// The deletion is the point. A directory that only ever gains files records
// every object that has ever existed rather than the ones that do, so a git
// history built on it never shows a drop and slowly fills with dead scripts.
//
// Only files this function could have written are ever removed: inside the
// directories it owns, ending in .sql. Anything else in the tree - a README, a
// .gitattributes, a subdirectory someone added - is left alone. And nothing is
// removed at all by a run filtered with --include or --exclude, which cannot
// tell an object it skipped from one that is gone.
func writeSchemaDir(dir string, dbm *model.Database, opts Options) (SchemaDirResult, error) {
	var res SchemaDirResult

	scripts := plan.ObjectScripts(dbm)
	wanted := make(map[string]bool, len(scripts))
	folded := make(map[string]string, len(scripts))
	for _, s := range scripts {
		rel := filepath.ToSlash(objectPath(s))
		wanted[rel] = true
		// On a case-insensitive filesystem - Windows, macOS by default - these
		// two would be one file, and whichever is written last wins.
		if other, ok := folded[strings.ToLower(rel)]; ok {
			opts.warn("%s and %s differ only in case; on a case-insensitive filesystem one overwrites the other", other, rel)
		}
		folded[strings.ToLower(rel)] = rel
	}

	// Files already present whose name differs from the one this run wants
	// only in case: what an object renamed from dbo.orders to dbo.Orders leaves
	// behind. On a case-insensitive filesystem writing the new name would land
	// in the old file and keep its old name, which pruning - comparing names
	// exactly - would then delete, taking the object's only script with it.
	// Removing the old one first lets the write create the file afresh under
	// the name it should have.
	present, err := listScripts(dir)
	if err != nil {
		return res, err
	}
	for _, rel := range present {
		if wanted[rel] {
			continue
		}
		if _, renamed := folded[strings.ToLower(rel)]; !renamed {
			continue // simply stale; pruning deals with it
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return res, fmt.Errorf("remove %s: %w", rel, err)
		}
	}

	for _, s := range scripts {
		rel := objectPath(s)
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return res, err
		}
		// Compare before writing: an unchanged object should leave its file's
		// modification time alone, so build tools and file watchers are not
		// woken by an export that changed nothing.
		if old, err := os.ReadFile(path); err == nil && string(old) == s.SQL {
			continue
		}
		if err := os.WriteFile(path, []byte(s.SQL), 0o644); err != nil {
			return res, err
		}
		res.Written++
	}

	if len(opts.Include) > 0 || len(opts.Exclude) > 0 {
		// A filtered run has no opinion about the objects it was told to
		// ignore, and a table it did not look at is not a table that was
		// dropped. Pruning here deleted the file of every table outside
		// --include - and of every view built on one - on the strength of
		// nothing but a guess.
		opts.log("  --include/--exclude is in effect, so files of objects outside it are left as they are")
		return res, nil
	}
	removed, err := pruneSchemaDir(dir, wanted, opts)
	if err != nil {
		return res, err
	}
	res.Removed = removed
	return res, nil
}

// objectPath is where one object's script belongs, relative to the root.
func objectPath(s plan.ObjectScript) string {
	name := archive.EscapeSegment(s.Name)
	if s.Schema != "" {
		name = archive.EscapeSegment(s.Schema) + "." + name
	}
	return filepath.Join(s.Kind, name+".sql")
}

// pruneSchemaDir removes .sql files under the directories plan owns that this
// run did not write.
func pruneSchemaDir(dir string, wanted map[string]bool, opts Options) (int, error) {
	present, err := listScripts(dir)
	if err != nil {
		return 0, err
	}
	var stale []string
	for _, rel := range present {
		if !wanted[rel] {
			stale = append(stale, rel)
		}
	}

	for _, rel := range stale {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return 0, fmt.Errorf("remove %s: %w", rel, err)
		}
		opts.log("  removed %s", rel)
	}
	return len(stale), nil
}

// listScripts lists the .sql files under the directories plan owns, as
// slash-separated paths relative to dir, sorted.
func listScripts(dir string) ([]string, error) {
	var out []string
	for _, kind := range plan.ObjectDirs() {
		entries, err := os.ReadDir(filepath.Join(dir, kind))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
				continue
			}
			out = append(out, filepath.ToSlash(filepath.Join(kind, e.Name())))
		}
	}
	sort.Strings(out)
	return out, nil
}
