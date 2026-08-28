package sqlsrv

import (
	"path"
	"strings"
)

// NewTableFilter builds a filter from include/exclude glob patterns.
//
// Patterns match case-insensitively against "schema.table". A pattern without
// a dot is treated as "*.<pattern>", so "Orders" matches dbo.Orders. Excludes
// win over includes; an empty include list means "everything".
func NewTableFilter(include, exclude []string) TableFilter {
	inc := normalizePatterns(include)
	exc := normalizePatterns(exclude)
	if len(inc) == 0 && len(exc) == 0 {
		return nil
	}
	return func(schema, table string) bool {
		name := strings.ToLower(schema + "." + table)
		for _, p := range exc {
			if matchPattern(p, name) {
				return false
			}
		}
		if len(inc) == 0 {
			return true
		}
		for _, p := range inc {
			if matchPattern(p, name) {
				return true
			}
		}
		return false
	}
}

func normalizePatterns(pats []string) []string {
	out := make([]string, 0, len(pats))
	for _, p := range pats {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !strings.Contains(p, ".") {
			p = "*." + p
		}
		out = append(out, p)
	}
	return out
}

func matchPattern(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}
