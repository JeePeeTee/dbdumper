# dbdumper

A Go command-line tool that dumps a SQL Server database — **definitions and data** — into a
single portable archive, and restores that archive into a new database. Same idea as a
`.bacpac`, but the format is plain zip + JSON + SQL, so you can read it, diff it and script it
without DacFx or SSMS.

![Terminal recording of dbdumper exporting a 33-table database: a progress bar, row counts and an ETA update in place, finishing in five seconds](docs/demo.gif)

*A real run, not a mock-up: 33 tables and 1,008,720 rows in 5.1 seconds, recorded from the tool's
own output. The large table is split into ranges read in parallel — see
[reading tables in parallel](#reading-tables-in-parallel).*

It is not the right tool for every job — see [when not to use this](#when-not-to-use-this) before
reaching for it on a large cloud database.

## Install

Download a binary for your platform from
[the latest release](https://github.com/JeePeeTee/dbdumper/releases/latest) — one static file,
around 7 MB, nothing to install alongside it. `SHA256SUMS` is published with each release.

With a Go toolchain to hand:

```bash
go install github.com/JeePeeTee/dbdumper/cmd/dbdumper@latest
```

Or from a clone:

```bash
go build -o bin/dbdumper ./cmd/dbdumper
```

## Quick start

Dump a database:

```bash
dbdumper export --server "DEVBOX\SQLEXPRESS" --protocol lpc --database AppDb --out AppDb.dbdump
```

Restore it into a brand new database:

```bash
dbdumper import --server "DEVBOX\SQLEXPRESS" --protocol lpc --database AppDb_restore --create-database --in AppDb.dbdump
```

Check what happened:

```bash
dbdumper verify --server "DEVBOX\SQLEXPRESS" --protocol lpc --database AppDb_restore --in AppDb.dbdump
```

## Commands

| Command | What it does |
| --- | --- |
| `export` | Reads a live database and writes a `.dbdump` archive. |
| `import` | Recreates the objects and loads the rows into a target database. |
| `verify` | Compares a database against an archive: row counts per table, plus existence of indexes, foreign keys, checks and modules. |
| `inspect` | Prints an archive's manifest summary; `--show <entry>` dumps any file inside it. |

### Connection flags (shared)

```
--server        host, host\instance or host,port          (default localhost)
--port          TCP port
--database      database name                             (required)
--user          SQL login; omit for Windows authentication
--password      password for --user (or set DBDUMPER_PASSWORD)
--trusted       force Windows authentication              (implied when --user is empty)
--encrypt       disable | false | true | strict           (default disable)
--trust-cert    accept a self-signed server certificate   (default true)
--protocol      tcp | np (named pipes) | lpc (shared memory)
--packet-size   TDS packet size, 512..32767                (default 32767)
--dsn           full connection string, overrides all of the above except --database
```

`--protocol lpc` is what you need for a local instance that has TCP/IP disabled in SQL Server
Configuration Manager — which is the default for a developer install.

### Using a connection string

`--dsn` takes a connection string in either form the driver understands, and replaces the other
connection flags:

```bash
dbdumper export --dsn "sqlserver://DEVBOX/SQLEXPRESS?database=AppDb&trusted_connection=true&encrypt=disable&protocol=lpc" --out AppDb.dbdump
```

```bash
dbdumper export --dsn "Server=DEVBOX\SQLEXPRESS;Database=AppDb;Integrated Security=True;TrustServerCertificate=True;protocol=lpc" --out AppDb.dbdump
```

A named instance goes in the URL *path* (`sqlserver://host/instance`), not the authority. In the
ADO form, `Server`/`Data Source`, `Database`/`Initial Catalog` and `Integrated Security` all work,
so a .NET connection string can usually be pasted as-is.

`--database` still applies on top of a `--dsn`, and wins over any database named in it. That is
what you want when reusing one connection string across databases, and on `import` when the
target database has a different name from the source:

```bash
# one connection string, several dumps
DSN="Server=DEVBOX\SQLEXPRESS;Integrated Security=True;protocol=lpc"
dbdumper export --dsn "$DSN" --database AppDb --out AppDb.dbdump
dbdumper export --dsn "$DSN" --database AppDb_archive --out AppDb_archive.dbdump

# restore into a new name
dbdumper import --dsn "$DSN" --database AppDb_restore --create-database --in AppDb.dbdump
```

Passwords in a `--dsn` are masked in log output, in both forms.

### Azure SQL Database

Pass the discrete flags rather than building a URL by hand — an Azure login is usually of the
form `user@servername`, and that `@` has to be percent-encoded inside a `sqlserver://` URL.
The flags do the escaping for you:

```bash
export DBDUMPER_PASSWORD='...'
dbdumper export --server myserver.database.windows.net --database appdb --user "appuser@myserver" --out appdb.dbdump
```

`--password` is read from the `DBDUMPER_PASSWORD` environment variable when the flag is not
given, so the password stays out of your shell history and out of the process list.

Azure endpoints are recognised by hostname (`*.database.windows.net` and the sovereign-cloud and
Synapse equivalents) and change three defaults, because the local-server ones are wrong there:

| | local default | Azure default |
| --- | --- | --- |
| `--encrypt` | `disable` | `true` (Azure mandates encryption) |
| `--trust-cert` | `true` | `false` (Azure's certificate chains to a public root) |
| `--protocol` | negotiated | not applicable — TCP 1433 only |

Setting either flag explicitly still wins. Two further Azure differences are handled in
`import`: `DROP DATABASE` skips the `SET SINGLE_USER` step, which Azure SQL Database does not
support, and `--create-database` waits for the new database to report `ONLINE`, because
provisioning there is asynchronous.

Over an internet link, latency dominates. The driver's own default TDS packet size is 4096
bytes; dbdumper asks for 32767 instead, the protocol maximum, which an encrypted connection
negotiates down to 16383 — still four times fewer round trips. `--packet-size -1` restores the
driver default. On a local shared-memory connection the setting makes no measurable difference
either way.

Exporting a large database across the internet is slow simply because every row crosses it.
Before starting, it is worth knowing what you are about to pull. `DataMB` is what crosses the
wire; `TotalMB` includes non-clustered indexes, which do not:

```sql
SELECT TOP 20 s.name + '.' + t.name AS TableName,
       SUM(CASE WHEN ps.index_id IN (0,1) THEN ps.row_count ELSE 0 END) AS [Rows],
       CONVERT(decimal(12,1), SUM(CASE WHEN ps.index_id IN (0,1) THEN ps.used_page_count ELSE 0 END)*8.0/1024) AS [DataMB],
       CONVERT(decimal(12,1), SUM(ps.reserved_page_count)*8.0/1024) AS [TotalMB]
FROM sys.dm_db_partition_stats ps
JOIN sys.tables t  ON t.object_id = ps.object_id
JOIN sys.schemas s ON s.schema_id = t.schema_id
GROUP BY s.name, t.name
ORDER BY SUM(CASE WHEN ps.index_id IN (0,1) THEN ps.used_page_count ELSE 0 END) DESC;
```

Log and audit tables are usually the largest and the least worth copying. Use `--exclude-data` to
keep their definitions without paying for their rows.

#### When a connection is refused

Azure SQL authenticates against the *target database*, not the server, and reports a wrong
password, a database that does not exist, and a login with no user in that database all as
`Login failed for user` (18456). That is deliberate — telling them apart would reveal which
databases exist — but it leaves you with no idea which of the three to fix, so dbdumper prints
the three candidates and the SQL for the one people hit most:

```
error: connect to sqlserver://appuser%40myserver:***@myserver.database.windows.net?database=appdb: mssql: Login failed for user 'appuser'. (18456)

Azure SQL authenticates against the target database, and reports all three of these
as the same error. Any one of them could be the cause:
  1. The password is wrong.
  2. The database "appdb" does not exist on this server.
  3. The login "appuser@myserver" has no user in "appdb".
     ...
         CREATE USER [appuser] FOR LOGIN [appuser];
```

The quickest way to split them is to run the same credentials against a database the login can
definitely reach with `--schema-only`: if that works, the password is not the problem.

Firewall rejections (40615), a paused serverless database (40613) and an unroutable login (40532)
are distinguished by Azure, and get their own one-line explanations.

If your credentials live in a .NET `app.config`, map the keys straight across:

| appSettings key | flag |
| --- | --- |
| `Server` | `--server` |
| `Database` | `--database` |
| `User` | `--user` |
| `Password` | `DBDUMPER_PASSWORD`, decrypted first if `Encrypted` is `True` |

An `Encrypted=True` password is ciphertext produced by your own application; dbdumper has no way
to read it and does not try. Get the plaintext from whatever code in your app decrypts it.

### export

```
--out <file>           archive to write                        (required)
--force                overwrite --out if it exists
--resume               continue an export interrupted earlier
--restart              discard an interrupted export and start over
--parallel <n>         pieces read concurrently                (default 4)
--chunk-min-bytes <n>  split tables above this size into ranges (default 128MB;
                       -1 disables)
--schema-only          definitions only, no rows
--include <glob>       only these tables, glob on schema.table, repeatable
--exclude <glob>       omit these tables entirely, definition included, repeatable
--exclude-data <glob>  keep these tables' definitions but skip their rows, repeatable
--where <glob>:<sql>   dump only the rows matching a predicate, repeatable
```

Globs match case-insensitively against `schema.table`. A pattern with no dot is treated as
`*.<pattern>`, so `--exclude Audit*` skips `dbo.AuditTrail` and `log.AuditTrail` alike.

`--exclude` and `--exclude-data` differ in an important way:

- `--exclude` removes the table from the dump completely. It is not in the manifest, so a restore
  does not create it at all. Foreign keys from other tables into it are dropped, with a warning.
- `--exclude-data` keeps the table, its columns, indexes and constraints, and restores it empty.

`--exclude-data` is what you want for a large log or audit table you still need the shape of. Be
aware of the consequence: rows elsewhere that reference the emptied table now dangle, so foreign
keys pointing at it are created `WITH NOCHECK` and left untrusted. Export lists exactly which
keys those are:

```
  ! 8 foreign key(s) point at a table whose data was skipped; they will be created
  ! WITH NOCHECK and left untrusted, so the restored database will have dangling references:
  !   FK_Invoice_Document on dbo.Invoice -> dbo.Document
```

Exclude the referencing tables' data too if you need referential integrity.

### Reading tables in parallel

Tables are read concurrently, each on its own connection, and spooled to its own
file. `--parallel` sets how many at once.

How much this buys depends on where the time goes, and the ceiling is the
**largest single table** — parallelism across tables cannot split one. On the
403-table database this was developed against, one table is 72% of the work:

| `--parallel` | time |
| ---: | ---: |
| 1 | 36.1s |
| 2 | 24.4s |
| 4 | 24.6s |
| 6 | 25.2s |

Two workers collect the whole gain and more add nothing, because everything else
finishes long before the big table does.

That ceiling is what **splitting a table into ranges** removes. A table above
`--chunk-min-bytes` whose clustered index has a single, orderable key column is
divided into ranges that are read at the same time:

```
  dbo.FileData                                split into 12 ranges on [Oid]
```

| | time |
| --- | ---: |
| `--parallel 1` | 36.1s |
| `--parallel 6` | 24.8s |
| `--parallel 6`, splitting on | **9.2s** |

Boundaries come from the server — `NTILE` over a `TABLESAMPLE` of the key — so
they respect its ordering rather than the client's. That matters for
`uniqueidentifier`, which SQL Server compares by its last six bytes first, so
neither byte order nor the order the text form suggests would divide it evenly.

**Correctness does not depend on the boundaries being well chosen.** The ranges
are consecutive and half-open over the same ordering the server compares with,
so they partition the table whatever the values are; a poor boundary makes one
range larger than another, it cannot lose or duplicate a row. Keys that are NULL
get a range of their own, since no comparison would match them.

A split table's rows arrive as several archive entries, listed in the manifest
as `dataFiles`, and the importer loads them in order. The division is recorded
in the work directory, so a `--resume` reads the remaining ranges with the
boundaries the first run used rather than recomputing them from data that has
moved on.

A table is read in one piece when it is below the threshold, is a heap, has a
composite or unorderable clustered key, or when the sample cannot produce
distinct boundaries.

Over a network the picture differs again: each connection is latency-bound
rather than CPU-bound, so more workers keep paying off until the link saturates.

Output does not depend on the worker count. Tables are written to the archive in
catalogue order however they were read, so an archive produced with `--parallel 8`
is byte-identical to one produced with `--parallel 1`.

Work is scheduled largest-first, since the run cannot end before its biggest
table does and starting that one last would leave the other workers idle.

### Dumping the schema without the data

`--schema-only` writes the manifest and the DDL and nothing else. It is the whole-database
counterpart to `--exclude-data`, which works a table at a time:

```bash
dbdumper export --server "DEVBOX\SQLEXPRESS" --database AppDb --out AppDb.dbdump --schema-only
```

The data path is skipped rather than filtered: no table is read, and the archive contains no
`data/` entries at all. The cost of the run is the schema pass alone, which scales with the number
of objects rather than with how much data the database holds — a 4 TB database and a 4 MB one with
the same schema take about the same time.

That makes it usable as a nightly job that keeps a database's shape under version control:

```bash
for db in AppDb Billing Reporting; do
  dbdumper export --server "$SQLHOST" --database "$db" --out "schema/$db.dbdump" --schema-only --force
done
git add schema && git commit -m "nightly schema snapshot"
```

Two things to know before relying on that:

- Output is **not yet byte-identical between runs**, so a nightly commit can show changes even when
  nothing in the database moved. See [#21](https://github.com/JeePeeTee/dbdumper/issues/21).
- A schema-only archive restores as **empty tables**, which is the point — but `verify` then
  compares zero rows against zero rows and reports `OK`. That is correct and still worth expecting.

`import` has its own `--schema-only`, which creates the objects from an archive and loads no rows
even when the archive has data in it. The two flags are independent: either end can be
schema-only.

### Taking part of a table

`--exclude-data` is all-or-nothing. `--where` keeps a slice instead:

```bash
dbdumper export --server ... --database AppDb --out AppDb.dbdump   --where "LogEvents:CreatedOn > dateadd(day,-90,getutcdate())"   --where "AuditTrail:Level >= 3"
```

The part before the first colon is a table glob, matched exactly as `--include` and `--exclude`
are — case-insensitive against `schema.table`, and a bare name means `*.name`. Everything after it
is **raw T-SQL**, appended to the table's `SELECT` as a `WHERE` clause. Only the first colon
splits, so a predicate may contain them:

```bash
--where "Shifts:StartsAt >= '08:30:00' AND EndsAt <= '17:00:00'"
```

There is nothing to escape and nothing is validated: the predicate is an expression, not a value,
and a bad one comes back as a SQL Server error naming the table. Patterns are tried in order and
the first match wins, so put a specific rule before a general one; a table matched by more than one
is reported rather than resolved silently.

Three consequences worth knowing:

- **Foreign keys pointing at a filtered table are created `WITH NOCHECK`** and left untrusted,
  exactly as for `--exclude-data`, because rows elsewhere may reference rows the filter excluded.
  Export lists which keys those are.
- **No percentage or ETA for a filtered table.** The row estimate behind the progress bar counts
  the whole table, so it would be wrong. The line falls back to a running count and elapsed time.
- **Changing a `--where` invalidates a resume.** The predicates are part of the fingerprint, so
  continuing an interrupted export under a different filter is refused rather than mixing rows
  selected two different ways.

The filter is recorded in the manifest as `rowFilter`, so an archive says on its face that it holds
a subset.

### Resuming an interrupted export

Pulling several gigabytes across the internet takes long enough that something
will eventually interrupt it. An export writes its tables to a work directory
next to the output — `<out>.dbdump.part` — and assembles the archive only once
every table is present, so a run that dies can be continued instead of repeated:

```bash
dbdumper export --server myserver.database.windows.net --database appdb --user "appuser@myserver" --out appdb.dbdump
# ...interrupted after 300 of 400 tables...

dbdumper export --server myserver.database.windows.net --database appdb --user "appuser@myserver" --out appdb.dbdump --resume
resuming: 300 table(s) already dumped in appdb.dbdump.part
```

Finding a work directory without being told what to do about it is an error, not
a guess: pass `--resume` to continue it or `--restart` to throw it away.

A table counts as done only once its data has been flushed and its state file
written, so a run killed mid-table simply redoes that table. Resuming is refused
if the source database's name or schema no longer matches what the interrupted
run saw, since mixing rows read before a schema change with rows read after it
would produce an archive that never existed.

Two things to know:

- **A resumed dump is not a point-in-time snapshot.** Tables carried over hold
  the rows they had when the first run read them. Neither is an ordinary export,
  which reads each table in its own statement — but a resume widens the window
  from minutes to however long passed between the runs.
- **Disk.** The work directory holds the table data already compressed, so it is
  about the size of the finished archive rather than the size of the rows. During
  packaging each spooled table is deleted as soon as it is safely inside the
  archive, so the peak is roughly one archive plus the largest single table, not
  two archives.

### Progress display

On a terminal, `export` draws a single status line that is rewritten in place and then replaced
by the table's final line when it completes:

```
  dbo.Address                                     36580 rows      5.1 MB
  dbo.AuditEntry                                   6435 rows    877.2 KB
| [142/403 63%] dbo.Document [############----] 77% 1536/1985 469.0 MB ETA 11s
```

Two figures, answering two questions. `77%` with the bar is the table being read; `63%` in the
counter and the ETA are the export as a whole.

The estimates come from the engine's own accounting, read once before the dump starts
(`sys.dm_db_partition_stats`), so they cost nothing and can be slightly stale — they drive the
display and nothing else. If the server will not give them up, the line falls back to a plain row
count and elapsed time rather than inventing a number.

**The whole-export figure is measured in source bytes, not rows.** Rows per second varies tenfold
between a table of integers and a table of blobs, so a row-based estimate would swing wildly on a
mixed database; bytes per second stays roughly comparable, and the rate calibrates itself as the
run proceeds. Only what this run will actually read is counted: tables skipped with
`--exclude-data` and tables carried over by `--resume` cost nothing now and are left out of both
totals. A table restricted by `--where` keeps its overall share but shows no per-table percentage,
since the row estimate covers the whole table.

On the 403-table database this was developed against, the ETA read 23s at 26%, 17s at 45% and 11s
at 63%, against an actual finish of 35.1s.

The bar uses block-drawing characters where the console can render them. On Windows that depends
on the console code page: a classic PowerShell window runs under a legacy page such as 437 or
850, where UTF-8 bytes come out as mojibake. dbdumper switches the console to code page 65001
for the duration of the run and puts the previous one back on exit; if that switch fails it
falls back to an ASCII bar. Either way the bar occupies the same number of columns.

When output is not a terminal - piped into `grep`, redirected to a log - there is no in-place
rewriting. Each update is written as an ordinary line instead, so a captured log still shows
progress.

### import

```
--in <file>            archive to restore                      (required)
--create-database      create --database if it does not exist
--drop-existing        DROP and recreate --database first (destroys data)
--collation <name>     collation for a newly created database  (default: the source's)
--schema-only          create objects, load no rows
--data-only            load rows into an existing, matching schema
--include / --exclude  same globbing as export
--batch-rows <n>       rows per INSERT statement               (default 500)
--commit-rows <n>      rows per transaction                    (default 20000)
--parallel <n>         tables loaded concurrently              (default 4)
--no-bulk              use INSERT statements even where bulk copy would work
--continue-on-error    log and skip failures instead of aborting
```

Foreign keys are created after the data, so table load order is irrelevant and tables load in
parallel, largest first.

### How data is loaded

Two paths, chosen per table:

- **Bulk copy** (TDS `INSERT BULK`) whenever every column of the table is a type the protocol's
  encoder handles. Rows go over the wire in the server's own row format — no statement text, no
  parameter declarations, no plan compilation. `KEEP_NULLS` is on, so NULLs stay NULL instead of
  being replaced by column defaults.
- **Batched `INSERT`** for everything else: tables containing `xml`, `sql_variant`, `geography`,
  `geometry` or `hierarchyid`, which the bulk encoder has no case for.

Both paths run with `SET IDENTITY_INSERT` on for tables with an identity column, so keys are
preserved either way. `--no-bulk` forces the second path everywhere, which is a useful thing to
try if a load ever misbehaves.

Restoring the 403-table, 681k-row, 498 MB database this was developed against takes about 25
seconds with bulk copy and about 11 minutes with `--no-bulk`.

## What gets captured

| | |
| --- | --- |
| Schemas | `CREATE SCHEMA`, owner recorded |
| Tables | columns, types, collations, nullability, identity seed/increment, sparse, `ROWGUIDCOL`, computed columns (incl. `PERSISTED`) |
| Constraints | primary keys, unique constraints, defaults (named), check constraints, foreign keys with `ON DELETE`/`ON UPDATE` and trusted/disabled state |
| Indexes | clustered, non-clustered, unique, filtered, `INCLUDE` columns, `FILLFACTOR`, `PAD_INDEX`, `IGNORE_DUP_KEY`, clustered/non-clustered columnstore |
| Programmability | views, scalar/inline/table-valued functions, stored procedures, triggers — stored verbatim with their `ANSI_NULLS`/`QUOTED_IDENTIFIER` settings |
| Sequences | full definition plus the current value |
| User-defined types | scalar alias types and table types |
| Data | every row of every non-excluded table |

Not captured: database users, roles and permissions; database master keys and database-scoped
credentials; XML and spatial indexes; partition schemes and filegroups (everything lands on
`PRIMARY`); Always Encrypted keys; temporal-table system versioning; CLR assemblies; full-text
catalogs; extended properties; constraints declared *inside* a table type (its columns are
reproduced, its primary key is not). Objects it cannot reproduce are reported as warnings at
export time rather than silently dropped.

Do not read that list as "a `.bacpac` keeps none of these". It keeps more than this tool does: a
bacpac carries database users and database roles. What it does not carry is the **server logins**
behind them, so those users arrive orphaned in the target and someone has to remap them by hand.

On the 403-table database used for the comparisons below, that bacpac's `model.xml` happened to
contain no user, role or permission elements at all — but that is a fact about one database, not
about the format, and an earlier version of this file wrongly generalised it into one. What it did
carry that this does not is a database master key and two database-scoped credentials.

If you need permissions reproduced faithfully, neither format is the answer: script them with
[dbatools](https://dbatools.io) — `Export-DbaLogin` and `Export-DbaUser` — or SSMS's Generate
Scripts, both of which read `sys.database_principals` and `sys.database_permissions` directly.

## Archive format

A `.dbdump` is a zip file:

```
manifest.json                     the full schema model — the authoritative source
schema/010_schemas.sql            the same DDL rendered for sqlcmd/SSMS (informational)
schema/020_types.sql
schema/030_sequences.sql
schema/040_tables.sql
schema/050_indexes.sql
schema/060_checks.sql
schema/070_foreignkeys.sql
schema/080_modules.sql
schema/090_finalize.sql
data/dbo.Customer.jsonl           one file per table
README.txt
```

`import` regenerates its DDL from `manifest.json`, using the exact same code that produced the
`.sql` files — so the scripts are an honest record of what a restore runs, but editing them has
no effect. Edit `manifest.json` if you want to change what gets created.

Each data file is JSON Lines. The first line is a header, every line after it is one row as a
JSON array in header column order:

```json
{"table":"dbo.Customer","columns":["Id","Name","Balance","CreatedUtc","Photo"]}
[1,"Acme","1250.0000","2026-08-27T09:14:22.1234567",null]
[2,"Böhm & Co","-3.5000","2026-08-27T09:14:23.0000000","iVBORw0KGgo="]
```

### Value encoding

| SQL type | JSON |
| --- | --- |
| `bit` | `true` / `false` |
| `tinyint`, `smallint`, `int`, `bigint` | number |
| `float`, `real` | number |
| `decimal`, `numeric`, `money`, `smallmoney` | decimal **string**, e.g. `"12.3400"` — never a float, so nothing is rounded |
| `uniqueidentifier` | `"3F2504E0-4F89-11D3-9A0C-0305E82C3301"` |
| `binary`, `varbinary`, `image`, `geography`, `geometry`, `hierarchyid` | base64 string |
| `date` | `"2006-01-02"` |
| `time` | `"15:04:05.1234567"` |
| `datetime`, `smalldatetime` | `"2006-01-02T15:04:05.999"` |
| `datetime2` | `"2006-01-02T15:04:05.1234567"` |
| `datetimeoffset` | `"2006-01-02T15:04:05.1234567+02:00"` |
| everything else | string |
| `NULL` | `null` |

The `T` separator is deliberate: it makes the string language-neutral, so a restore onto a
server with a different default language still parses the value the same way.

Computed columns and `rowversion`/`timestamp` columns are not stored — the server regenerates
them.

## How a restore is ordered

1. schemas → user-defined types → sequences → tables (primary keys, unique constraints and
   defaults inlined, so tables are loadable straight away)
2. data, with `SET IDENTITY_INSERT` on for tables that have an identity column so keys survive
3. indexes → check constraints → foreign keys (after the data, which is both faster and avoids
   ordering problems)
4. views, functions, procedures, triggers
5. sequence positions and identity reseeds

Steps that can fail purely because of ordering — views on views, computed columns calling a
scalar function — are retried until a full pass makes no further progress. Only then is the
remaining error reported. This means module dependency order does not have to be perfect.

In `--data-only` mode the schema already exists, so the loader disables triggers and constraint
checking for the duration of the load and re-enables them afterwards.

## When not to use this

dbdumper reads every row over the wire, one table at a time, and writes a format designed to be
read by humans. That buys selectivity and transparency, and costs throughput and fidelity. Several
jobs are better served by something else.

**Copying a large database out of Azure SQL wholesale.** Use a `.bacpac` produced *server-side*,
so the extract never touches your connection. Not because the artifact is smaller — on the
database these notes were written against the two came out within 2% of each other, 488 MB against
498 MB, describing the same 403 tables — but because the extract runs inside Azure instead of
across your link:

```bash
az sql db export --resource-group <rg> --server <server> --name <database> \
  --admin-user <admin> --admin-password <password> \
  --storage-key-type StorageAccessKey --storage-key <key> \
  --storage-uri https://<account>.blob.core.windows.net/<container>/<database>.bacpac
```

The database-to-blob leg runs inside Azure at Azure speed, and you download one compressed file
instead of making a round trip per batch. For a full copy of a multi-gigabyte database this wins
by a wide margin. You will need `sqlpackage` or SSMS to import it, and Microsoft's advice is to
`CREATE DATABASE ... AS COPY OF` first and export the copy, because a `.bacpac` of a live database
is not transactionally consistent either.

**Copying a database between SQL Server instances you control.** Native `BACKUP`/`RESTORE` is
faster and loses nothing — users, permissions, filegroups, statistics, the lot. Azure SQL Database
cannot do it (Managed Instance can, with `COPY_ONLY` to a URL), which is the gap this tool grew
out of, but on-premises it is the right answer.

**Anything that needs a point-in-time snapshot.** Each table is read in its own statement, so rows
in one table can be newer than rows in another. A resumed export widens that window further. If
consistency across tables matters, copy or restore the database on the server first and dump the
copy.

**Users, roles and permissions.** Not captured — see
[what gets captured](#what-gets-captured) for the full list of what is left out. Script them with
[dbatools](https://dbatools.io) (`Export-DbaLogin`, `Export-DbaUser`) or SSMS's Generate Scripts,
which read the security catalogue directly. A `.bacpac` is only a partial alternative: it carries
database users and roles, but not the server logins behind them, so they restore orphaned.

Microsoft's `mssql-scripter` used to be the obvious cross-platform answer, and earlier versions of
this README recommended it. It is
[no longer maintained](https://github.com/microsoft/mssql-scripter) — the repository says so, and
its last release is still an alpha. Prefer dbatools.

**Loading one large table as fast as physically possible.** `bcp` with a native-format file will
beat this; it does one thing.

**Schema version control or migrations.** A dumper is not a migration tool. Flyway, Liquibase or
DacFx are.

### When it is the right tool

- Pulling *part* of a database — `--exclude-data` on a log table turned one 5 GB transfer into
  1.4 GB. `sqlpackage` can do that much with `/p:TableData=`, but not a row filter within a table,
  and never a subset of the schema.
- Keeping a database's shape in version control — see
  [dumping the schema without the data](#dumping-the-schema-without-the-data), which costs the same
  whether the database holds 4 MB or 4 TB.
- Wanting to read, grep or diff what you dumped, or edit `manifest.json` before restoring.
- Restoring somewhere with nothing installed: one static binary, no DacFx, no SSMS.
- Restoring into a differently named database, or a subset of tables, without ceremony.
- Checking that a restore actually matches its source, with `verify`.

## Notes and limits

- **Parameter limit.** A single `INSERT` cannot carry more than 2100 parameters, so on the INSERT
  path the effective batch size is `min(--batch-rows, 2000 / column-count)`. A table with more
  than 2000 columns can only be loaded by bulk copy; if it also contains a type bulk copy cannot
  encode, the load fails with an explicit error. The bulk path has no such limit.
- **`sql_variant`** round-trips through its string form. If you store non-string values in a
  `sql_variant` column they come back as strings.
- **Memory.** Both directions stream: rows are never all held at once, in either the archive
  writer or the loader.
- **Interrupting.** Ctrl-C aborts cleanly. A partial archive is left on disk for inspection; a
  partial restore is not rolled back beyond the current transaction.

## Testing

The integration tests build a database that exercises every supported type and object kind,
dump it, restore it into a second database, and compare the two. They need a SQL Server you can
create databases on:

```bash
DBDUMPER_TEST_DSN='sqlserver://DEVBOX/SQLEXPRESS?trusted_connection=true&encrypt=disable&protocol=lpc' \
  go test ./... -v
```

Without `DBDUMPER_TEST_DSN` those tests skip and the unit tests still run.
