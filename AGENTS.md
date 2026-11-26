# AGENTS.md

Rules for AI coding agents in this repository.

## Read first

Read `docs/architecture.md` before you change behavior. It is the contract. A difference from it is a defect. A spec change updates the spec in the same pull request. A section marked Planned is intent, not contract: build it only after a change moves it into the contract.

## Public repository

This repository is public. No file, commit message, branch name, pull request title or body, or comment may name an internal project, host, address, bucket, person or product of a caller. Name motte, which is public. Use "a control plane" or "a compute service" for any other caller or dependency.

The CI job `leak` runs `leaklint` of `majorcontext/tools` against the secret `LEAK_DENYLIST`. The job fails when the secret is unset. Never commit denylist content.

## Tests

- Write the failing contract test first for a change in what a caller receives.
- Write a unit test only for pure code: codecs, the token, ref pattern matching.
- Name the failure a test catches before you write it.
- Added test lines stay at or below added code lines.
- No wall-clock waits in tests: `time.Sleep` and `time.After` only inside a `synctest.Test` bubble; otherwise use channels. A wait that no bubble can hold, such as a cross-process wait, carries a `//lint:allow wallclock REASON` comment on its line. The analyzer `wallclock` of `majorcontext/tools` enforces this.
- Every top-level `TestXxx` calls `t.Parallel()`, or `testdata/serial-tests.txt` lists it as `package TestName reason`. The analyzer `paralleltests` enforces this.
- Run Go tests with `-race`.
- A test is at most 10 s and a package at most 10 s, unless `testdata/test-time-budget.txt` says more. Make a slow test fast; raise a budget only with a stated reason in the same change. `testtime` of `majorcontext/tools` enforces the budget in CI only, in its reference environment, where each package runs alone (`go test -p 1`) and a package time is its own cost. The land check `test` runs every test with `-race` and checks correctness, not time.

## Provider seams

- Put each external provider behind an interface that the consumer owns. See `docs/architecture.md` "Provider seams".
- Give each seam a conformance suite that every implementation runs, and at least two implementations. A local or in-memory implementation counts.
- Import a provider SDK only in an implementation package or under `cmd/`.
- Name a provider host only in a file of `testdata/provider-hosts.txt`.
- Update the seams table in the same diff as a change to a seam.
- A seam that does not meet the rule is a gap row in the table and a line of `testdata/seam-gaps.txt`. The list only shrinks.
- `internal/seams` enforces these rules in `go test`. It stays in this repository because the shared `seams` package has no `Contract:` row, which the table of ward uses.

## Comments

Write no comment by default. A comment states why: a constraint, a hazard, or a rejected alternative. It never restates the code. It carries no history, no date, no person, no pull request number and no path to justify the code.

## Style

- Use standard Go style. Run `gofmt` and `go vet`.
- Use ASD-STE100 Simplified Technical English for prose. Use one term for one concept and the active voice.
- Write tooling in Go.

## Commits and pull requests

- Use Conventional Commit subjects: `type(scope): description`, lowercase, no final period.
- Squash merge.
- A commit body gives the problem, the design, the semantic change and the verification. Hard-wrap it at 76 columns.
- Do not hard-wrap a pull request body.
- Add no attribution footer.

## Checks

```bash
go build ./...
go vet ./...
go tool lint ./...
go test -race -count=1 -p 1 -json ./... | go tool testtime
test -z "$(gofmt -l .)"
LEAK_DENYLIST="$(cat denylist.txt)" go tool leaklint
```

`ward check` runs the checks of `.ward/checks.json` locally. The `chart` and `leak` jobs stay on GitHub.
