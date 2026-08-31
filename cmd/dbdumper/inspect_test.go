package main

import (
	"strings"
	"testing"

	"github.com/JeePeeTee/dbdumper/internal/model"
)

// TestInspectNamesTheTablesItDoesNotHoldInFull - an excluded table reports zero
// rows, which on its own is indistinguishable from a table that was empty at
// the source. Someone handed an archive has no other way to find out that a
// restore of it will not reproduce the database.
func TestInspectNamesTheTablesItDoesNotHoldInFull(t *testing.T) {
	d := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "Orders", RowCount: 42},
		{Schema: "dbo", Name: "LogEvents", DataSkipped: true},
		{Schema: "audit", Name: "Trail", RowCount: 7, RowFilter: "CreatedAt >= '2026-01-01'"},
	}}

	var b strings.Builder
	reportPartialData(&b, d)
	got := b.String()

	for _, want := range []string{
		"2 table(s) are not held in full",
		"dbo.LogEvents",
		"rows excluded (--exclude-data)",
		"audit.Trail",
		"CreatedAt >= '2026-01-01'",
		"will not reproduce the source database",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	// A table dumped whole has nothing to explain.
	if strings.Contains(got, "dbo.Orders") {
		t.Errorf("a complete table should not be listed as partial:\n%s", got)
	}
}

func TestInspectSaysNothingWhenEveryTableIsComplete(t *testing.T) {
	d := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "Orders", RowCount: 42},
		{Schema: "dbo", Name: "Empty"},
	}}
	var b strings.Builder
	reportPartialData(&b, d)
	if got := b.String(); got != "" {
		t.Errorf("expected no output for a complete archive, got:\n%s", got)
	}
	if anyPartialData(d) {
		t.Error("anyPartialData should be false when nothing was held back")
	}
}

func TestPartialNote(t *testing.T) {
	cases := []struct {
		t    model.Table
		want string
	}{
		{model.Table{}, "complete"},
		{model.Table{DataSkipped: true}, "excluded"},
		{model.Table{RowFilter: "Id > 5"}, "filtered"},
		// --exclude-data wins: no rows at all is the stronger statement.
		{model.Table{DataSkipped: true, RowFilter: "Id > 5"}, "excluded"},
	}
	for _, c := range cases {
		if got := partialNote(c.t); got != c.want {
			t.Errorf("partialNote(%+v) = %q, want %q", c.t, got, c.want)
		}
	}
}

// A long predicate is trimmed rather than allowed to wrap the table.
func TestInspectTrimsALongPredicate(t *testing.T) {
	long := "CreatedAt >= '2026-01-01' AND Status IN ('open','pending','review','escalated','archived')"
	d := &model.Database{Tables: []model.Table{
		{Schema: "dbo", Name: "Tickets", RowFilter: long},
	}}
	var b strings.Builder
	reportPartialData(&b, d)
	got := b.String()
	if strings.Contains(got, long) {
		t.Errorf("the full predicate should have been trimmed:\n%s", got)
	}
	if !strings.Contains(got, "CreatedAt >= '2026-01-01'") {
		t.Errorf("the start of the predicate should survive:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 100 {
			t.Errorf("line is %d chars, too wide for a terminal: %q", len(line), line)
		}
	}
}
