package sqlsrv

import (
	"strings"
	"testing"
)

func TestNewRowFilter(t *testing.T) {
	warn := func(string, ...any) {}

	f, err := NewRowFilter([]string{
		"dbo.Orders:CreatedOn > dateadd(day,-90,getutcdate())",
		"LogEvents:Level >= 3",
	}, warn)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[[2]string]string{
		{"dbo", "Orders"}:    "CreatedOn > dateadd(day,-90,getutcdate())",
		{"DBO", "ORDERS"}:    "CreatedOn > dateadd(day,-90,getutcdate())", // case-insensitive
		{"dbo", "LogEvents"}: "Level >= 3",                                // bare name means *.name
		{"log", "LogEvents"}: "Level >= 3",
		{"dbo", "Customer"}:  "", // unmatched
	}
	for in, want := range cases {
		if got := f(in[0], in[1]); got != want {
			t.Errorf("%s.%s = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// TestRowFilterSplitsOnFirstColonOnly - a predicate routinely contains a colon,
// in a time literal or a schema-qualified name; a table pattern does not.
func TestRowFilterSplitsOnFirstColonOnly(t *testing.T) {
	f, err := NewRowFilter([]string{"Shifts:StartsAt >= '08:30:00' AND EndsAt <= '17:00:00'"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := f("dbo", "Shifts"), "StartsAt >= '08:30:00' AND EndsAt <= '17:00:00'"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRowFilterFirstMatchWins - a specific rule placed before a general one
// overrides it, and the shadowed rules are reported rather than dropped quietly.
func TestRowFilterFirstMatchWins(t *testing.T) {
	var warnings []string
	f, err := NewRowFilter([]string{"dbo.Orders:a=1", "*:b=2"},
		func(format string, args ...any) { warnings = append(warnings, format) })
	if err != nil {
		t.Fatal(err)
	}
	if got := f("dbo", "Orders"); got != "a=1" {
		t.Errorf("the specific rule should win, got %q", got)
	}
	if got := f("dbo", "Other"); got != "b=2" {
		t.Errorf("the general rule should still apply elsewhere, got %q", got)
	}
	if len(warnings) == 0 {
		t.Error("a table matched by two rules should be reported")
	}
}

func TestNewRowFilterRejectsMalformedSpecs(t *testing.T) {
	for _, spec := range []string{"NoColonHere", ":no table", "tbl:", "tbl:   ", "   :x"} {
		if _, err := NewRowFilter([]string{spec}, nil); err == nil {
			t.Errorf("%q should be rejected", spec)
		} else if !strings.Contains(err.Error(), "--where") {
			t.Errorf("%q: the error should name the flag: %v", spec, err)
		}
	}
	if f, err := NewRowFilter(nil, nil); err != nil || f != nil {
		t.Errorf("no specifications should give a nil filter, got %v %v", f, err)
	}
}
