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
		fmt.Printf("\nlargest tables\n")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TABLE\tROWS\tCOLS")
		for i, t := range tables {
			if i >= 20 {
				fmt.Fprintf(tw, "  ... and %d more\t\t\n", len(tables)-i)
				break
			}
			fmt.Fprintf(tw, "  %s.%s\t%d\t%d\n", t.Schema, t.Name, t.RowCount, len(t.Columns))
		}
		tw.Flush()
	}
	return nil
}
