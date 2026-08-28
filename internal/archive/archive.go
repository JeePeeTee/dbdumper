// Package archive reads and writes the .dbdump container: a zip holding
// manifest.json, human-readable .sql scripts, and one JSONL file per table.
package archive

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// ManifestName is the archive entry holding the serialized model.Manifest.
const ManifestName = "manifest.json"

// DataPath returns the archive entry name for a table's rows.
func DataPath(schema, table string) string {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch r {
			case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
				return '_'
			}
			return r
		}, s)
	}
	return path.Join("data", safe(schema)+"."+safe(table)+".jsonl")
}

// Writer builds an archive. Entries are written sequentially.
type Writer struct {
	f  *os.File
	zw *zip.Writer
}

// Create opens path for writing, truncating any existing archive and creating
// missing parent directories.
func Create(p string) (*Writer, error) {
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.Create(p)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, zw: zip.NewWriter(f)}, nil
}

// Add begins a new deflate-compressed entry. The previous entry, if any, is
// finished automatically.
func (w *Writer) Add(name string) (io.Writer, error) {
	return w.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
}

// AddJSON writes v as indented JSON.
func (w *Writer) AddJSON(name string, v any) error {
	wr, err := w.Add(name)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(wr)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// AddText writes a UTF-8 text entry.
func (w *Writer) AddText(name, content string) error {
	wr, err := w.Add(name)
	if err != nil {
		return err
	}
	_, err = io.WriteString(wr, content)
	return err
}

// Close finalizes the zip central directory and closes the file.
func (w *Writer) Close() error {
	if err := w.zw.Close(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// Reader gives access to an existing archive and its manifest.
type Reader struct {
	zr       *zip.ReadCloser
	Manifest *model.Manifest
	byName   map[string]*zip.File
}

// Open reads an archive and parses its manifest.
func Open(p string) (*Reader, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, fmt.Errorf("open archive %s: %w", p, err)
	}
	r := &Reader{zr: zr, byName: make(map[string]*zip.File, len(zr.File))}
	for _, f := range zr.File {
		r.byName[f.Name] = f
	}

	mf, ok := r.byName[ManifestName]
	if !ok {
		zr.Close()
		return nil, fmt.Errorf("%s: not a dbdumper archive (no %s)", p, ManifestName)
	}
	rc, err := mf.Open()
	if err != nil {
		zr.Close()
		return nil, err
	}
	defer rc.Close()

	var m model.Manifest
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		zr.Close()
		return nil, fmt.Errorf("parse %s: %w", ManifestName, err)
	}
	if m.FormatVersion > model.FormatVersion {
		zr.Close()
		return nil, fmt.Errorf("archive format version %d is newer than this tool supports (%d)",
			m.FormatVersion, model.FormatVersion)
	}
	r.Manifest = &m
	return r, nil
}

// OpenEntry opens an archive entry by name.
func (r *Reader) OpenEntry(name string) (io.ReadCloser, error) {
	f, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("archive entry %q not found", name)
	}
	return f.Open()
}

// HasEntry reports whether the named entry exists.
func (r *Reader) HasEntry(name string) bool {
	_, ok := r.byName[name]
	return ok
}

// Close releases the archive.
func (r *Reader) Close() error { return r.zr.Close() }
