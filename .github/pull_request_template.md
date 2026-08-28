## What

<!-- What changes, in a sentence or two. -->

## Why

<!-- The problem this solves. If it fixes a bug, describe the failure a user
     would have hit, not just the code that was wrong. -->

## How it was verified

<!-- Delete what does not apply. -->

- [ ] `go test ./...` passes
- [ ] Integration tests pass against a real SQL Server (`DBDUMPER_TEST_DSN` set)
- [ ] Round-trip checked by hand: export, import, `verify`
- [ ] Behaviour reproduced before the fix and confirmed gone after

## Notes for the reviewer

<!-- Anything deliberately left out, assumptions made, or parts that could not
     be tested here (Azure SQL, other server versions, other platforms). -->
