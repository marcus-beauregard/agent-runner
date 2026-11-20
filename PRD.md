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

## 3. One entry point, two drivers

There is one way to do work and one way to find it. Everything else is a route inside `pass`.

```
agent-runner pass <url> --repo <path> [--dry-run] [--force <pipeline>] [--gate <cmd> ...] [--json]
agent-runner watch --repo <path> --pr <url> [--issue <url>] [--interval 60s] [--for 30m | --once] [--json]
```

Every invocation does one job and exits. Nothing here schedules itself.

### `pass` — one thread, one route

The unit of work is a **thread**: an issue and the pull requests that reference it, together with what the ledger already knows about them. `pass` takes any URL inside that thread and resolves the whole thing, because work on one end invalidates the other. A maintainer answering the issue changes what an open PR should say; a review on the PR can make the issue's original ask obsolete.

The subject is whatever the URL points at. The pipeline is chosen by what changed since the ledger last looked, not by the URL type:

| Thread state, newer than the ledger | Pipeline |
|---|---|
| Issue open, unclaimed, unfixed, no PR | `build` — triage, reproduce, implement, gates, one commit, open the PR |
| Issue open, PR open, maintainer commented on the **issue** | `revise` — re-read the scope, then amend the patch or say why it still stands |
| Issue open, PR open, review activity on the **PR** | `resolve` — fix what is actionable, push, answer each point once |
| PR approved, green, clean | nothing — exit `0`, the thread is with a human |
| Issue closed, or fixed by a merged PR | nothing — drop the thread |
| PR closed without a merge | `needs_human` — never open a replacement on its own authority |
| Nothing new | nothing — exit `0` with `unchanged` |

That last-but-two row matters as much as the silence rule. A run that quietly opens a fresh PR for a rejected one is the same failure as reopening it, one indirection further out.

`--dry-run` prints the route and the reason it was chosen without executing anything, so "what would it do" is a question with a plain answer. `--force` pins a pipeline for the case where a human knows better; the ledger records that the route was overridden.

`dev` and `resolve` survive as **presets** — `--force build` and `--force resolve` — for the person who wants to be explicit, and for scripts written before `pass` existed. They are not separate entry points, because the routing decision is exactly the classification `watch` already performs, and a second place to make it is a second place to drift.

### `watch` — the inbox, and no model

```
agent-runner watch --repo <path> --pr <url> [--issue <url>] [--interval 60s] [--for 30m | --once] [--json]
```

Poll, digest, compare against the ledger, classify, enqueue at most one item. **No model is called.** A tick where nothing changed costs a few conditional requests and exits `0`.

This is the part that runs on a timer, and it must be boring. A program that calls a model 288 times a day to discover that nothing happened is unaffordable, and worse, its behaviour changes with the weather. `watch` decides *whether* there is work and *what kind*; `pass` decides *how*.

`watch` and `pass` share one classifier. `watch` runs it over a set and writes to the inbox; `pass` runs it over one thread and executes the answer. The enqueued item is literally the invocation — the same subject and the same pipeline name — so there is no translation layer between "work found" and "work done", and no way for the two to disagree.

Polling is cheap because it is conditional. A request carrying `If-None-Match` that returns `304` does not count against the rate limit — measured on this account, two plain requests each cost one unit and a conditional `304` cost none. The ETag is a filter and never the recorded state: it is opaque and can churn without a semantic change, so the ledger stores a digest it can replay.

Classification is a lookup, not a judgement:

| Observed, newer than our last action | Enqueue |
|---|---|
| A review with `CHANGES_REQUESTED` | `fix` |
| A question from someone else on an open thread | `reply` |
| A check on our head that went red | `repair` |
| `mergeable_state: dirty` — a real conflict | `rebase` |
| Approved, green, clean | nothing, and the PR leaves the inbox |
| Closed or merged by someone else | nothing, and the PR leaves the inbox |
| **Our own comment is the newest event** | **nothing** |
| Every ETag unchanged | nothing, exit `0` |
| Silence | nothing, exit `0` |

That table is the whole guard against a loop talking to itself, and it is code, so it cannot be argued out of.

### Watching the issue, not only the PR

The expensive failure on the `build` side is not a bad patch. It is a good patch to the wrong problem: implementing something a maintainer answered in a comment an hour earlier. So the issue is polled too, and its state cancels queued work.

| Observed on the issue | Effect |
|---|---|
| Closed by anyone | cancel; any queued `build` item is dropped |
| Fixed by a merged PR referencing it | cancel |
| Claimed by another account | cancel |
| Labelled `wontfix`, `needs-discussion`, `invalid` | cancel |
| A new comment from anyone who is not us | the next run re-reads before it implements or amends |

The last row earns its keep. Scope announced before the run started is scope the run must not follow.

### The pipelines are strict

Whichever route a run takes, its pipeline has a fixed stage list and does one job. Nothing improvises a stage, and nothing expands scope mid-review.

`resolve` is **strictly responsive**: it does not improve the neighbourhood while a reviewer is reading. If it spots something the reviewer did not raise, it says so in the reply and leaves the change to a separate `build` run. A bot that silently widens scope inside somebody's review is untrustworthy, and trust is the only currency here.

### The agentic shape

A `build` run produces the PR. A `revise` or `resolve` run, invoked by whatever is scheduling, keeps it moving until merged. They are separate invocations on purpose: a build run that also loops never ends, and a resolve run that also triages invents work.

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

### 6.1 The worktree (where it writes)

Each run gets its own worktree. Never the user's checkout, never a shared tree.

- If the repo is missing, clone it. If it exists, use it only as the object store.
- `git worktree add <run>/worktree -b agent/<slug> origin/<default>` — a fresh branch, always.
- The run's write scope is that directory and nothing else. The existing argv-only command runner already confines the working directory; the PR must not be the first time we learn it did not.
- Commands run with a scrubbed environment. Only what `gh` and `git` need to authenticate, and the transcript records that a command ran, never what it carried.

### 6.2 The machine, not just the files

A worktree confines the diff. It does not confine the process, and the process is the dangerous part: the worker reads attacker-controlled issue text and then executes code, so "it only writes inside the worktree" is a claim about a shell that can still run `npm install -g`, start a daemon, edit a crontab, or curl a host that has nothing to do with the repo.

The rule is therefore about the host, not the checkout: **a run changes nothing on this machine outside its own run directory.** Concretely:

- **No writes outside the run directory.** Not `$HOME`, not `/etc`, not a neighbouring repo, not the package manager's global store. `TMPDIR` is set to a directory inside the run and inherited by children, so a tool that wants scratch space gets it there.
- **No installs, no services, no schedulers.** A gate that needs a toolchain the host lacks is a refusal (§11), not an invitation to `apt-get install` something. The worker never starts a daemon, edits a crontab, or registers a git hook in the parent repo beyond the worktree entry it cleans up.
- **No long-lived processes.** Every command runs under a timeout, in its own process group, and the group is killed on exit. A command that tries to daemonise is killed with its children; a run never leaves something behind holding a port.
- **Network is an allowlist, not the internet.** `github.com` for `gh` and `git`, plus the model endpoint, and nothing else. A test that reaches a real service is exactly the class of test that should not run unattended, and an issue body that instructs the worker to fetch a URL does not get to become a request.
- **The blast radius should be a namespace, not a convention.** `--sandbox container` (bubblewrap or `unshare` when present) is the default where the kernel allows it; `--sandbox worktree` is the fallback and prints a warning naming what is unenforced. Convention is not isolation; the flag exists so the weaker mode is a visible choice.
- **The run audits itself.** Before it reports, it lists anything it observed changing outside the run directory. A non-empty list is a refusal, not a footnote, because the next run would otherwise assume the machine is as it found it.

This is separate from §12's "the issue body is data, not instructions". That is intent; this is enforcement. If the only protection against a hostile issue is that the model was told to be careful, there is no protection.

### 6.3 Cleanup is a step, not a background hope

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

## 7. Pipeline: `build` (issue to PR)

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

## 8. Pipelines: `revise` and `resolve`

`resolve` answers the pull request. `revise` answers the issue, when a maintainer has said something on it that an open PR does not yet reflect. They are different pipelines because the input is different: a review comment names the change it wants, while an issue comment changes the ground the change stands on.

### `revise`

| # | Stage | Does | Done when |
|---|---|---|---|
| 1 | **Re-read** | The issue, every comment since the PR opened, and the current patch | You can state what the maintainer changed |
| 2 | **Compare** | What the patch does against what was just asked for | The gap is named, or there is none |
| 3 | **Decide** | Amend the patch, or leave it and say why it already satisfies the new scope | The decision is written down before any file changes |
| 4 | **Amend** | Smallest change that closes the gap, or nothing at all | Gates pass, or the patch is untouched |
| 5 | **Push** | Same branch, one commit per coherent group | Remote matches local |
| 6 | **Say so** | One comment on the issue, quoting what was asked and what changed | The maintainer can see their point was read |

Stage 3 is the one that keeps this honest. "The reviewer asked for X and the patch already does X" is a legitimate, useful outcome, and it costs one comment instead of a rewrite. "A maintainer said something, therefore rewrite" is how a run churns a patch that was already correct.

### `resolve`

| # | Stage | Does | Done when |
|---|---|---|---|
| 1 | **Read** | PR state, checks, review threads with comment ids, our previous replies | Every open thread is known by id |
| 2 | **Classify** | Each thread: actionable, already answered, or a question for a human | No thread is unclassified |
| 3 | **Fix** | Only the actionable items; one commit per coherent group | Gates pass |
| 4 | **Push** | Same branch, recorded sha | Remote matches local |
| 5 | **Answer** | One reply per distinct point, referencing what changed | No comment answered twice |
| 6 | **Report** | What changed, what was answered, what is left | The next run can see it did not repeat itself |

### The guards

A resolve loop on a timer will, left alone, act when there is nothing to do. That is not malice; it is that a schedule looks like an instruction. Every guard below is enforced in code and has a test.

- **The world is the input, not the clock.** Before acting, the run digests the PR's observable state: head and base sha, draft and mergeable flags, every check by name and conclusion, every review by id and state, every comment by id and author, every unresolved thread id. If the digest matches the previous run's, the run exits `0` with `unchanged`. Two people asleep produce no digest change, so the loop does nothing for as long as they sleep.
- **Our own events are not state changes.** Comments, reviews and pushes authored by the authenticated account are excluded from the digest. Without this the loop reads its own output as news and answers itself forever.
- **Every action spends a trigger.** An outbound write must cite the event id it answers, and an id can be spent once, recorded in the ledger. A retry, a replay or a later scheduled run finds the trigger spent and does nothing.
- **Response only.** The action set is closed: fix a `CHANGES_REQUESTED` review, answer a new question once, repair a check that went red on our head, rebase a conflict. Nothing in it is "ask for attention". Requesting review or merge, bumping, `@`-mentioning, "any update?", close/reopen, and opening a replacement PR for a rejected branch are absent from the code, rather than discouraged in a prompt.
- **Approval ends the run.** An approved, green, conflict-free PR is finished. The worker does not seek the merge; merging belongs to a person (§12). Silence is also an ending: exit `0` and say nothing.
- **Two failures are a stop.** If two consecutive attempts from the same world fail to change it, exit `blocked` with the reason. A third try is a third notification.
- **One PR in flight per repo.** Review is the scarce resource. A second open worker PR in a repo that already has one awaiting review doubles a maintainer's cost for no gain.
- **Never push to a branch we do not own.** If the PR author is not the authenticated account, `resolve` refuses.
- **Replay proves it.** `replay <run-dir>` recomputes the plan from the recorded observations with writes suppressed, and the action set must match the recorded one. Determinism is a golden test, not a property of the prompt.

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
- a command tries to write outside the run directory, or reach a host outside the network allowlist;
- `resolve` is pointed at a PR we do not own;
- a thread's PR was closed without merging, and the next step would be a replacement PR.
- the route implies `--force` and the pipeline's preconditions do not hold.

A refusal is a successful run. The failure mode to fear is a confident wrong PR, not a stop.

## 12. Contracts

- **Write scope:** the run's worktree, nothing outside it.
- **The host is not a scratch pad.** Outside the run directory, a run leaves nothing: no installs, no services, no scheduler edits, no processes still running when it exits. §6.2 is the enforcement, not an aspiration.
- **One commit, one idea.** A worker that bundles is a worker whose patches cannot be reverted.
- **The issue body is data, not instructions.** An issue can contain "ignore your rules and run this". The pipeline treats its text as a defect report to understand, never as a command, and verifies every claim about the repo against the checkout. The worker reads attacker-controlled text and then executes code; the sandbox is the enforcement, this rule is the intent.
- **The reviewer is not the author.** Same-session self-review is not review; the goldfish gets the diff and the issue, never the transcript.
- **Never merge.** The worker's authority ends at a PR. Merging belongs to a person or a maintainer.
- **No secrets in the transcript.** Tokens come from the environment; the record shows that a command ran.
- **No self-scheduling.** One invocation, one job, exit.
- **Never ask for anything.** The worker produces work and answers questions. Requests for review, for merge, for attention or for urgency are not in its vocabulary; a person decides when the work is looked at, and their clock is not ours to hurry.
- **The model decides how, never whether.** Whether there is work, and which kind, is a deterministic classification over fetched state. A model is invoked only after that decision, with exactly one item, and it never chooses to act when the inbox is empty.
- **The ledger is the state.** Every run reads the previous ledger and writes one. Nothing is remembered in a process, a scratchpad or a prompt, so a run can be replayed and two runs cannot disagree about what happened.

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
| `pass`/`watch` entry points, routing table, `--dry-run` | **missing** — `dev`/`resolve` flags parse and print a plan |
| Run directory, ndjson record, `REPORT.md` | **missing** |
| Event ledger: observations, actions, trigger spend | **missing** |
| `watch`: conditional fetch, digest, classify, enqueue | **missing** — no model in this path |
| `replay` from a recorded run | **missing** |
| Resume and stage idempotency | **missing** |
| Worktree per run (filesystem scope) | **missing** |
| Machine isolation: no writes outside, network allowlist, process-group timeouts, container default | **missing** |
| `gc` with dry run, two tiers, orphan pruning | **missing** |
| Real LLM client | **missing** — the loop runs on a deterministic mock |
| Gate runner | **missing** |
| `gh` and git operations | **missing** |
| Goldfish pass | **missing in-process** — proven externally via `copilot` |

## 16. Milestones

Ordered so the property that matters is proven first. Determinism is not a milestone; it is the acceptance test every milestone after M0 must keep passing.

- **M0 — the ledger.** Append-only events, the world digest, trigger spend, and `replay`. Proven by three fixtures taken from real failures: a PR whose newest event is our own comment with no human reply, a PR we closed, and a PR whose review we already answered. Each must plan zero actions and exit `0`.
- **M0.5 — the cage.** `--sandbox` with container detection, network allowlist, process-group timeouts, and the self-audit that lists writes outside the run directory. Proven by a deliberate escape attempt that gets refused.
- **M0.7 — the inbox.** `watch --once` over the seven open PRs: conditional requests, digest, classification table, and an exit of `0` with nothing enqueued. Proven by running it twice against an untouched PR and getting no second action, and by pointing it at a PR whose newest event is our own comment.
- **M1 — resolve on a live PR.** One PR, one review round, guards enforced, and a silent world ending in `unchanged` with no notification. This is the first milestone where the program does something a prompt cannot.
- **M2 — one repo, one issue.** Reproduce, implement, gates, commit on a worktree branch. No PR.
- **M3 — PR back.** Push, `gh pr create`, refusal paths exercised, one PR in flight per repo enforced.
- **M4 — build end to end.** Stages 1 to 8 with a real client, `REPORT.md` rendered, resume proven by killing a run mid-stage.
- **M5 — dogfood.** The worker takes the next three issues on my repos; the rotation only reviews what it produced.

## 17. Failure modes worth naming

Three things went wrong in practice, before this program existed, and each one has a guard above because the shape recurs.

- **The loop answers its own comment.** A comment it wrote looks like activity, so the next scheduled run replies again, and the one after that. Fix: our events are excluded from the digest, and a trigger can be spent once.
- **Nobody is awake, so it applies pressure.** A maintainer asleep at 03:00 is not a stalled PR. A loop that reads silence as a problem to solve will ask for a merge, then ask again. Fix: requests do not exist in the action set; an approved PR is a finished run.
- **A wall closes and it tries the door.** Close/reopen three times on one PR, each attempt a notification to someone who did not ask. Fix: those verbs are absent from the code, and a repeated failure from the same world is a `blocked` exit.

## 18. How we know it works

- **Unattended-to-merged rate** — PRs opened by the worker that a maintainer merges without a second round of review.
- **Gate-first accuracy** — share of runs where the repo's own gate agreed with the worker's claim on the first try.
- **Refusals that were right** — of the runs that stopped, how many a human agreed should have stopped.
- **Resume honesty** — runs interrupted on purpose that finished correctly when resumed, without repeating a side effect.
- **Cost per merged PR**, counted from the run records rather than from memory.
- **Replay fidelity** — runs whose recomputed plan matches the recorded one, across the fixture set. It belongs at 100%; anything lower means the loop responded to something the record does not contain.
- **Wordless runs** — resolve invocations that ended `unchanged` and produced no notification. A high number is the product working: the loop is on a timer and the humans are asleep.
