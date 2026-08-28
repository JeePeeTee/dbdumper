package sqlsrv

import (
	"fmt"
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

// RowFilter finds the WHERE predicate that applies to a table, or "" when none
// does.
type RowFilter func(schema, table string) string

// NewRowFilter parses "<table-glob>:<predicate>" specifications.
//
// The split is on the first colon only: a predicate routinely contains one, in
// a time literal or a schema-qualified name, while a table pattern realistically
// does not.
//
// Patterns are tried in the order given and the first match wins, so a specific
// rule placed before a general one overrides it. A table matched by more than
// one is reported rather than resolved silently.
func NewRowFilter(specs []string, warn func(string, ...any)) (RowFilter, error) {
	type rule struct {
		spec, pattern, predicate string
	}
	rules := make([]rule, 0, len(specs))

	for _, spec := range specs {
		pattern, predicate, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf("--where %q: expected <table>:<predicate>, for example \"LogEvents:CreatedOn > dateadd(day,-90,getutcdate())\"", spec)
		}
		pattern, predicate = strings.TrimSpace(pattern), strings.TrimSpace(predicate)
		if pattern == "" {
			return nil, fmt.Errorf("--where %q: the table pattern is empty", spec)
		}
		if predicate == "" {
			return nil, fmt.Errorf("--where %q: the predicate is empty", spec)
		}
		rules = append(rules, rule{spec, normalizePatterns([]string{pattern})[0], predicate})
	}
	if len(rules) == 0 {
		return nil, nil
	}

	return func(schema, table string) string {
		name := strings.ToLower(schema + "." + table)
		var chosen string
		var shadowed []string
		for _, r := range rules {
			if !matchPattern(r.pattern, name) {
				continue
			}
			if chosen == "" {
				chosen = r.predicate
				continue
			}
			shadowed = append(shadowed, r.spec)
		}
		if len(shadowed) > 0 && warn != nil {
			warn("%s.%s matches more than one --where; using the first and ignoring %s",
				schema, table, strings.Join(shadowed, ", "))
		}
		return chosen
	}, nil
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
