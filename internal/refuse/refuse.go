// Package refuse names why a run stopped. A refusal is a successful run (PRD
// §11), so it is a value the record can hold rather than an error string a
// reader has to parse: one code from a closed vocabulary, one sentence a person
// can read, the stage it happened in, and the evidence behind it.
//
// The vocabulary is closed so that a new reason to stop is a deliberate edit
// here — something a reviewer sees — instead of a fresh string in whichever
// stage happened to need one.
package refuse

import (
	"errors"
	"fmt"
)

// ErrRefused is the sentinel every Refusal unwraps to. report.ErrRefused is the
// same value, so a caller that checks either name means the same thing, and the
// pipeline maps it to exit 3.
var ErrRefused = errors.New("refused")

// Code is a reason a run refuses, from the closed vocabulary below.
type Code string

// The vocabulary. The first block is PRD §11's list, one code per cause it
// names; the second block holds the two causes the binary produces today that
// §11 implies without naming.
const (
	IssueClosed        Code = "issue-closed"        // the issue is closed
	AlreadyFixed       Code = "already-fixed"       // a merged PR already fixed it
	ClaimedByOther     Code = "claimed-by-other"    // someone else is on it
	CLAUnsigned        Code = "cla-unsigned"        // the task needs a CLA nobody signed
	AgentsForbidden    Code = "agents-forbidden"    // CONTRIBUTING forbids unattended agents
	GateUnavailable    Code = "gate-unavailable"    // a program the gate called could not run
	OutsideWriteScope  Code = "outside-write-scope" // the diff would leave the write scope
	OutsideRunDir      Code = "outside-run-dir"     // a command wrote outside the run directory
	HostNotAllowed     Code = "host-not-allowed"    // a request reached an off-allowlist host
	NotOurPR           Code = "not-our-pr"          // resolve was pointed at a PR we do not own
	PRClosedUnmerged   Code = "pr-closed-unmerged"  // the thread's PR closed without merging
	ForcePreconditions Code = "force-preconditions" // --force implied, preconditions unmet

	// ToolUnavailable is the cause §11 implies but does not name: the plan needs
	// a tool this host does not have, or a gh that cannot answer. A missing
	// environment is not this one — that is exit 6 before the run starts (§6.4).
	ToolUnavailable Code = "tool-unavailable"
	// SandboxUnavailable is a container sandbox asked for on a host that cannot
	// build one. It is a refusal, never a silent downgrade (PRD §6.2).
	SandboxUnavailable Code = "sandbox-unavailable"
)

// all is the vocabulary in one place, so Codes and Valid cannot disagree about
// what a code is.
var all = []Code{
	IssueClosed,
	AlreadyFixed,
	ClaimedByOther,
	CLAUnsigned,
	AgentsForbidden,
	GateUnavailable,
	OutsideWriteScope,
	OutsideRunDir,
	HostNotAllowed,
	NotOurPR,
	PRClosedUnmerged,
	ForcePreconditions,
	ToolUnavailable,
	SandboxUnavailable,
}

// Codes returns the vocabulary in declaration order. A caller that prints it or
// checks it for coverage gets a stable order rather than a map's.
func Codes() []Code { return append([]Code(nil), all...) }

// Valid reports whether c is in the vocabulary.
func Valid(c Code) bool {
	for _, k := range all {
		if k == c {
			return true
		}
	}
	return false
}

// Refusal is one refusal: what the run stopped for, where, and the evidence a
// reader can check it against. Evidence is a small map rather than a string
// because the report prints it line by line, and a code whose evidence cannot
// be read is a code nobody can trust.
type Refusal struct {
	Code     Code              // one entry from the vocabulary
	Where    string            // the stage that refused, when it has a name
	Reason   string            // one sentence, printed as written
	Evidence map[string]string // what the reader can check the reason against
}

// New builds a refusal. The code is not validated here: it is a typed constant,
// and the place that persists it — run.Refuse — is where an unknown code has to
// fail, because that is where it would otherwise become a record.
func New(code Code, where, reason string) *Refusal {
	return &Refusal{Code: code, Where: where, Reason: reason}
}

// Error prints the code and the reason in one line, so a person reading stderr
// sees what was refused and why without opening the record.
func (r *Refusal) Error() string {
	if r.Where == "" {
		return fmt.Sprintf("refused: %s: %s", r.Code, r.Reason)
	}
	return fmt.Sprintf("refused: %s at %s: %s", r.Code, r.Where, r.Reason)
}

// Unwrap is what makes a Refusal exit 3: however deep it travelled, every
// mapper sees ErrRefused.
func (r *Refusal) Unwrap() error { return ErrRefused }
