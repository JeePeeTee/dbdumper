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
//
// The encoding has to be injective. Replacing awkward characters with "_", as
// this once did, is not: schema "a/b" and schema "a_b" collapse together, and
// so do schema "dbo" table "a.b" and schema "dbo.a" table "b", because the dot
// that joins the two parts is also legal inside either of them. Colliding names
// are not rejected by the zip writer, so one table's rows would silently
// replace another's.
func DataPath(schema, table string) string {
	return path.Join("data", escapeSegment(schema)+"."+escapeSegment(table)+".jsonl")
}

// escapeSegment percent-encodes everything that would make an entry name
// ambiguous or illegal on a filesystem: the separator this package joins with,
// the characters Windows forbids, and "%" itself so the encoding can be undone.
func escapeSegment(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '%' || r == '.' || r == '/' || r == '\\' || r == ':' ||
			r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|',
			r < 0x20:
			b.WriteByte('%')
			b.WriteByte(hex[(r>>4)&0xF])
			b.WriteByte(hex[r&0xF])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ChunkDataPath returns the entry name for one range of a table read in
// several pieces. The index is zero-padded so the entries sort in key order in
// any listing.
func ChunkDataPath(schema, table string, chunk int) string {
	return path.Join("data", fmt.Sprintf("%s.%s.part%03d.jsonl",
		escapeSegment(schema), escapeSegment(table), chunk))
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

// RawEntry describes data that is already DEFLATE-compressed, so it can be
// placed into the archive without being compressed a second time.
type RawEntry struct {
	Name             string
	UncompressedSize uint64
	CompressedSize   uint64
	CRC32            uint32
}

// AddRaw begins an entry whose bytes are written already compressed. The
// caller must write exactly CompressedSize bytes of a raw DEFLATE stream
// matching the CRC and length given, since the header is written from those
// values rather than derived from the data.
func (w *Writer) AddRaw(e RawEntry) (io.Writer, error) {
	// This header carried a Modified time until it turned out never to reach
	// the file: CreateRaw writes ModifiedTime and ModifiedDate straight out,
	// and only CreateHeader translates Modified into them. The field was
	// therefore doing nothing except suggesting that entries were timestamped.
	// They are not, which is what an archive wants - two exports of an
	// unchanged database should differ only where the data differs. When the
	// archive was made is recorded once, in the manifest.
	return w.zw.CreateRaw(&zip.FileHeader{
		Name:               e.Name,
		Method:             zip.Deflate,
		CRC32:              e.CRC32,
		UncompressedSize64: e.UncompressedSize,
		CompressedSize64:   e.CompressedSize,
	})
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
