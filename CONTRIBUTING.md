# Contributing

## Workflow

`main` is protected: it takes no direct pushes, and every change arrives through a pull request
whose CI run has passed. Merges are squashed and the branch is deleted afterwards, so `main`
stays a linear list of self-contained changes.

```bash
git switch -c fix/columnstore-index-ddl
# ...work...
go test ./...
git commit
git push -u origin HEAD
gh pr create --fill
```

Branch names are `<type>/<short-description>`, using the same types as the commit subject below.

## Commits

One logical change per commit, subject in the imperative mood, prefixed with a type:

```
fix: reject sort order on columnstore index DDL
feat: filter modules alongside tables
chore: add contributing guide
docs: correct the table sizing query
test: cover NULL parameter typing per column type
```

The body explains *why*, not what — the diff already says what. If a change fixes something a
user could hit, describe the failure it prevents.

## Tests

```bash
go test ./...
```

The unit tests need nothing but Go. The integration tests build a database exercising every
supported type and object kind, dump it, restore it into a second database and compare the two;
they skip themselves unless `DBDUMPER_TEST_DSN` points at a SQL Server you may create databases
on:

```bash
DBDUMPER_TEST_DSN='sqlserver://DEVBOX/SQLEXPRESS?trusted_connection=true&encrypt=disable&protocol=lpc' \
  go test ./... -race
```

Run the race detector at least once with a DSN set: the parallel table loading is only exercised
by the integration tests.

A change to how values are encoded, or to generated DDL, needs a case added to
`internal/integration/schema_test.go`. That suite round-trips through both the bulk-copy and the
INSERT path, so a new column type is covered on both by adding it to `dbo.BulkTypes` (if bulk
copy supports it) and `dbo.AllTypes`.

## Style

`gofmt` is enforced by CI. Beyond that: comments explain why a thing is done, not what the line
says, and anything discovered empirically about SQL Server or the driver gets written down where
the code depends on it — that knowledge is expensive to rediscover.
