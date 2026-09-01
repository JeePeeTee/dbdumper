# dbdumper vs. DacFx / `.bacpac` — the case for dbdumper

This page lists only what dbdumper does better. It is deliberately one-sided; for
an even-handed account of what dbdumper does *not* capture, and when a `.bacpac`
or a native `BACKUP` is the right tool, see
[When not to use this](../README.md#when-not-to-use-this).

Everything under "Measured" was recorded on one machine against one database.
Everything under "Structural" follows from how the two formats are built, and is
not a benchmark result.

---

## Measured

Local SQL Server 2025, 403 tables, 681,357 rows, 742.0 MB of table data, both
tools run on the same machine against the same database.

| | dbdumper | DacFx (Data-tier Application) |
| --- | --- | --- |
| Export | **9.2 – 11.5 s** | 57 s |
| Restore into a fresh database | **22 s** | 81 s |
| **Full round trip** | **~34 s** | **~138 s** |
| Round trip verified | `verify: OK — 403 tables match` | — |

**About 5× faster to export, 3.7× faster to restore, 4× faster end to end.**

The export gap comes from three things dbdumper does and DacFx does not: it
reads tables concurrently, it splits a large table into key ranges read in
parallel, and it splices already-compressed streams into the archive instead of
recompressing them.

The restore gap comes from bulk copy on a pinned connection with
`IDENTITY_INSERT` preserved, tables loaded concurrently, and indexes and
foreign keys created *after* the rows rather than in front of them — so the
server is not maintaining index pages and checking constraints on every insert.

Export timings were checked against a cold buffer cache as well as a warm one,
so they are not an artefact of the second run being cheaper.

---

## Structural

Advantages that follow from the design rather than from a measurement.

### Export only part of a database

`sqlpackage` is not all-or-nothing — `/p:TableData=` selects which tables' data to include — but
the subsetting stops at whole tables. There is no row filter, and the schema is always complete.

```bash
dbdumper export --include 'sales.*' --exclude-data LogEvents \
                --where 'Orders:CreatedAt >= ''2026-01-01''' ...
```

- `--include` / `--exclude` — glob on `schema.table`
- `--exclude-data` — keep a table's definition, drop its rows
- `--where` — a T-SQL predicate per table

Foreign keys pointing at a partially held table are created `WITH NOCHECK` and
the run says so loudly, so a filtered archive still restores.

### Resume an interrupted export

Rows are spooled to a work directory beside the archive. A run killed at 90% is
continued with `--resume` rather than started again — the difference between
losing ten seconds and losing an hour on a large database. A `.bacpac` export
that fails starts over.

The work directory is fingerprinted against the source schema and the active
`--where` filters, so a resume against a database that has changed underneath it
is refused rather than silently mixing two different reads.

### An archive you can read

A `.bacpac` is `model.xml` plus BCP-format binary blobs. A `.dbdump` is a zip of:

```
manifest.json               the full schema model, authoritative
schema/010_schemas.sql      the same DDL as runnable script
schema/040_tables.sql
schema/070_foreignkeys.sql
data/dbo.Orders.jsonl       one JSON array per row, with a column header
```

You can `grep` it, diff two of them, review the DDL before restoring, or pull one
table's rows out with standard tools. `dbdumper inspect --show
schema/040_tables.sql` prints any entry without unpacking the archive.

The DDL in the archive is the *same text* the restore executes, so the script is
an honest record of what will happen rather than a rendering that might drift.

### No install, no .NET

One statically linked binary, around 7 MB, for Windows, Linux and macOS on both
amd64 and arm64. SqlPackage needs a .NET runtime and its own installation.
Useful in a container, a CI job, or on a locked-down jump box.

### Progress you can plan around

A single self-updating line with a bar, a rate and an ETA for the whole export —
not just the current table — naming whichever table the run is currently waiting
on. Redirected to a file it degrades to plain periodic lines.

### It does not refuse the database

DacFx validates the schema on export and stops on elements it does not support.
dbdumper reads the catalogue and writes what it finds; anything it cannot model
is reported as a warning against that object rather than aborting the export.

### Restore is not all-or-nothing either

`--schema-only`, `--data-only` into an existing schema, and
`--continue-on-error` to press on past a failing object and get a list of what
failed at the end.

### Auditable and modifiable

About 6,500 lines of Go with tests, under your own control. Round-trip fidelity
across every supported column type is covered by an integration suite that dumps
a purpose-built database, restores it, dumps it again and compares the two
archives byte for byte.

---

## Conditions

- Database: 403 tables, 681,357 rows, 742.0 MB of table data
- Server: local SQL Server 2025 (17.0.1000.7), shared-memory connection
- dbdumper: `--parallel 4`, chunking enabled (default)
- DacFx: Export / Import Data-tier Application from SSMS, default settings
- Both restores were into a freshly created, empty database
- All timings are wall clock on the same machine, cache state controlled for
