# agent-runner

[![CI](https://github.com/marcus-beauregard/agent-runner/actions/workflows/ci.yml/badge.svg)](https://github.com/marcus-beauregard/agent-runner/actions/workflows/ci.yml)
[![License: GPL-2.0-or-later](https://img.shields.io/badge/license-GPL--2.0--or--later-blue.svg)](LICENSE)

An agent runtime, and the worker built on it: hand it an issue or a pull request, get back something a maintainer can review, with a run record a stranger can pick up.

**Status: early.** The loop, the tool registry and the hardened command runner work. The worker is specified and mostly not built; `dev` and `resolve` parse their flags and print a plan. The planner is a deterministic mock, so no model is called yet. Nothing here is ready to point at somebody else's repository.

Where it is going is written down in [PRD.md](PRD.md), which is the source of truth for behaviour. This README is the front door; that file is the design.

## Layout

The canonical Go layout, and nothing invented:

```
cmd/agent-runner/     the binary
internal/agent/       the loop: plan, act, observe, reflect
internal/tools/       tool registry and the argv-only command runner
internal/policy/      what a tool is allowed to do
internal/report/      plans and run reports
```

Everything lives under `internal/` on purpose. There is no public API to keep compatible while the design is still moving.

## Build and test

```bash
go build -o agent-runner ./cmd/agent-runner
./agent-runner run "List the files in this project"
go test -race ./...
```

The default entry point runs offline against the mock planner, so a run is reproducible without a key.

Gates, and CI runs all three:

```bash
test -z "$(gofmt -l .)" && go vet ./... && go test -race ./...
```

## Contributing

Humans and automated agents are both welcome. No CLA; every commit carries a DCO sign-off (`git commit -s`).

Two rules matter more than the rest: **keep changes small**, and **open an issue before writing code** unless the change is a one-liner. [CONTRIBUTING.md](CONTRIBUTING.md) has the full set, and [AGENTS.md](AGENTS.md) is the same contract written for coding agents.

## Licence

GPL-2.0-or-later. See [LICENSE](LICENSE).
