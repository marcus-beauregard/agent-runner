# AGENTS.md

Instructions for coding agents working in this repository. A human contributor should read [CONTRIBUTING.md](CONTRIBUTING.md) instead; this file is the same contract with the parts agents get wrong spelled out.

## Before you write anything

1. Read [PRD.md](PRD.md). It is the source of truth for behaviour. A patch that contradicts it is wrong even if it compiles.
2. Read the issue you are working on, in full, including comments. Someone may have said the important part in the third comment.
3. **If there is no issue, stop and ask for one.** The exception is a one-line obvious fix. An agent that invents work is an agent that produces PRs nobody wanted.

## The size budget

**Hard limit: about 400 changed lines, at most 8 files, one concern.** This is not advisory. A PR over it gets closed with a request to split, and the split has to happen before the work counts.

When the change doesn't fit:

- Do not shrink it by hiding things in a bigger refactor, and do not split it by file to fake the numbers.
- Post on the issue what the pieces are — typically: the preparation, the change, the tests, the docs.
- Implement **only the first piece**, open the PR for that, and say in the PR body which pieces remain.

A single ticket that cannot fit in one reviewable change is a **ticket that needs breaking up**. Say so on the issue rather than delivering a large PR that the maintainer now has to decompose.

## What one PR may contain

- One behaviour change, or one refactor, or one documentation change.
- The tests that prove it.
- Nothing else.

Specifically not:

- **No opportunistic refactors.** A file you touched for a good reason is not a licence to reformat the rest of it.
- **No dependency changes.** Not even a version bump. Ask on an issue.
- **No unrelated formatting.** `gofmt` on files you changed is required; `gofmt` across the repo is a separate PR and nobody asked.
- **No renames** outside the change's own scope.
- **No workflow, licence, or governance edits** without being asked explicitly.
- **No new abstraction** with one caller. Write the direct code; the abstraction can come when the second caller exists.

## Gates before you open the PR

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
```

All three, clean, on the final code. Not "on the version before I made the last edit". Paste the result into the PR body. If a gate fails and you cannot fix it inside the size budget, stop and report that — an honest blocked PR is worth more than a green one that skipped something.

## Commits

- Conventional prefix, imperative, one idea: `feat: add the gate runner`.
- **Sign every commit: `git commit -s`.** The DCO check is not optional.
- Body says why, wrapped at 72. Reference the issue there.
- Don't force-push a branch under review. Add a commit.

## Code rules that are not negotiable

- Errors: lowercase, unpunctuated, wrapped with `%w`. No `panic` for ordinary failures.
- `context.Context` first parameter, never a field.
- Doc comments are full sentences starting with the name.
- New code goes in `internal/`. Nothing new is exported.
- Security-relevant changes — `internal/tools`, `internal/policy` — need tests that demonstrate the **refusal**, not only the path that works. This program decides what may run on a machine. Prove the boundary holds.

## When to stop instead of guessing

Stop, write down the blocker on the issue, and end the run if:

- the issue is ambiguous about what "fixed" means;
- the change requires a design decision the PRD does not make;
- the fix needs more than the size budget;
- a gate fails for a reason unrelated to your change;
- you find the bug is not where the issue said it was.

Guessing produces a confident wrong PR, and a confident wrong PR from an agent costs a maintainer more than an empty issue thread. A refusal with a reason is a successful run. This project is built on that idea; apply it to yourself.

## The PR body

Four things, short:

1. **What changed** — one paragraph, no marketing.
2. **Why** — the issue, and the specific behaviour that was wrong.
3. **How it was verified** — the gate output, or the repro you ran by hand.
4. **What is not done** — remaining pieces, known gaps, anything you decided against.

If you are an automated agent, say so in the PR body. Not because it changes the review, but because the maintainer deserves to know how the patch got here.
