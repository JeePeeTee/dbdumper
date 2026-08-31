package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/JeePeeTee/dbdumper/internal/archive"
	"github.com/JeePeeTee/dbdumper/internal/model"
)

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	in := fs.String("in", "", "archive to inspect (required)")
	show := fs.String("show", "", "print an archive entry verbatim, e.g. schema/040_tables.sql")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" && fs.NArg() == 1 {
		*in = fs.Arg(0)
	}
	if *in == "" {
		return errors.New("--in is required")
	}

	ar, err := archive.Open(*in)
	if err != nil {
		return err
	}
	defer ar.Close()

	if *show != "" {
		rc, err := ar.OpenEntry(*show)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(os.Stdout, rc)
		return err
	}

	m := ar.Manifest
	fmt.Printf("archive        %s\n", *in)
	fmt.Printf("format         %d (written by %s)\n", m.FormatVersion, m.Tool)
	fmt.Printf("created        %s\n", m.CreatedAt.Local().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("source server  %s\n", m.Source.Server)
	fmt.Printf("source db      %s\n", m.Source.Database)
	fmt.Printf("server version %s\n", m.Source.ServerVersion)
	fmt.Printf("collation      %s\n", m.Database.Collation)
	if m.Source.SchemaOnly {
		fmt.Printf("contents       schema only (no rows)\n")
	}
	reportPartialData(os.Stdout, &m.Database)

	d := &m.Database
	var mods = map[model.ModuleKind]int{}
	for _, mo := range d.Modules {
		mods[mo.Kind]++
	}
	fmt.Printf("\nobjects\n")
	fmt.Printf("  schemas      %d\n", len(d.Schemas))
	fmt.Printf("  user types   %d\n", len(d.UserTypes))
	fmt.Printf("  sequences    %d\n", len(d.Sequences))
	fmt.Printf("  tables       %d\n", len(d.Tables))
	fmt.Printf("  views        %d\n", mods[model.ModuleView])
	fmt.Printf("  functions    %d\n", mods[model.ModuleFunction])
	fmt.Printf("  procedures   %d\n", mods[model.ModuleProcedure])
	fmt.Printf("  triggers     %d\n", mods[model.ModuleTrigger])

	var idx, fks, checks int
	var rows int64
	for _, t := range d.Tables {
		idx += len(t.Indexes)
		fks += len(t.ForeignKeys)
		checks += len(t.CheckConstraints)
		rows += t.RowCount
	}
	fmt.Printf("  indexes      %d\n", idx)
	fmt.Printf("  foreign keys %d\n", fks)
	fmt.Printf("  checks       %d\n", checks)
	fmt.Printf("  rows         %d\n", rows)

	if len(d.Tables) > 0 {
		tables := append([]model.Table(nil), d.Tables...)
		sort.Slice(tables, func(i, j int) bool { return tables[i].RowCount > tables[j].RowCount })
		partial := anyPartialData(d)
		fmt.Printf("\nlargest tables\n")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		header := "  TABLE\tROWS\tCOLS"
		if partial {
			header += "\tDATA"
		}
		fmt.Fprintln(tw, header)
		for i, t := range tables {
			if i >= 20 {
				fmt.Fprintf(tw, "  ... and %d more\t\t\n", len(tables)-i)
				break
			}
			fmt.Fprintf(tw, "  %s.%s\t%d\t%d", t.Schema, t.Name, t.RowCount, len(t.Columns))
			if partial {
				fmt.Fprintf(tw, "\t%s", partialNote(t))
			}
			fmt.Fprintln(tw)
		}
		tw.Flush()
	}
	return nil
}

// anyPartialData reports whether any table holds less than all of its rows.
func anyPartialData(d *model.Database) bool {
	for _, t := range d.Tables {
		if t.PartialData() {
			return true
		}
	}
	return false
}

// partialNote describes what an archive holds for one table, and is empty for
// the ordinary case of a table dumped whole.
func partialNote(t model.Table) string {
	switch {
	case t.DataSkipped:
		return "excluded"
	case t.RowFilter != "":
		return "filtered"
	default:
		return "complete"
	}
}

// reportPartialData names the tables an archive does not hold in full.
//
// A restore of such an archive is not a copy of the source, and the row counts
// printed above give no sign of it - an excluded table shows zero rows, which
// is indistinguishable from a table that was genuinely empty. It is worth
// several lines to say so plainly.
func reportPartialData(w io.Writer, d *model.Database) {
	var excluded, filtered []model.Table
	for _, t := range d.Tables {
		switch {
		case t.DataSkipped:
			excluded = append(excluded, t)
		case t.RowFilter != "":
			filtered = append(filtered, t)
		}
	}
	if len(excluded)+len(filtered) == 0 {
		return
	}

	fmt.Fprintf(w, "\npartial data   %d table(s) are not held in full\n", len(excluded)+len(filtered))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	shown := 0
	for _, t := range excluded {
		if shown++; shown > 20 {
			break
		}
		fmt.Fprintf(tw, "  %s.%s\trows excluded (--exclude-data)\n", t.Schema, t.Name)
	}
	for _, t := range filtered {
		if shown++; shown > 20 {
			break
		}
		fmt.Fprintf(tw, "  %s.%s\trows filtered: %s\n", t.Schema, t.Name, truncate(t.RowFilter, 60))
	}
	if n := len(excluded) + len(filtered) - 20; n > 0 {
		fmt.Fprintf(tw, "  ... and %d more\t\n", n)
	}
	tw.Flush()
	fmt.Fprintf(w, "  restoring this archive will not reproduce the source database\n")
}
