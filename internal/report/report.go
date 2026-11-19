// Package report produces the provenance plan for the worker's dry-run and
// maps failures to process exit codes.
package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Sentinel errors map to exit codes via ExitCode.
var (
	ErrGateFailed = errors.New("gate failed")
	ErrRefused    = errors.New("refused")
	ErrNeedsHuman = errors.New("needs a human")
)

// ExitCode maps a sentinel error to the process exit code it stands for:
// 2 gate failed, 3 refused, 4 needs a human. Anything unexpected is 1.
func ExitCode(err error) int {
	switch {
	case errors.Is(err, ErrGateFailed):
		return 2
	case errors.Is(err, ErrRefused):
		return 3
	case errors.Is(err, ErrNeedsHuman):
		return 4
	default:
		return 1
	}
}

// Issue is the slice of an issue the worker needs to decide whether to proceed.
type Issue struct {
	Title     string
	State     string
	Assignees []string
}

// Plan is the M0 deliverable: what would be done, and nothing that was done.
type Plan struct {
	Ref   string
	State string
	Scope string
	Files []string
	Gate  string
}

// String renders the plan as the reviewable, deterministic summary printed by
// `agent-runner work --dry-run`.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue: %s\n", p.Ref)
	fmt.Fprintf(&b, "State: %s\n", orUnknown(p.State))
	fmt.Fprintf(&b, "Scope: %s\n", orUnknown(p.Scope))
	if len(p.Files) == 0 {
		fmt.Fprintf(&b, "Files: (none determined)\n")
	} else {
		fmt.Fprintf(&b, "Files: %s\n", strings.Join(p.Files, ", "))
	}
	fmt.Fprintf(&b, "Gate: %s\n", orNone(p.Gate))
	return b.String()
}

// Work runs the M0 dry-run: read the issue with gh if it can, then produce a
// plan. It writes nothing and creates no branch. A refusal — gh unavailable,
// an unreadable issue, or one that is closed or already claimed — is returned
// as an error wrapping ErrRefused, alongside the best plan we can still print.
func Work(ctx context.Context, ref, repo, gate string) (Plan, error) {
	plan := Plan{Ref: ref, Gate: gate}
	gh, err := exec.LookPath("gh")
	if err != nil {
		plan.Scope = "(gh CLI unavailable)"
		return plan, fmt.Errorf("%w: gh CLI not available: %v", ErrRefused, err)
	}
	issue, err := fetchIssue(ctx, gh, ref)
	if err != nil {
		plan.Scope = "(issue could not be read)"
		return plan, fmt.Errorf("%w: cannot read issue: %v", ErrRefused, err)
	}
	plan.State = issue.State
	plan.Scope = fmt.Sprintf("Resolve issue %s: %s", ref, issue.Title)
	if !strings.EqualFold(issue.State, "OPEN") {
		return plan, fmt.Errorf("%w: issue is %s", ErrRefused, strings.ToLower(issue.State))
	}
	if len(issue.Assignees) > 0 {
		return plan, fmt.Errorf("%w: issue already claimed by %s", ErrRefused, strings.Join(issue.Assignees, ", "))
	}
	return plan, nil
}

type ghAssignee struct {
	Login string `json:"login"`
}

// fetchIssue reads issue metadata through the gh CLI as an argv array — never
// a shell — and returns only the fields the worker needs to decide.
func fetchIssue(ctx context.Context, gh, ref string) (Issue, error) {
	out, err := exec.CommandContext(ctx, gh, "issue", "view", ref,
		"--json", "number,title,state,assignees").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return Issue{}, fmt.Errorf("gh issue view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return Issue{}, err
	}
	var raw struct {
		Number    int          `json:"number"`
		Title     string       `json:"title"`
		State     string       `json:"state"`
		Assignees []ghAssignee `json:"assignees"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Issue{}, fmt.Errorf("parse gh output: %w", err)
	}
	assignees := make([]string, 0, len(raw.Assignees))
	for _, a := range raw.Assignees {
		assignees = append(assignees, a.Login)
	}
	return Issue{Title: raw.Title, State: raw.State, Assignees: assignees}, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
