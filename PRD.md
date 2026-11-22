# PRD: agent-runner as a worker

**Status:** draft 3, 2026-11-22
**Owner:** Marcus Beauregard
**One line:** hand it an issue or a pull request, get back something a maintainer can review, with a run record a stranger can pick up.

---

## 1. Problem

The pipeline that produced this year's merges lives in a chat session. Read the issue, check it is not already fixed, reproduce, patch, run the repo's gates, get a second opinion, commit, open the PR. Every step exists as prose in an automation prompt, so every run re-derives it and the quality drift is invisible until a maintainer says no.

A repeatable process belongs in code: versioned, testable, runnable on a box with no conversation attached.

The second problem is what happens after the PR exists. A review arrives, or CI fails, and the work resumes in somebody's head. Two days later nobody can say what was answered and what was skipped, and the same comment gets a second reply. The tooling is fine; nothing records what happened.

## 2. Who it is for

1. **Me, on my own repos.** An issue sits unstarted for a week; the worker takes it overnight.
2. **Me, upstream.** An approved issue becomes a branch and a PR while I do something else, with the same discipline a maintainer would review.
3. **An agent.** Something with no hands calls the CLI, reads machine output, and decides what to do next.
4. **Anyone reading the repo.** If the process is in the PRD and the code, the tool is legible without watching me work.

## 3. Three pipelines, two provisioning commands, and nothing that stays running

This binary reads and writes. It does not poll, watch, loop, retry on a timer, or hold a lock between runs. One invocation, one pipeline, one exit.

```
agent-runner build   <issue-url> --repo <path> [--gate <cmd> ...] [--json]
agent-runner resolve <pr-url>    --repo <path> [--gate <cmd> ...] [--max-attempts 3] [--json]
agent-runner amend   <pr-url>    --repo <path> [--issue <url>] [--gate <cmd> ...] [--json]
```

Each answers one question:

- **`build`** — can this issue become something a maintainer will review?
- **`resolve`** — is there anything left in this review that we owe an answer to?
- **`amend`** — has the issue moved since the PR was written, and does the patch still stand?

Two more commands take a repo and never a URL:

```
agent-runner env-check [--repo <path>] [--json]
agent-runner env-init  [--repo <path>] [--apply] [--json]
```

`env-check` answers whether the environment this repo declares is present and intact. `env-init` builds it, printing the plan unless `--apply` is passed. Neither touches a thread: they answer for the machine, and §6.4 is the contract.

They are separate commands because a pipeline that installs a toolchain is a pipeline that changes the host, which is the one thing §6.2 forbids a run to do. And because "this machine cannot run this repo" has to arrive at the front door — `env-check` before any work, exit `6` (§13) — instead of at the gate, after the work, looking like a patch that broke the tests.

Nothing chooses which of the three pipelines to run. That is the caller's job, and the reason this stays a set of commands rather than a framework: scheduling, polling, queues, leases and work-in-progress limits belong to whatever sits above this and knows what it wants to spend.

### The thread is the unit, even for one command

A pipeline command takes one URL and follows the thread from it: the issue and the pull requests that cite it, together, with whatever the ledger already knows. A maintainer answering the issue changes what an open PR should say, and a review on the PR can make the issue's original ask obsolete. Neither can be judged from one URL alone, and reading the other side costs one API call.

Where a thread disagrees with the command, the command loses:

| Thread state | What the command does |
|---|---|
| Issue already closed, or fixed by a merged PR | refuse — exit `3`, say which |
| Issue claimed by another account | refuse |
| PR approved, green, clean | nothing left to do — exit `0` |
| PR closed without a merge, and a replacement would be the next step | refuse, `needs_human` — exit `4` |
| Nothing newer than the ledger | exit `0` with `unchanged` |

The fourth row is the one that gets skipped. Opening a fresh PR for a rejected one repeats the reopening failure one indirection further out.

### Every command is safe to run twice

Four properties make a command safe to call from a loop somebody else wrote:

- **Read before acting.** The command digests the thread's observable state — head and base sha, checks by name and conclusion, reviews by id and state, comments by id and author — and compares it with the ledger. Nothing newer means exit `0` with `unchanged`, no model call, no writes.
- **Our own events are not state changes.** Anything authored by the authenticated account is excluded from the digest, so a command cannot read its own comment as news and answer it.
- **Every action spends a trigger.** An outbound write cites the event id it answers, and an id can be spent once, in the ledger. A retry, a replay or a caller's loop finds it spent and does nothing.
- **Silence is a result.** A command that finds nothing to do exits `0` and says nothing to anyone but its caller.

A caller may run these as often as it likes. Running one twice costs a handful of conditional requests and changes nothing.

### The issue is part of the job, not a separate watch

The expensive failure on the `build` side is a good patch to the wrong problem: work on something a maintainer answered in a comment an hour earlier. So `build` reads the issue comments as part of stage 1 and refuses when the issue already moved:

| Seen while reading the issue | Effect |
|---|---|
| Closed by anyone | refuse, exit `3` |
| Fixed by a merged PR referencing it | refuse |
| Claimed by another account | refuse |
| Labelled `wontfix`, `needs-discussion`, `invalid` | refuse |
| A comment newer than the claim, from someone else | re-read it, and implement what it says |

The last row is the cheap one. Scope announced before the run started is scope the run has to follow, and finding out takes one call.

### Each pipeline is a chain, and it is strict

Each command runs a fixed stage list and does one job. Nothing improvises a stage, and nothing expands scope mid-review. Stages share one tool set and one record shape, which is what lets the three pipelines be run back to back without a translation layer between them.

`resolve` is **strictly responsive**: it does not improve the neighbourhood while a reviewer is reading. If it spots something the reviewer did not raise, it says so in the reply and leaves the change to a separate `build` run. Widening scope inside somebody's review is how a contributor stops being asked back.

### The agentic shape

`build` produces the PR. `resolve` and `amend` keep it moving, each invoked when the caller decides it is worth invoking. They are separate invocations on purpose: a build that also loops never ends, and a resolve that also triages invents work.

### The clock belongs to the caller

This tool does not schedule itself. One invocation runs one job and exits. Looping belongs to the user's shell, an agent's planner, or a meta-framework above this one.

A process that stays running has to answer questions that are not about the work: when to retry, whether the last run is still going, what a stale lock means, what to do when the same thread is picked twice. Those belong to the layer that knows what it wants to spend. Keeping them out keeps the pipelines testable, and lets the same commands run from a shell, a cron, a queue worker or a terminal with no conditional in the code.

`agent-runner runs --json` exists so an external scheduler can see what is in flight and decide.

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

Three ways to read the same events:

- **run.ndjson** for machines. One JSON object per line, append-only, flushed as it happens, so a truncated file loses only its tail.
- **REPORT.md** for people, rendered from the same events by `agent-runner report <id>`.
- **`--json`** at exit, one object: run id, mode, status, exit code, gate result, report path, PR URL if any. A command that ends before a run exists — `env-check`, `env-init`, or any exit `6` — prints one JSON object with no run id: the repo, the digest, and what is missing or was planned (§6.4). No TTY assumptions, no prompts, no colour.

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
- Commands run with a scrubbed environment. Only what `gh` and `git` need to authenticate, plus the anchor the environment sets (§6.4) — `PATH` into `envs/<digest>/bin` and its install prefixes. The runner exports that anchor; it is never inherited from the operator's shell, and the transcript records that a command ran, never what it carried.

### 6.2 The machine, not just the files

A worktree confines the diff. It does not confine the process, and the process is the dangerous part: the worker reads attacker-controlled issue text and then executes code, so "it only writes inside the worktree" is a claim about a shell that can still run `npm install -g`, start a daemon, edit a crontab, or curl a host that has nothing to do with the repo.

The rule is therefore about the host, not the checkout: **a run changes nothing on this machine outside its own run directory.** Concretely:

- **No writes outside the run directory.** Not `$HOME`, not `/etc`, not a neighbouring repo, not the package manager's global store. `TMPDIR` is set to a directory inside the run and inherited by children, so a tool that wants scratch space gets it there.
- **No installs, no services, no schedulers.** A run installs nothing on this host. The toolchain it needs was provisioned before it started, by a command that is not a run (§6.4), and is read-only to the run. A gate that meets a program the environment does not have is a refusal (§11), not an invitation to `apt-get install` something. The worker never starts a daemon, edits a crontab, or registers a git hook in the parent repo beyond the worktree entry it cleans up.
- **No long-lived processes.** Every command runs under a timeout, in its own process group, and the group is killed on exit. A command that tries to daemonise is killed with its children; a run never leaves something behind holding a port.
- **Network is an allowlist, not the internet.** `github.com` for `gh` and `git`, plus the model endpoint, and nothing else. A test that reaches a real service is exactly the class of test that should not run unattended, and an issue body that instructs the worker to fetch a URL does not get to become a request.
- **The blast radius should be a namespace, not a convention.** `--sandbox container` (bubblewrap or `unshare` when present) is the default where the kernel allows it; `--sandbox worktree` is the fallback and prints a warning naming what is unenforced. Convention is not isolation; the flag exists so the weaker mode is a visible choice.
- **The run audits itself.** Before it reports, it lists anything it observed changing outside the run directory. A non-empty list is a refusal, not a footnote, because the next run would otherwise assume the machine is as it found it.

This is separate from §12's "the issue body is data, not instructions": that rule states intent, this one enforces it. If the only protection against a hostile issue is that the model was told to be careful, there is no protection.

### 6.3 Cleanup is a step, not a background hope

Nothing deletes anything on a timer. Cleanup is a command a person or a scheduler runs, in the same spirit as everything else here: one invocation, one job, exit.

```
agent-runner gc [--apply] [--runs <days>] [--worktrees <days>] [--envs <days>] [--json]
```

Default is a dry run. It prints every candidate with its size and the reason it qualifies, and deletes nothing until `--apply` is passed.

Three tiers, because they cost different things to lose.

| Tier | What goes | Default age | Why |
|---|---|---|---|
| **Worktree** | `worktree/` and its git registration | exit 0, as the run's last step; otherwise merged/closed or 14 days | Large (hundreds of MB), regenerable from the branch once the work is on it |
| **Run record** | the whole run directory | 90 days, or the newest 50 runs | Small, and it is the history |
| **Environment** | `envs/<digest>` | 30 days unused, or all but the newest few per repo; never a digest a run is bound to or a lock is held on | The largest thing this tool creates, and regenerable by `env-init` from a declaration it can still read |

Losing a worktree costs a re-clone. Losing a run record costs the reason a decision was made, so the two are never deleted under the same policy and the record always outlives the checkout. An environment costs a re-provision, and the run record that names its digest outlives it too: `env-check` on a collected digest says not ready, never a run that finds half a toolchain.

A run whose work is settled gives its tree back itself. Exit 0 is the only code whose change reached the branch, so the checkout is scratch from that moment, and the run removes it as its last recorded step — `worktree:remove` in the stream, next to the `worktree:add` that made it. Every other ending keeps its tree for `gc`, by age: the gate runs before the commit stage, so a run that failed (1), was refused by the gate (2) or policy (3), or stopped for a person (4) may be holding the only copy of the attempt and of why it was stopped, and a resume needs the tree it was editing. A run with no terminal state is resumable and is never a cleanup candidate.

Rules it will not break:

- A run with no terminal state is **resumable**, and is never collected unless `--force` names it explicitly.
- A worktree whose run directory has already gone is an orphan: prune the directory and run `git worktree prune` on the parent repo, so the parent does not accumulate entries pointing at nothing.
- Deleting a worktree never deletes the branch on the remote. The PR is evidence; the checkout is scratch.
- `--json` reports what was freed, per run, so the daily disk question has a number instead of a feeling.

### 6.4 The environment a run needs

A repo declares what it takes to build and test it, and a run on a machine that cannot execute that declaration produces nothing worth reviewing. So the environment is an input, handled like the worktree: named, recorded, and prepared before the run rather than improvised inside it.

- **Declared, not discovered.** The declaration is what the repo already says — `go.mod`'s go directive, `package.json`'s engines, package manager and lockfile, `rust-toolchain.toml`, `.python-version` or `requires-python`, the base image a `Dockerfile` names — plus an optional `agent-runner.env.toml` for what those cannot express: system packages, services, environment variables. Each entry keeps the file that justified it, so `env-init` can say why it wants to install something. A repo that declares nothing has an empty declaration and a digest, not an error: there is nothing to verify, so `env-check` is satisfied and `env-init` plans nothing. The gate is then the only thing that can tell that host from a provisioned one.
- **A digest names it.** The digest is a hash over the normalised declaration and the bytes of the declaring files: an edit that changes nothing that matters leaves it alone, a version bump changes it. The decision is a comparison, not a judgement — `env-check` asks whether the directory at that digest exists and is intact, and a run records the digest it was bound to.
- **It lives in the state dir, and nothing enters the host.** `${XDG_STATE_HOME:-~/.local/state}/agent-runner/envs/<digest>/` holds `env.json` (the declaration, the resolved tools and versions, sizes, when it was built), `bin/`, `lib/` and `share/`. Every tool in it was installed inside that directory — `PATH` and each ecosystem's install prefix point into it, and `env-init` is what put them there — never into `$HOME`, `/usr/local` or the system package manager. It is built in a temporary sibling and renamed on success, as the run record is, so a failed init leaves no half-environment that `env-check` could accept.
- **Read-only in the run.** `--sandbox container` binds `envs/<digest>` read-only at `/opt/env` and puts `/opt/env/bin` first on `PATH`. `--sandbox worktree` gets the same `PATH` prefix and no bind; what holds there is the no-write bits `env-init` left on the directory, and the record says so, which is the honesty §6.1 already applies to the diff. Either way the run's `TMPDIR` stays inside the run, and so does every writable cache a tool insists on — `GOCACHE`, cargo's cache, the npm cache — because Go, cargo and npx all write at build time, and a shared tree they can write to is a tree one run can poison for the next. A cold cache per run is the price of that. `run.json` gains `env_digest`, so a run names its environment the way it names its branch and base sha.
- **Shared without contention.** Being read-only — a bind in container mode, no-write bits outside it — one environment serves every run on its digest: no per-run copy, no warm-up step. A reader takes a lock in the environment and `gc` never removes one it cannot take (§6.3), so the only writer and the only deleter cannot meet a run. An environment that is missing or torn when a run starts is exit `6` before any work, never a run that discovers half a toolchain at the gate.
- **Provisioning is a command, and it is recorded.** `env-init` is the only thing that writes under `envs/`, and what it did is an artifact inside the environment: `env.json`, plus `env.ndjson` with one line per step. A run reads that record; it never writes it. The trust it asks for, downloading a toolchain and choosing its source, is why it is a command someone ran with a plan to read first, and not a side effect of a pipeline.

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

## 8. Pipelines: `resolve` and `amend`

`resolve` answers the pull request. `amend` answers the issue, when a maintainer has said something on it that an open PR does not yet reflect. They are different pipelines because the input is different. A review comment names the change it wants; an issue comment can change what the change should be.

### `amend`

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

A resolve loop on a timer will act when there is nothing to do, because a schedule looks like an instruction. Each guard below is enforced in code and has a test.

- **The world is the input, not the clock.** Before acting, the run digests the PR's observable state: head and base sha, draft and mergeable flags, every check by name and conclusion, every review by id and state, every comment by id and author, every unresolved thread id. If the digest matches the previous run's, the run exits `0` with `unchanged`. Two people asleep produce no digest change, so the loop does nothing for as long as they sleep.
- **Our own events are not state changes.** Comments, reviews and pushes authored by the authenticated account are excluded from the digest. Without this the loop reads its own output as news and answers itself forever.
- **Every action spends a trigger.** An outbound write must cite the event id it answers, and an id can be spent once, recorded in the ledger. A retry, a replay or a later scheduled run finds the trigger spent and does nothing.
- **Response only.** The action set is closed: fix a `CHANGES_REQUESTED` review, answer a new question once, repair a check that went red on our head, rebase a conflict. Nothing in it is "ask for attention". Requesting review or merge, bumping, `@`-mentioning, "any update?", close/reopen, and opening a replacement PR for a rejected branch are absent from the code, rather than discouraged in a prompt.
- **Approval ends the run.** An approved, green, conflict-free PR is finished. The worker does not seek the merge, because merging belongs to a person (§12). Finding nothing to do also ends a run: exit `0`, say nothing.
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
- A gate the shell could not run is not a failing gate. When the shell reports 127 or 126, the run refuses (§11, exit `3`) and records the shell's own message; a test that ran and failed stays exit `2`. A missing program is the machine's answer; a red test is the patch's.

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
- the gate runs, but the shell could not run a program it calls — 127 for absent, 126 for not executable — and the record keeps what the shell said;
- the gate needs a host outside the network allowlist to pass;
- the diff would touch a path outside the write scope;
- a command tries to write outside the run directory, or reach a host outside the network allowlist;
- `resolve` is pointed at a PR we do not own;
- a thread's PR was closed without merging, and the next step would be a replacement PR.
- the route implies `--force` and the pipeline's preconditions do not hold.

A missing *environment* is not one of these. It is exit `6` at the front door (§6.4), before the run starts: the answer is `env-init`, not a different subject, and §13 keeps the two codes apart. A test that can only pass by reaching the open internet is the allowlist bullet, not a red test.

A refusal is a successful run. Stopping costs a maintainer nothing; a confident wrong PR costs them a review.

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
- **A command decides how; the caller decides whether.** Which of the three runs is the caller's business. Within a run, whether there is work at all is a digest comparison, not a judgement, and a command with nothing new exits before it spends a model call.
- **The ledger is the state.** Every run reads the previous ledger and writes one. Nothing is remembered in a process, a scratchpad or a prompt, so a run can be replayed and two runs cannot disagree about what happened.

## 13. Exit codes

| Code | Meaning |
|---|---|
| 0 | Done: PR opened, or nothing left actionable |
| 2 | Gate failed |
| 3 | Refused (see §11) |
| 4 | Needs a human: ambiguous scope, a question only the maintainer can answer |
| 5 | Budget exhausted |
| 6 | Environment not ready: run `env-init`, then retry (§6.4) |

`6` is not `3`. A caller that sees `3` changes the subject; a caller that sees `6` provisions the environment and calls the same command again with the same arguments. Two codes whose next step differs that much cannot be one code.

A command that ends at `6` writes no run: there is nothing to record yet, and `--json` carries the repo, the digest and what is missing (§4) instead of a run id.

## 14. Non-goals

- Autonomy across many issues at once. Serial first.
- Merging, signing CLAs, or posting to maintainers on its own.
- Replacing review. It produces something worth reviewing.
- A UI. The report is Markdown because Markdown is the interface.
- An agent protocol. This is a batch process with a paper trail; interactive transports are a separate concern at one seam, stage 4's implementer, and they can be added there without changing the shape.
- Owning a clock. Scheduling is the caller's problem by design.
- Watchers, daemons, pollers, queues, leases or work-in-progress limits. Those are the meta framework's job: it decides which thread gets a command and when. A `watch` command inside this binary would be the first step of turning a tool into a service, and the moment that happens the pipelines stop being testable in isolation.
- Deciding which of the three pipelines to run. Routing on "what changed" looks helpful and duplicates a decision the caller has to make anyway.
- Deleting anything on its own initiative, beyond the one exception in §6.3: a run that finished done removes the worktree it wrote in, as its last recorded step. Everything else that removes files is `gc`, a command with a dry run.

## 14.1 Where the meta framework goes

Scheduling a `resolve` every ten minutes, holding a queue of issues, enforcing one PR in flight per repo, deciding that a thread is worth another run — all of it lives above this binary and talks to it through exit codes, `--json` and the run records. Nothing in that layer needs to be in this repo, and nothing in this repo needs to know it exists.

The contract between the two is deliberately small:

- a pipeline command takes a URL and a repo path, the provisioning commands take a repo path, and both exit with a code from §13;
- `--json` on stdout describes what happened, in a shape that does not change between runs;
- a run record on disk describes how it happened, and can be replayed.

If a scheduler needs more than that, the scheduler is doing something the pipeline should be doing.

## 15. What exists, what is missing

| Piece | State |
|---|---|
| Step-capped loop, transcript, tool registry | **done** — `internal/agent`, `internal/tools` (Go) |
| argv-only command runner, flag allowlist, workdir confinement | **done** — Go port, race-tested, symlink escapes covered |
| `build`/`resolve`/`amend` commands, thread reading, `--dry-run` | **partial** — `build`/`resolve` flags parse and print a plan |
| Run directory, ndjson record, `REPORT.md` | **missing** |
| Event ledger, and idempotence: digest, own-event exclusion, trigger spend | **missing** |
| `amend` pipeline | **missing** |
| `replay` from a recorded run | **missing** |
| Resume and stage idempotency | **missing** |
| Worktree per run (filesystem scope) | **missing** |
| Machine isolation: no writes outside, network allowlist, process-group timeouts, container default | **missing** |
| `gc` with dry run, two tiers, orphan pruning | **missing** |
| Environment as an input: `env-check`, `env-init`, digest and state layout | **missing** |
| Real LLM client | **missing** — the loop runs on a deterministic mock |
| Gate runner | **missing** |
| `gh` and git operations | **missing** |
| Goldfish pass | **missing in-process** — proven externally via `copilot` |

## 16. Milestones

Ordered so the property that matters is proven first. Determinism is not a milestone; it is the acceptance test every milestone after M0 must keep passing.

- **M0 — the ledger.** Append-only events, the world digest, trigger spend, and `replay`. Proven by three fixtures taken from real failures: a PR whose newest event is our own comment with no human reply, a PR we closed, and a PR whose review we already answered. Each must plan zero actions and exit `0`.
- **M0.5 — the cage.** `--sandbox` with container detection, network allowlist, process-group timeouts, and the self-audit that lists writes outside the run directory. Proven by a deliberate escape attempt that gets refused.
- **M0.7 — safe to run twice.** `resolve` over one open PR: digest, own-event exclusion, trigger spend. Proven by running it twice against an untouched PR and getting `unchanged` with no writes the second time, and by pointing it at a PR whose newest event is our own comment.
- **M1 — resolve on a live PR.** One PR, one review round, guards enforced, and a silent world ending in `unchanged` with no notification. This is the first milestone where the program does something a prompt cannot.
- **M2 — one repo, one issue.** Reproduce, implement, gates, commit on a worktree branch. No PR.
- **M3 — PR back.** Push, `gh pr create`, refusal paths exercised, one PR in flight per repo enforced.
- **M4 — build end to end.** Stages 1 to 8 with a real client, `REPORT.md` rendered, resume proven by killing a run mid-stage.
- **M5 — dogfood.** The worker takes the next three issues on my repos, and I review what it produces.

## 17. Failure modes worth naming

Three things went wrong in practice before this program existed, and each has a guard above. All three came from a loop on its own timer. Two cannot happen here now that the timer lives outside, and the third is why the digest exists.

- **The loop answers its own comment.** A comment it wrote looks like activity, so the next scheduled run replies again, and the one after that. Fix: our events are excluded from the digest, and a trigger can be spent once.
- **Nobody is awake, so it applies pressure.** A maintainer asleep at 03:00 is not a stalled PR. A loop that reads silence as a problem to solve will ask for a merge, then ask again. Fix: requests do not exist in the action set; an approved PR is a finished run. The timer being someone else's makes this rarer, not impossible — a careless cron is still a cron.
- **A wall closes and it tries the door.** Close/reopen three times on one PR, each attempt a notification to someone who did not ask. Fix: those verbs are absent from the code, and a repeated failure from the same world is a `blocked` exit.

## 18. How we know it works

- **Unattended-to-merged rate.** PRs opened by the worker that a maintainer merges without a second round of review.
- **Gate-first accuracy.** Share of runs where the repo's own gate agreed with the worker's claim on the first try.
- **Refusals that were right.** Of the runs that stopped, how many a human agreed should have stopped.
- **Resume honesty.** Runs interrupted on purpose that finished correctly when resumed, without repeating a side effect.
- **Cost per merged PR**, counted from the run records rather than from memory.
- **Replay fidelity.** Runs whose recomputed plan matches the recorded one, across the fixture set. It belongs at 100%; anything lower means the loop responded to something the record does not contain.
- **Wordless runs** — invocations that ended `unchanged` and notified nobody. A high number means the caller is polling more than the world is changing, which is exactly the state a scheduler should reach overnight. Whoever is calling measures it from the exit codes; this binary only reports them.
