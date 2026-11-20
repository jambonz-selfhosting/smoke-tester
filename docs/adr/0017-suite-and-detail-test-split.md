# 0017. Split tests into a suite and a detail set with a build tag

- **Status:** Accepted
- **Date:** 2026-11-20
- **Deciders:** hoan.h.luu@jambonz.org
- **Tags:** testing, tooling

## Context

The verbs package grew to ~530 tests. Most vendor and feature files cover one
feature in depth: options, languages, cached replay, barge-in, two turns,
regressions. `make test-report` ran all of them, which took well over 20
minutes at `-parallel 4` and spent vendor credit on detail nobody needs on
every run. What we want from the everyday run is breadth: every jambonz
feature exercised once or twice, end to end.

## Decision

Every feature file is split in two:

- `<feature>_test.go` holds **at most 2** tests that show the feature works
  (the "suite"). It also keeps the file's helpers, so both builds compile.
- `<feature>_detail_test.go` starts with `//go:build detail` and holds every
  other test for that feature.

`make test-report`, `make test`, `make test-rest` and `make test-verbs` build
without the tag and run only the suite. `make full` (and `TAGS=detail` on the
others) adds the detail files. The single-test, single-file and release-gate
targets always build with the tag, so any test can still be run by name.

`scripts/check-test-layout.sh` (run by `make lint`, `make test-report` and
`make full`) fails when a suite file has more than 2 tests, a detail file is
missing the tag, or a non-detail file carries it.

Adding a test needs no list edit: put it in the feature's `_test.go` if it is
one of the two best tests for that feature, otherwise in `_detail_test.go`.

## Consequences

- Positive: the default report runs ~190 of ~535 tests and still touches every
  verb and vendor. Regression reproductions (`agent_prod_defects`) and
  parameter matrices (`nova_llm_s2s_params`) are detail by design.
- Positive: offline unit tests live in `*_unit_test.go`, are never capped and
  always run.
- Positive: the split is visible in the file tree and enforced by lint; there
  is no list of test names to keep in sync.
- Negative: a feature's tests live in two files. The detail file can use
  helpers from the suite file, but not the other way round: a helper that a
  suite test needs must stay in the suite file.
- Negative: `go vet ./...` alone misses detail files; `make lint` vets both
  builds. Raw `go test -run <detail test>` without `-tags detail` reports
  `[no tests to run]`, which reads like a pass.
- Neutral: files that hold several small features (`readonly_test.go`,
  `rtpengine_directional_block_test.go`, combined STT+TTS vendors) get only 2
  suite tests in total. Split such a file per feature if both halves need to
  be in the suite.

## Alternatives considered

### Option A — separate folders (`tests/verbs/suite`, `tests/verbs/detail`)
Rejected because a Go folder is a package, and the ~100 shared helpers live in
`_test.go` files that another package cannot import. Moving them into an
importable package is a large refactor of every test file for no gain over a
file suffix in the same folder.

### Option B — a static list of suite test names
Built first and rejected: every new test or rename needs a list edit, and the
list drifts from the code. A checker can catch drift but cannot remove the
chore.

### Option C — `testing.Short()` / an env check inside each detail test
Rejected because each test needs a guard line, a forgotten guard silently
puts a test in the suite, and skipped tests still show up as noise in
`report.html`.

## References

- ADR-0012 (go test as runner). The `drachtio` and `studio` build tags already
  keep long or costly packages out of the default run.
