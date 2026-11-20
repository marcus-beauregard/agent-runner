# Contributing to agent-runner

Thanks for looking. One person maintains this and it stays small on purpose. The most useful contribution is a finished small one.

Anyone can contribute: people, agents, and whoever runs them. There is no CLA.

[PRD.md](PRD.md) is the source of truth for behaviour. If a change contradicts it, the conversation is about the PRD. Open an issue to argue with the design; a patch that quietly disagrees gets closed.

## Start from an issue

Open an issue before writing code, unless the fix is a single obvious line. It costs two minutes and stops two people spending the same week.

A useful issue says three things: what is wrong or wanted, how you would know it is fixed, and which part of the PRD it belongs to. If you cannot name the third, it is a design question, and the design conversation happens first.

## The budget

**One idea, roughly 400 changed lines, at most 8 files.** A ceiling, not a target.

When the change exceeds it, the ticket splits, not the PR. The pieces are usually: the preparation that makes the change possible, the change, the tests that prove it, the docs that describe it. Land the first, come back for the rest.

A change is over the budget when any of these is true:

- the description needs more than one paragraph to say what it does;
- it touches files with no relationship to each other;
- it mixes a refactor with a behaviour change, which makes the diff unreviewable and impossible to revert;
- you cannot name the single thing a reviewer should check.

## Setup

```bash
git clone https://github.com/marcus-beauregard/agent-runner.git
cd agent-runner
go build ./... && go test -race ./...
```

Go 1.22 or newer. Nothing else.

## Gates

These three, green, before you push. CI runs the same commands:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
```

`gofmt` output is empty, `vet` is silent, the suite passes under the race detector. A gate that fails for an unrelated reason is worth saying out loud in the PR rather than working around.

## Tests

A behaviour change ships with a test. Two exceptions, both stated in the PR: a documentation-only change, and a change you genuinely cannot test yet, which needs a written manual repro instead.

Table-driven tests are the idiom here, and the failure message is the part that earns its keep. Name the case that would have caught the bug: `TestGateStopsAtFirstFailure` says more than `TestGate2`. Test behaviour, so a refactor does not break your test for the wrong reason.

## Commits

Imperative subject with a conventional prefix: `fix: reject an empty title`, `docs: state the licence`, `refactor: extract the gate runner`.

One idea per commit; if the message needs "and", it is two commits. The body explains why, wrapped at 72 columns, with the issue reference there (`Fixes #12`) rather than in the subject.

Sign every commit with `git commit -s`. CI checks the DCO.

## Code

The house style is [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) plus [Effective Go](https://go.dev/doc/effective_go). The parts that get skipped:

- Errors read lowercase and unpunctuated, and wrap with `%w`: `fmt.Errorf("open config: %w", err)`. A wrap that loses the cause is a bug.
- `context.Context` leads the parameter list and lives in no struct.
- Doc comments are full sentences beginning with the name: `// Run executes one plan and returns its exit code.`
- Ordinary failures return an error. `panic` is for programmer error alone.
- A nil slice (`var xs []T`) unless JSON encoding needs `[]`.
- Exported names and non-obvious unexported ones carry doc comments.

Project rules:

- New code goes in `internal/`. The design is still moving, so there is no public API to keep compatible; if something feels like it must be exported, raise an issue first.
- The standard library is the default answer. A dependency needs a reason that survives being written down, and that discussion happens on an issue before the import appears.
- `internal/tools` and `internal/policy` decide what may execute on someone's machine. Changes there carry a test that demonstrates the refusal, not only the path that works.

## Review

Answer every comment, including with "you're right, changed" or "I disagree because X". Add commits rather than force-pushing over a review; the maintainer squashes. If a requested change pushes the PR past the budget, that is the budget talking: split it.

Rejection is not personal. A closed PR costs a maintainer less than a merged one that has to be reverted.

## Before you open the PR

- The diff contains the files the issue names, and the tests for them.
- One behaviour change, or one refactor, or one doc change.
- Gates green on the final code, output in the body.
- Commits signed.
- The body answers: what changed, why, how it was verified, what is left.
