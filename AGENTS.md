# AGENTS.md

You are working in agent-runner. The contract is one line: **one idea, inside the budget, gates green — or stop and say why.**

Read [PRD.md](PRD.md) for behaviour and [CONTRIBUTING.md](CONTRIBUTING.md) for house style. This file is the procedure.

## The budget

**One idea, roughly 400 changed lines, at most 8 files.** That is the ceiling; most good changes are well under it.

When the change exceeds the budget, the ticket gets split, not the PR grown. Post the pieces on the issue — preparation, change, tests, docs, in that order — then implement only the first piece and name the rest in the PR body. A ticket that cannot fit in one reviewable change is a ticket that needs breaking up, and saying so is a complete contribution.

## Steps

Work these in order. Each ends on a condition you can check.

| # | Step | Done when |
|---|---|---|
| 1 | **Read** the issue in full, comments included | You can state what "fixed" means in one sentence |
| 2 | **Claim** it on the issue, or confirm it is unclaimed | The thread shows you on it |
| 3 | **Scope** the change to files the issue names | You can name the single thing a reviewer should check |
| 4 | **Build** the smallest change that fixes the cause | The failure reproduces before, and does not after |
| 5 | **Verify** with the gates | All three green on the final code, output pasted into the PR |
| 6 | **Commit** one idea, signed | `git commit -s`, one commit, subject in the imperative |
| 7 | **Open** the PR with the four-line body below | A stranger knows what changed, why, how it was checked, and what is left |

Step 3 is where runs go wrong. If scoping reveals the change is bigger than the budget, return to the budget section: split the issue and take the first piece.

## When to stop

Stop, write the blocker on the issue, and end the run. A blocked run that explains itself is a success; a confident wrong PR costs the maintainer more than an empty thread.

Stop when:

- "fixed" is still ambiguous after reading the issue and its comments;
- the change needs a design decision PRD.md does not make;
- the work exceeds the budget and the issue does not split cleanly;
- a gate fails for reasons unrelated to your change;
- the bug is not where the issue says it is.

## The PR body

Four short sections:

1. **What changed** — one paragraph, plain.
2. **Why** — the issue, and the behaviour that was wrong.
3. **Verified** — gate output, or the manual repro you ran.
4. **Left** — remaining pieces and anything you decided against.

If you are an automated agent, say so here. It does not change the review; the maintainer deserves to know how the patch arrived.

## House style

Errors wrap with `%w` and read lowercase. `context.Context` leads the parameter list. Doc comments are sentences that begin with the name. New code lives in `internal/`, and nothing new is exported.

`internal/tools` and `internal/policy` decide what may run on a machine. A change there carries a test that shows the refusal, not only the path that works.

The full list is [CONTRIBUTING.md §Code](CONTRIBUTING.md#code).
