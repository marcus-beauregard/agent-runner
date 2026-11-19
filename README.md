# agent-runner

[![CI](https://github.com/marcus-beauregard/agent-runner/actions/workflows/ci.yml/badge.svg)](https://github.com/marcus-beauregard/agent-runner/actions/workflows/ci.yml) [![License: GPL-2.0-or-later](https://img.shields.io/badge/license-GPL--2.0--or--later-blue.svg)](LICENSE)

A minimal autonomous agent runtime, and the worker built on it: **hand it an issue, get back a reviewable pull request.**

The runtime is a pluggable LLM driving a plan → act → observe → reflect loop against a small, safe-by-default tool registry (`read_file`, `write_file`, and an allowlisted command runner: argv only, no shell, flags declared per command, paths confined to the working directory).

This is the furthest point of the arc: taking everything learned across the other repos — small surfaces, explicit state, verification — and using it to orchestrate an agent that writes and runs its own code inside a sandbox.

Swap the mock LLM for a real client to go live. Defaults matter: step caps, a command allowlist, and a full transcript on every run.

## Two layers

| Layer | What it is | Where |
|---|---|---|
| **Runtime** | the loop, the tools, the transcript | `internal/agent`, `internal/tools` |
| **Worker** | a job on top of it: issue → patch → gate → review → PR | specified in [PRD.md](PRD.md) |

The runtime works today. The worker is a specification with the hard dependency already met.

## Run it

```bash
go build -o agent-runner ./cmd/agent-runner
./agent-runner run "List the files in this project"
go test -race ./...
```

Or install the binary directly:

```bash
go install github.com/marcus-beauregard/agent-runner/cmd/agent-runner@latest
```

The default entry point runs offline against a deterministic mock planner, so every run is reproducible and reviewable without a key.

The npm package `@marcus.beauregard/agent-runner` 1.0.0 is the historical JavaScript implementation. v2 is Go and ships as a binary; it is not republished to npm.

## The worker, in one command

```bash
agent-runner work https://github.com/owner/repo/issues/42 \
  --repo ~/src/repo \
  --gate "go test ./... && go vet ./... && gofmt -l ." \
  --review copilot
```

Eight stages, each with a completion criterion: fetch the issue and prove it is still open, scope the change, reproduce the failure, implement the smallest fix, run the repo's own gate, pass a fresh-context review, commit once, open the PR with the claim and the proof attached.

It refuses rather than guesses. Exit `3` means it stopped on purpose — issue already fixed, another person claimed it, a CLA it cannot sign, or a gate it cannot run here. A refusal is a successful run.

Full requirements, contracts, refusals and milestones: [PRD.md](PRD.md).

## Safety model

- **argv, not shell.** `run_command` execs an array; no metacharacters, no interpreter flags, `node`/`python3` off the allowlist.
- **Paths stay inside the working directory**, symlinks included.
- **Step cap** on every run, and a transcript of every action.
- **No merge authority.** The worker ends at a pull request.

## Status

| | |
|---|---|
| Runtime loop | working in Go (mock planner) |
| Tool registry + hardened command runner | working, tested |
| Worker: `work --dry-run` | built — prints the plan and the gate, writes nothing |
| Worker: fetch / reproduce / gate / review / commit / PR | specified, not built |

Next milestone: `work --dry-run` against a real issue — it prints the plan and the gate it would run, and writes nothing.

## Contributing

Humans and automated agents are both welcome. There is no CLA — every commit carries a [DCO](https://developercertificate.org/) sign-off (**git commit -s**). Start with a [good first issue](https://github.com/marcus-beauregard/agent-runner/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) or read [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

GPL-2.0-or-later — see [LICENSE](LICENSE). Copyright (C) 2026 Marcus Beauregard.

Versions published before 2026-11-19 remain available under MIT; this licence applies to the current tree and every release from here.
