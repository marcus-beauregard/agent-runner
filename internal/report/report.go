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

	"github.com/marcus-beauregard/agent-runner/internal/gate"
	"github.com/marcus-beauregard/agent-runner/internal/refuse"
)

// Sentinel errors map to exit codes via ExitCode.
var (
	ErrGateFailed = errors.New("gate failed")
	// ErrRefused is refuse.ErrRefused under this package's name: one sentinel,
	// so a caller that checks either means the same thing, and a typed refusal
	// maps to exit 3 wherever it was raised.
	ErrRefused    = refuse.ErrRefused
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
// an unreadable issue, one that is closed, already fixed by a merged pull
// request, or already claimed, or a gate whose programs this host does not
// have — is returned as a typed *refuse.Refusal, alongside the best plan we can
// still print, so the caller can record why the run stopped instead of only
// that it did.
func Work(ctx context.Context, ref, repo, gateCmd string) (Plan, error) {
	plan := Plan{Ref: ref, Gate: gateCmd}
	gh, err := exec.LookPath("gh")
	if err != nil {
		plan.Scope = "(gh CLI unavailable)"
		refusal := refuse.New(refuse.ToolUnavailable, "plan", "gh CLI is not available")
		refusal.Evidence = map[string]string{"error": err.Error()}
		return plan, refusal
	}
	issue, merged, err := fetchIssue(ctx, gh, ref)
	if err != nil {
		plan.Scope = "(issue could not be read)"
		refusal := refuse.New(refuse.ToolUnavailable, "plan", "the issue could not be read")
		refusal.Evidence = map[string]string{"gh": err.Error()}
		return plan, refusal
	}
	plan.State = issue.State
	plan.Scope = fmt.Sprintf("Resolve issue %s: %s", ref, issue.Title)
	if !strings.EqualFold(issue.State, "OPEN") {
		refusal := refuse.New(refuse.IssueClosed, "plan",
			fmt.Sprintf("issue is %s", strings.ToLower(issue.State)))
		refusal.Evidence = map[string]string{"state": issue.State}
		return plan, refusal
	}
	if len(issue.Assignees) > 0 {
		refusal := refuse.New(refuse.ClaimedByOther, "plan",
			fmt.Sprintf("issue is claimed by %s", strings.Join(issue.Assignees, ", ")))
		refusal.Evidence = map[string]string{"assignees": strings.Join(issue.Assignees, ", ")}
		return plan, refusal
	}
	if merged != nil {
		refusal := refuse.New(refuse.AlreadyFixed, "plan",
			fmt.Sprintf("issue was already fixed by merged PR #%d", merged.Number))
		refusal.Evidence = map[string]string{"pull_request": merged.URL, "state": "MERGED"}
		return plan, refusal
	}
	// The gate is checked last: when the issue is closed, claimed or already
	// fixed, that is the answer — planning it is moot, and a missing toolchain on
	// top of a dead issue is a second problem, not the reason the run stopped.
	if missing := gate.Missing(gateCmd); len(missing) > 0 {
		refusal := refuse.New(refuse.GateUnavailable, "plan", gateUnavailableReason(missing))
		refusal.Evidence = map[string]string{
			"gate":    gateCmd,
			"missing": strings.Join(missing, ", "),
		}
		return plan, refusal
	}
	return plan, nil
}

// gateUnavailableReason names the program the host lacks, because that is what
// the operator can install. Blaming the gate command or the sandbox would leave
// them reading a command line they already copied correctly.
func gateUnavailableReason(missing []string) string {
	if len(missing) == 1 {
		return fmt.Sprintf("gate cannot run: %s is not on PATH", missing[0])
	}
	return fmt.Sprintf("gate cannot run: %s are not on PATH", strings.Join(missing, ", "))
}

// pullRequest is a pull request GitHub links to an issue, once it has merged.
// The issue's own list names the link, not the outcome, so the state is read
// from the pull request: an open one is work in flight, not a fix.
type pullRequest struct {
	Number int
	URL    string
}

type ghAssignee struct {
	Login string `json:"login"`
}

// ghPRRef is one entry of the issue's closedByPullRequestsReferences: a pull
// request that names the issue and would close it, whatever its state.
type ghPRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// fetchIssue reads issue metadata through the gh CLI as an argv array — never
// a shell — and returns only the fields the worker needs to decide, plus the
// first linked pull request that has merged, if any.
func fetchIssue(ctx context.Context, gh, ref string) (Issue, *pullRequest, error) {
	out, err := exec.CommandContext(ctx, gh, "issue", "view", ref,
		"--json", "number,title,state,assignees,closedByPullRequestsReferences").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return Issue{}, nil, fmt.Errorf("gh issue view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return Issue{}, nil, err
	}
	var raw struct {
		Number    int          `json:"number"`
		Title     string       `json:"title"`
		State     string       `json:"state"`
		Assignees []ghAssignee `json:"assignees"`
		ClosedBy  []ghPRRef    `json:"closedByPullRequestsReferences"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Issue{}, nil, fmt.Errorf("parse gh output: %w", err)
	}
	assignees := make([]string, 0, len(raw.Assignees))
	for _, a := range raw.Assignees {
		assignees = append(assignees, a.Login)
	}
	merged, err := mergedOf(ctx, gh, raw.ClosedBy)
	if err != nil {
		return Issue{}, nil, err
	}
	return Issue{Title: raw.Title, State: raw.State, Assignees: assignees}, merged, nil
}

// mergedOf returns the first linked pull request that has already merged. Each
// link is read on its own because the issue's list does not say how it ended,
// and "already fixed" is a claim about the outcome. A link that cannot be read
// is an error rather than a maybe: the run would otherwise proceed on a guess
// about whether the work is already done.
func mergedOf(ctx context.Context, gh string, refs []ghPRRef) (*pullRequest, error) {
	for _, link := range refs {
		out, err := exec.CommandContext(ctx, gh, "pr", "view", link.URL,
			"--json", "number,state,url").Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("gh pr view: %s", strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, err
		}
		var pr struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			URL    string `json:"url"`
		}
		if err := json.Unmarshal(out, &pr); err != nil {
			return nil, fmt.Errorf("parse gh output: %w", err)
		}
		if !strings.EqualFold(pr.State, "MERGED") {
			continue
		}
		number, url := pr.Number, pr.URL
		if number == 0 {
			number = link.Number
		}
		if url == "" {
			url = link.URL
		}
		return &pullRequest{Number: number, URL: url}, nil
	}
	return nil, nil
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
