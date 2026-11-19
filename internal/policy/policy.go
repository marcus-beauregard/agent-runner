// Package policy declares the command allowlist that keeps run_command safe
// by default. Extend it deliberately.
package policy

// Command describes what a single allowlisted binary may do: the exact flags
// it accepts, and, for a multiplexer like git, the subcommands and their flags.
type Command struct {
	Flags       []string
	Subcommands map[string][]string
}

// Commands is the allowlist. Every entry declares the flags it accepts, which
// keeps an allowlisted binary from becoming a launcher: `node -e`,
// `python3 -c` and `git -c core.pager=...` all execute arbitrary code without
// ever needing a shell, so they are simply not allowed.
var Commands = map[string]Command{
	"ls":  {Flags: []string{"-1", "-a", "-A", "-l", "-h", "-la", "-al", "-lah"}},
	"cat": {},
	"wc":  {Flags: []string{"-l", "-w", "-c", "-m"}},
	"git": {Subcommands: map[string][]string{
		"status": {"--short", "--porcelain", "-s"},
		"log":    {"--oneline", "--stat", "--decorate", "-n", "--max-count"},
		"diff":   {"--stat", "--name-only", "--cached"},
		"show":   {"--stat", "--oneline"},
	}},
}
