# Contributing to agent-runner

Thanks for looking. One person maintains this and it is deliberately small. The most useful contribution is a small, finished one.

Anyone can contribute: people, agents, and whoever runs them. There is no CLA.

## Read this first

[PRD.md](PRD.md) is the source of truth for what this program does and why. If a change contradicts it, the conversation is about the PRD, not about the patch. Open an issue to argue with the design; don't land a PR that quietly disagrees with it.

## The size rule

This is the rule the rest of the document is downstream of.

**A pull request is one idea, and one idea fits in roughly 400 changed lines across at most 8 files.** That is a ceiling, not a target. Most good PRs here are well under it.

When a change doesn't fit:

1. Stop and say so on the issue.
2. Break the issue into the smallest pieces that each stand alone — a refactor that makes the change possible, the change, the tests that prove it, the docs that describe it.
3. Land the first piece. Come back for the rest.

A large PR gets asked to split, so splitting it yourself is faster for everyone. "It only makes sense all at once" is almost always false; it usually means the design hasn't been separated into steps yet.

Signs a PR is too big:

- the description needs more than one paragraph to explain what it does;
- it touches files with no relationship to each other;
- it mixes a refactor with a behaviour change — split those always, because a refactor that also changes behaviour cannot be reviewed and cannot be reverted;
- you cannot name the single thing a reviewer should check.

## Issues before code

Open an issue first for anything that is not a one-line obvious fix. It costs two minutes and it stops two people doing the same week's work.

An issue needs three things: what is wrong or wanted, how you would know it is fixed, and which part of the PRD it belongs to. If you cannot name the third, it is probably a design question.

## Setup

```bash
git clone https://github.com/marcus-beauregard/agent-runner.git
cd agent-runner
go build ./... && go test -race ./...
```

Go 1.22 or newer. No other toolchain.

## Gates

CI runs these three and so should you, before you push:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
```

- **`gofmt`** — non-negotiable. Formatting arguments belong nowhere near a code review.
- **`go vet`** — clean, not "clean except one thing".
- **`go test -race`** — the suite is small on purpose and should stay fast. If your change makes it slow, that is a design problem worth mentioning in the PR.

## Tests

A behaviour change comes with a test. Two exceptions, both of which need saying out loud in the PR: a pure documentation change, and a change you genuinely cannot test yet, which needs a written repro of the manual check instead.

- Prefer table-driven tests; they are the idiom and they make the failure message the useful part.
- Test the behaviour, not the implementation. If a refactor breaks your test, the test was pointing at the wrong thing.
- Name the case that would have caught the bug. `TestGateStopsAtFirstFailure` says more than `TestGate2`.

## Commit style

- Imperative subject, conventional prefix: `fix: reject an empty title`, `docs: state the licence`, `refactor: extract the gate runner`.
- One idea per commit. If the message needs "and", it is two commits.
- Body explains *why*, wrapped at 72 columns. Reference the issue there (`Fixes #12`), not in the subject.
- **Sign every commit: `git commit -s`.** CI and the DCO check both require it.

## Code

The house style is the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) plus [Effective Go](https://go.dev/doc/effective_go). Called out because they get skipped:

- Error strings are lowercase and unpunctuated: `fmt.Errorf("open config: %w", err)`, not `"Open config failed."`
- Wrap with `%w` so callers can test with `errors.Is`. A wrapped error that loses the cause is a bug.
- `context.Context` is the first parameter, never a struct field.
- Doc comments are full sentences starting with the name: `// Run executes one plan and returns its exit code.`
- No `panic` for ordinary errors. Return them.
- Prefer a nil slice (`var xs []T`) unless you are encoding JSON and need `[]`.
- Exported names get doc comments. So do non-obvious unexported ones.

Project-specific:

- **New code goes in `internal/`.** There is no public API while the design moves. If you think something must be exported, open an issue first.
- **No new dependencies without an issue.** The standard library is the default answer. A dependency needs a reason that survives being written down.
- **Security-relevant code is reviewed harder.** `internal/tools` and `internal/policy` decide what this program may execute on someone's machine. Changes there need tests that show the refusal, not only the happy path.

## Review

- Every review comment gets an answer, even if the answer is "you're right, changed" or "I disagree because X".
- Don't force-push over a review. Add commits; the maintainer squashes.
- If a review asks for a change that makes the PR too big, that is the size rule talking — split it.
- Rejection isn't personal. A closed PR costs the maintainer less than a merged one that has to be reverted.

## What gets closed without discussion

- A PR that bundles unrelated changes.
- A refactor with no issue behind it.
- Formatting-only churn in files the PR does not otherwise touch.
- A "fix" with no test and no repro.
- Anything that adds telemetry, analytics or a network call this project did not already make.
