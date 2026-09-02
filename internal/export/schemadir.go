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
// .gitattributes, a subdirectory someone added - is left alone.
func writeSchemaDir(dir string, dbm *model.Database, opts Options) (SchemaDirResult, error) {
	var res SchemaDirResult

	scripts := plan.ObjectScripts(dbm)
	wanted := make(map[string]bool, len(scripts))

	for _, s := range scripts {
		rel := objectPath(s)
		wanted[filepath.ToSlash(rel)] = true

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
	var stale []string
	for _, kind := range plan.ObjectDirs() {
		sub := filepath.Join(dir, kind)
		entries, err := os.ReadDir(sub)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, e := range entries {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".sql") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(kind, e.Name()))
			if !wanted[rel] {
				stale = append(stale, rel)
			}
		}
	}
	sort.Strings(stale)

	for _, rel := range stale {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			return 0, fmt.Errorf("remove %s: %w", rel, err)
		}
		opts.log("  removed %s", rel)
	}
	return len(stale), nil
}
