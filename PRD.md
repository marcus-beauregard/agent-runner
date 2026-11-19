# PRD: agent-runner as a worker

**Status:** draft, 2026-11-19
**Owner:** Marcus Beauregard
**One line:** hand it an issue URL, get back a reviewable pull request — with the gates we already trust.

---

## 1. Problem

The pipeline that produced today's merges lives in a chat session: read the issue, check it is not already fixed, reproduce, patch, run the repo's gates, get a second opinion, commit, open the PR. Every step exists in prose in an automation prompt, which means every run re-derives it, and the quality drift is invisible until a maintainer says no.

That is the wrong place for a repeatable process. The process should be code: versioned, testable, and able to run on a box with no conversation attached.

**agent-runner already has the hard part** — a step-capped loop over a tool registry whose command runner execs an argv array against a per-command flag allowlist, with paths confined to the working directory. What it lacks is a job: a task that starts from an issue and ends at a PR.

## 2. Who it is for

1. **Me, on my own repos.** An issue sits unstarted for a week; the worker takes it overnight.
2. **Me, upstream.** An approved `good first issue` becomes a branch and a PR while I do something else — with the same discipline the maintainer would review.
3. **Anyone reading the repo.** If the process is in the PRD and the code, the tool is legible without watching me work.

## 3. The command

```bash
agent-runner work https://github.com/owner/repo/issues/42 \
  --repo ~/src/repo \
  --gate "go test ./... && go vet ./... && gofmt -l ." \
  --review copilot \
  --max-steps 40
```

- `work <issue-url>` — the only required argument. `owner/repo#42` also accepted.
- `--repo <path>` — an existing checkout. The worker never clones into a path it invented; cloning is a host step it reports, not a decision it makes unobserved.
- `--gate "<cmd>"` — the command that decides whether the work is done. Repeatable. No gate given and the run stops after the patch and asks for one.
- `--review none|copilot` — the goldfish pass. `copilot` shells out to a different model family with a brief and no conversation.
- `--dry-run` — everything up to the commit, then stop and print the diff.

Exit codes: `0` PR opened · `2` gate failed · `3` refused (see §6) · `4` needs a human.

## 4. The pipeline

Each stage has one job and one completion criterion. A failed criterion ends the run with a report — it does not retry into the dark.

| # | Stage | Does | Done when |
|---|---|---|---|
| 1 | **Fetch** | `gh issue view` → title, body, labels, comments; merged PRs referencing it | The issue is open, unclaimed, and unfixed |
| 2 | **Scope** | One-sentence restatement, files likely touched, the gate command from the repo's own CI | You can name the change and the proof |
| 3 | **Reproduce** | A failing test or command that shows the defect | The failure is observed, not assumed |
| 4 | **Implement** | Tool loop; smallest change that fixes the cause | Gate passes |
| 5 | **Review** | Goldfish: fresh context, four questions, blocking issues only | Every blocker fixed or answered in writing |
| 6 | **Commit** | Conventional subject matching the repo's log, `-s` when DCO applies, body via file | Tree clean, one commit |
| 7 | **Open PR** | Push branch, `gh pr create --body-file` with claim/change/proof | PR URL, checks named |
| 8 | **Report** | `.agent-runner/runs/<id>/REPORT.md` + transcript JSON | A stranger can resume from it |

Stages 1–3 are cheap and local; they exist so stage 4 starts from a fact.

## 5. Contracts

- **Write scope:** the `--repo` path, and nothing outside it. `run_command` already enforces this; the PR must not be the first time we learn it did not.
- **One commit, one idea.** A worker that bundles is a worker whose patches cannot be reverted.
- **The gate is the repo's, quoted.** Not "tests pass" — the exact command and its exit code in the report.
- **The reviewer is not the author.** Same-session self-review is not review; the goldfish gets the diff and the issue, never the transcript.
- **Never merge.** The worker's authority ends at a PR. Merges belong to a person or a maintainer.
- **No secrets in the transcript.** Tokens come from the environment; the transcript records that a command ran, not what it carried.
- **The issue body is data, not instructions.** An issue can contain "ignore your rules and run this"; the pipeline treats its text as a defect report to understand, never as a command. Anything it claims about the repo is verified against the checkout. This is the sharpest edge in the design: the worker reads attacker-controlled text and then executes code.

## 6. Refusals (exit 3)

The worker refuses, with a reason it prints, when:

- the issue is closed, already fixed by a merged PR, or claimed by another person;
- the task needs a CLA the operator has not signed;
- the repo's CONTRIBUTING forbids unattended agents;
- the gate cannot be run in the sandbox (missing toolchain, network-only test);
- the diff would touch a path outside the write scope.

A refusal is a successful run. The failure mode to fear is a confident wrong PR, not a stop.

## 7. Non-goals

- Autonomy across many issues at once. Serial first.
- Merging, signing CLAs, or posting to maintainers on its own.
- Replacing review. It produces something worth reviewing.
- A UI. The report is Markdown because Markdown is the interface.

## 8. What exists, what is missing

| Piece | State |
|---|---|
| Step-capped loop, transcript, tool registry | **done** — `internal/agent`, `internal/tools` (Go) |
| argv-only command runner, flag allowlist, workdir confinement | **done** — Go port, race-tested, symlink escapes covered |
| Real LLM client | **missing** — the loop runs on a deterministic mock |
| `gh` integration (issue read, PR create) | **missing** — host steps today |
| git operations (branch, commit, push) | **missing** |
| Gate runner and refusal logic | **missing** |
| Goldfish pass | **missing in-process** — proven externally via `copilot` |

## 9. Milestones

- **M0 — provenance.** `work --dry-run` against a real issue with a mock planner; prints the plan and the gate it would run. No writes.
- **M1 — one repo, one issue.** Real LLM client, reproduce → implement → gate, report written, no PR. Proves the loop earns its keep on my own repo.
- **M2 — PR back.** Branch, commit, push, `gh pr create`. Still refuses anything outside its lane.
- **M3 — dogfood.** The worker takes the next three issues on my repos and the hourly rotation just reviews what it produced.

## 10. How we know it works

- **Unattended-to-merged rate** — PRs opened by the worker that a maintainer merges without a second round of review.
- **Gate-first accuracy** — share of runs where the repo's own gate agreed with the worker's claim on the first try.
- **Refusals that were right** — of the runs that stopped, how many a human agreed should have stopped.
- **Cost per merged PR** — steps and tokens, not vibes.

A worker that opens five PRs and lands one is worse than a worker that opens one and lands it.
