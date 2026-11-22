package refuse_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// normalize makes a sentence from the PRD and a sentence from the mapping table
// comparable: backticks are markup, a bullet that wraps is still one line, and
// the punctuation §11 ends each cause with is not part of the cause.
func normalize(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimRight(strings.Join(strings.Fields(s), " "), ";.")
}

// contains reports whether xs holds want.
func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// prdRefusalCauses reads PRD §11 and returns each bullet, normalized. The test
// reads the source of truth rather than a copy of it: a bullet added there and
// not named here is exactly the gap this test exists to catch.
func prdRefusalCauses(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("../../PRD.md")
	if err != nil {
		t.Fatalf("read PRD.md: %v", err)
	}
	text := string(b)
	start := strings.Index(text, "## 11. Refusals")
	if start < 0 {
		t.Fatal("PRD.md has no §11")
	}
	text = text[start:]
	if end := strings.Index(text, "## 12."); end >= 0 {
		text = text[:end]
	}
	var causes []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		causes = append(causes, normalize(line[2:]))
	}
	if len(causes) == 0 {
		t.Fatal("§11 lists no causes")
	}
	return causes
}

// TestVocabularyCoversEveryCauseInThePRD is the ticket's "no gaps" claim: every
// cause §11 names maps to at least one code, and every code the mapping uses is
// in the vocabulary — so a cause cannot be added to the PRD without this test
// failing, and a code cannot be invented outside the list.
func TestVocabularyCoversEveryCauseInThePRD(t *testing.T) {
	mapping := []struct {
		cause string
		codes []refuse.Code
	}{
		{
			cause: "the issue is closed, already fixed by a merged PR, or claimed by another person",
			codes: []refuse.Code{refuse.IssueClosed, refuse.AlreadyFixed, refuse.ClaimedByOther},
		},
		{
			cause: "the task needs a CLA the operator has not signed",
			codes: []refuse.Code{refuse.CLAUnsigned},
		},
		{
			cause: "the repo's CONTRIBUTING forbids unattended agents",
			codes: []refuse.Code{refuse.AgentsForbidden},
		},
		{
			cause: "a declared dependency resolves outside the registries the runner fetches — a private module, a git or path dependency, a registry override",
			codes: []refuse.Code{refuse.DependencyUnavailable},
		},
		{
			cause: "the gate runs, but the shell could not run a program it calls — 127 for absent, 126 for not executable — and the record keeps what the shell said",
			codes: []refuse.Code{refuse.GateUnavailable},
		},
		{
			cause: "the diff would touch a path outside the write scope",
			codes: []refuse.Code{refuse.OutsideWriteScope},
		},
		{
			cause: "a command tries to write outside the run directory, or reach a host outside the network allowlist",
			codes: []refuse.Code{refuse.OutsideRunDir, refuse.HostNotAllowed},
		},
		{
			cause: "resolve is pointed at a PR we do not own",
			codes: []refuse.Code{refuse.NotOurPR},
		},
		{
			cause: "a thread's PR was closed without merging, and the next step would be a replacement PR",
			codes: []refuse.Code{refuse.PRClosedUnmerged},
		},
		{
			cause: "the route implies --force and the pipeline's preconditions do not hold",
			codes: []refuse.Code{refuse.ForcePreconditions},
		},
	}

	causes := prdRefusalCauses(t)

	for i, row := range mapping {
		if !contains(causes, normalize(row.cause)) {
			t.Errorf("mapping entry %d (%q) does not match a bullet in §11", i, row.cause)
		}
		for _, code := range row.codes {
			if !refuse.Valid(code) {
				t.Errorf("code %q is used by the mapping but is not in the vocabulary", code)
			}
		}
	}

	for _, cause := range causes {
		covered := false
		for _, row := range mapping {
			if cause == normalize(row.cause) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("§11 cause %q has no code in this mapping", cause)
		}
	}
}

// TestRefusalIsAnExitThree is the mapping every stage relies on: a typed refusal
// is a refusal to the sentinel the pipeline maps, and it is exit 3 whether the
// caller named this package's sentinel or report's — they are the same value.
func TestRefusalIsAnExitThree(t *testing.T) {
	ref := refuse.New(refuse.HostNotAllowed, "fetch", "example.com is not on the allowlist")
	var got *refuse.Refusal
	if !errors.As(error(ref), &got) {
		t.Fatal("a Refusal does not travel as its own type")
	}
	if !errors.Is(ref, refuse.ErrRefused) {
		t.Error("a Refusal is not the refusal sentinel")
	}
	if !errors.Is(ref, report.ErrRefused) {
		t.Error("report.ErrRefused is not the same sentinel")
	}
	if code := report.ExitCode(ref); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

// TestErrorNamesTheCodeAndTheReason is what a person reading stderr gets: the
// code, the stage when there is one, and the sentence — never only "refused".
func TestErrorNamesTheCodeAndTheReason(t *testing.T) {
	tests := []struct {
		name string
		ref  *refuse.Refusal
		want string
	}{
		{
			name: "with a stage",
			ref:  refuse.New(refuse.OutsideRunDir, "run", "the command wrote /tmp/x"),
			want: "refused: outside-run-dir at run: the command wrote /tmp/x",
		},
		{
			name: "without a stage",
			ref:  refuse.New(refuse.IssueClosed, "", "issue is closed"),
			want: "refused: issue-closed: issue is closed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodesIsTheVocabulary: every code the package exports is listed, the list
// is not empty, and Valid says no to anything that is not in it — including the
// empty string, which is what a zero Refusal carries.
func TestCodesIsTheVocabulary(t *testing.T) {
	codes := refuse.Codes()
	if len(codes) == 0 {
		t.Fatal("Codes() is empty")
	}
	for _, code := range codes {
		if !refuse.Valid(code) {
			t.Errorf("Codes() lists %q but Valid says no", code)
		}
	}
	for _, bad := range []refuse.Code{"", "made-up", "issue_closed"} {
		if refuse.Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
	}
	// The copy is the caller's: appending to it must not extend the vocabulary.
	codes[0] = "mutation"
	if !refuse.Valid(refuse.IssueClosed) {
		t.Error("Codes() shares its backing array with the vocabulary")
	}
}
