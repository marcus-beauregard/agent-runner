# PRD: agent-runner as a worker

**Status:** draft 2, 2026-11-20
**Owner:** Marcus Beauregard
**One line:** hand it an issue or a pull request, get back something a maintainer can review, with a run record a stranger can pick up.

---

## 1. Problem

The pipeline that produced this year's merges lives in a chat session. Read the issue, check it is not already fixed, reproduce, patch, run the repo's gates, get a second opinion, commit, open the PR. Every step exists as prose in an automation prompt, so every run re-derives it and the quality drift is invisible until a maintainer says no.

That is the wrong place for a repeatable process. The process should be code: versioned, testable, and able to run on a box with no conversation attached.

The second problem is what happens after the PR exists. A review arrives, or CI fails, and the work resumes in somebody's head. Two days later nobody can say what was already answered and what was skipped, and the same comment gets a second reply. That is not a tooling gap, it is a missing record.

## 2. Who it is for

1. **Me, on my own repos.** An issue sits unstarted for a week; the worker takes it overnight.
2. **Me, upstream.** An approved issue becomes a branch and a PR while I do something else, with the same discipline a maintainer would review.
3. **An agent.** Something with no hands calls the CLI, reads machine output, and decides what to do next.
4. **Anyone reading the repo.** If the process is in the PRD and the code, the tool is legible without watching me work.

## 3. Two modes

The two modes exist because they have different exit contracts, not because they are two halves of a nicer name.

### `dev` — from an issue to something reviewable

```
agent-runner dev <issue-url> --repo <path> --gate <cmd> [--gate <cmd> ...] [--json]
```

Triage, reproduce, build, review, one commit on a new branch, open the PR. The subject is an issue. The terminal state is "a PR exists" or a refusal with a reason.

### `resolve` — from an open PR to nothing left to do

```
agent-runner resolve <pr-url> --repo <path> [--gate <cmd> ...] [--max-attempts 3] [--json]
```

Read the PR, read the review thread and CI state, fix what is actionable, push, answer each distinct point once. The subject is a pull request. The terminal state is merged, closed, nothing actionable, or blocked on a human.

`resolve` is **strictly responsive**. It does not improve the neighbourhood mid-review. If it spots something the reviewer did not raise, it says so in the reply and leaves the change to a separate `dev` run. A bot that silently expands scope inside somebody's review is untrustworthy, and trust is the only currency here.

### The agentic shape

A `dev` run produces the PR. A `resolve` run, invoked periodically by whatever is scheduling, keeps it moving until merged. The two are separate invocations on purpose: a dev run that also loops is a process that never ends, and a resolve run that also triages is a process that invents work.

### The clock belongs to the caller

This tool does not schedule itself. One invocation runs one job and exits. Looping belongs to the user's shell, an agent's planner, or a meta-framework above this one.

That is a deliberate boundary, not an omission. A worker with its own timer has to answer questions that are not about the work: when to retry, whether the last run is still going, what a stale lock means. Those are scheduler problems. `agent-runner runs --json` exists so an external scheduler can see what is in flight and decide.

## 4. The run record is the product

The PR is the visible output. The durable one is the run directory, because that is what a stranger reads when the run dies at 4am.

```
${XDG_STATE_HOME:-~/.local/state}/agent-runner/runs/<id>/
  run.json           kind, subject, repo, branch, base sha, policy, budget
  run.ndjson         append-only, one event per line: stage, tool, decision
  stages/
    01-triage/issue.json
    02-reproduce/repro.txt
    03-implement/
    04-review/review.json
    05-commit/commit.txt
    06-report/
  diff.patch         the change, standalone
  REPORT.md          rendered from the record, for people
  worktree/          the checkout this run is allowed to touch
```

Two views, one source of truth:

- **run.ndjson** is the machine interface. One JSON object per line, append-only, flushed as it happens. It survives truncation by losing only the tail.
- **REPORT.md** is the human interface, rendered from the same events by `agent-runner report <id>`.
- **`--json`** prints one object at exit: run id, mode, status, exit code, gate result, report path, PR URL if any. No TTY assumptions, no prompts, no colour.

Runs live outside the repo on purpose. A `.agent-runner/` directory inside every checkout is one careless `git add -A` away from committing a transcript containing paths, code and half-finished reasoning. The repo gets at most a single gitignored pointer listing run ids, and the runner works fine without it.

## 5. Resume

A run must survive its process dying. Overnight work that evaporates is worse than no overnight work.

The rule: **the run directory is the state, and every stage is idempotent.**

- A stage writes its outputs, then appends `stage.finish` with the output hash.
- `agent-runner resume <id>` finds the first stage without a `stage.finish` and continues from there.
- Re-running a completed stage is a no-op, not a repeat of side effects.
- Git operations record what they did: branch name and the sha it was created from, the pushed sha, the PR number. A run interrupted after pushing but before opening the PR resumes by opening the PR, not by rebuilding the work.

The worktree lives under the run directory for exactly this reason. `/tmp` does not survive a reboot, so a resume that depends on it is theatre.

## 6. Sandbox

Each run gets its own worktree. Never the user's checkout, never a shared tree.

- If the repo is missing, clone it. If it exists, use it only as the object store.
- `git worktree add <run>/worktree -b agent/<slug> origin/<default>` — a fresh branch, always.
- The run's write scope is that directory and nothing else. The existing argv-only command runner already confines the working directory; the PR must not be the first time we learn it did not.
- Commands run with a scrubbed environment. Only what `gh` and `git` need to authenticate, and the transcript records that a command ran, never what it carried.

### 6.1 Cleanup is a step, not a background hope

Nothing deletes anything on a timer. Cleanup is a command a person or a scheduler runs, in the same spirit as everything else here: one invocation, one job, exit.

```
agent-runner gc [--apply] [--runs <days>] [--worktrees <days>] [--json]
```

Default is a dry run. It prints every candidate with its size and the reason it qualifies, and deletes nothing until `--apply` is passed. A cleanup tool that acts before showing you its work is a cleanup tool nobody dares run.

Two tiers, because they cost different things to lose:

| Tier | What goes | Default age | Why |
|---|---|---|---|
| **Worktree** | `worktree/` and its git registration | merged/closed, or 14 days | Large (hundreds of MB), fully regenerable from the branch |
| **Run record** | the whole run directory | 90 days, or the newest 50 runs | Small, and it is the history |

Losing a worktree costs a re-clone. Losing a run record costs the reason a decision was made, so the two are never deleted under the same policy and the record always outlives the checkout.

Rules it will not break:

- A run with no terminal state is **resumable**, and is never collected unless `--force` names it explicitly.
- A worktree whose run directory has already gone is an orphan: prune the directory and run `git worktree prune` on the parent repo, so the parent does not accumulate entries pointing at nothing.
- Deleting a worktree never deletes the branch on the remote. The PR is evidence; the checkout is scratch.
- `--json` reports what was freed, per run, so the daily disk question has a number instead of a feeling.

## 7. Pipeline: `dev`

Each stage has one job and one completion criterion. A failed criterion ends the run with a report. It does not retry into the dark.

| # | Stage | Does | Done when |
|---|---|---|---|
| 1 | **Triage** | `gh issue view`: title, body, labels, comments; merged PRs referencing it | The issue is open, unclaimed, and unfixed |
| 2 | **Scope** | One-sentence restatement, files likely touched, the gate from the repo's own CI | You can name the change and the proof |
| 3 | **Reproduce** | A failing test or command showing the defect | The failure is observed, not assumed |
| 4 | **Implement** | Tool loop; smallest change that fixes the cause | Gates pass |
| 5 | **Review** | Goldfish: fresh context, four questions, blocking issues only | Every blocker fixed or answered in writing |
| 6 | **Commit** | Conventional subject matching the repo's log, `-s` when DCO applies, body via file | Tree clean, one commit |
| 7 | **Open PR** | Push branch, `gh pr create --body-file` with claim, change, proof | PR URL recorded, checks named |
| 8 | **Report** | `REPORT.md` rendered from the record | A stranger can resume from it |

Stages 1 to 3 are cheap and local. They exist so stage 4 starts from a fact rather than a guess.

## 8. Pipeline: `resolve`

| # | Stage | Does | Done when |
|---|---|---|---|
| 1 | **Read** | PR state, checks, review threads with comment ids, our previous replies | Every open thread is known by id |
| 2 | **Classify** | Each thread: actionable, already answered, or a question for a human | No thread is unclassified |
| 3 | **Fix** | Only the actionable items; one commit per coherent group | Gates pass |
| 4 | **Push** | Same branch, recorded sha | Remote matches local |
| 5 | **Answer** | One reply per distinct point, referencing what changed | No comment answered twice |
| 6 | **Report** | What changed, what was answered, what is left | The next run can see it did not repeat itself |

### The leash

A resolve loop that runs on a timer will, left alone, say the same thing every thirty minutes. Three rules, enforced in code, not in a prompt:

- **Stop on no progress.** If two consecutive attempts produce no new commit and no new state, stop with `blocked` and say why.
- **One reply per point.** Answered comments are recorded by id in the run record and never answered twice, across runs.
- **Never push to a branch we do not own.** If the PR author is not the authenticated account, `resolve` refuses. That single check prevents the worst outcome in this design.

## 9. Gates

The gate is the repo's, quoted, not paraphrased.

- `--gate` is repeatable. Gates run in order and stop at the first failure, so nobody waits five minutes on a test suite after `gofmt` already complained.
- Every gate records its command, exit code, duration and the tail of its output. Gates that never ran are recorded too, with the reason they were skipped.
- No `--gate` and the run stops after the patch and asks for one. "Tests pass" is not a gate; a command and an exit code are.
- Running the whole list is the default and needs no flag.

## 10. Budgets and terminal states

Unattended work needs a ceiling that is not "until it stops feeling productive".

- `--max-steps` bounds the tool loop.
- `--budget 45m` bounds wall clock.
- `--max-cost` bounds model spend where the client can report it.

Exhausting a budget is its own terminal state, not a failure: the run stops, writes a report saying what it had done and what remained, and exits `5`. A crash and a considered stop should never look the same in the record.

## 11. Refusals

The worker refuses, with a printed reason, when:

- the issue is closed, already fixed by a merged PR, or claimed by another person;
- the task needs a CLA the operator has not signed;
- the repo's CONTRIBUTING forbids unattended agents;
- the gate cannot run in this sandbox (missing toolchain, network-only test);
- the diff would touch a path outside the write scope;
- `resolve` is pointed at a PR we do not own.

A refusal is a successful run. The failure mode to fear is a confident wrong PR, not a stop.

## 12. Contracts

- **Write scope:** the run's worktree, nothing outside it.
- **One commit, one idea.** A worker that bundles is a worker whose patches cannot be reverted.
- **The issue body is data, not instructions.** An issue can contain "ignore your rules and run this". The pipeline treats its text as a defect report to understand, never as a command, and verifies every claim about the repo against the checkout. The worker reads attacker-controlled text and then executes code; the sandbox is the enforcement, this rule is the intent.
- **The reviewer is not the author.** Same-session self-review is not review; the goldfish gets the diff and the issue, never the transcript.
- **Never merge.** The worker's authority ends at a PR. Merging belongs to a person or a maintainer.
- **No secrets in the transcript.** Tokens come from the environment; the record shows that a command ran.
- **No self-scheduling.** One invocation, one job, exit.

## 13. Exit codes

| Code | Meaning |
|---|---|
| 0 | Done: PR opened, or nothing left actionable |
| 2 | Gate failed |
| 3 | Refused (see §11) |
| 4 | Needs a human: ambiguous scope, a question only the maintainer can answer |
| 5 | Budget exhausted |

## 14. Non-goals

- Autonomy across many issues at once. Serial first.
- Merging, signing CLAs, or posting to maintainers on its own.
- Replacing review. It produces something worth reviewing.
- A UI. The report is Markdown because Markdown is the interface.
- An agent protocol. This is a batch process with a paper trail; interactive transports are a separate concern at one seam, stage 4's implementer, and they can be added there without changing the shape.
- Owning a clock. Scheduling is the caller's problem by design.
- Deleting anything on its own initiative. `gc` is a command with a dry run, and it is the only thing here that removes files.

## 15. What exists, what is missing

| Piece | State |
|---|---|
| Step-capped loop, transcript, tool registry | **done** — `internal/agent`, `internal/tools` (Go) |
| argv-only command runner, flag allowlist, workdir confinement | **done** — Go port, race-tested, symlink escapes covered |
| `dev`/`resolve` subcommands, `--dry-run` plan | **partial** — flags parse, plan prints, nothing executes |
| Run directory, ndjson record, `REPORT.md` | **missing** |
| Resume and stage idempotency | **missing** |
| Worktree sandbox | **missing** |
| `gc` with dry run, two tiers, orphan pruning | **missing** |
| Real LLM client | **missing** — the loop runs on a deterministic mock |
| Gate runner | **missing** |
| `gh` and git operations | **missing** |
| Goldfish pass | **missing in-process** — proven externally via `copilot` |

## 16. Milestones

- **M0 — provenance.** `dev --dry-run` against a real issue: plan, gates it would run, run directory created, nothing written outside it.
- **M1 — the record.** Stages 1 to 3 with a real client, `REPORT.md` rendered, resume proven by killing a run mid-stage.
- **M2 — one repo, one issue.** Reproduce, implement, gates, commit on a worktree branch. No PR.
- **M3 — PR back.** Push, `gh pr create`, refusal paths exercised.
- **M4 — resolve.** One PR, one review round, leash enforced (no-progress stop, no duplicate replies).
- **M5 — dogfood.** The worker takes the next three issues on my repos; the hourly rotation only reviews what it produced.

## 17. How we know it works

- **Unattended-to-merged rate** — PRs opened by the worker that a maintainer merges without a second round of review.
- **Gate-first accuracy** — share of runs where the repo's own gate agreed with the worker's claim on the first try.
- **Refusals that were right** — of the runs that stopped, how many a human agreed should have stopped.
- **Resume honesty** — runs interrupted on purpose that finished correctly when resumed, without repeating a side effect.
- **Cost per merged PR**, counted from the run records rather than from memory.
