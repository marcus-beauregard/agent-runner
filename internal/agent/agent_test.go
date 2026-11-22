package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/refuse"
	"github.com/marcus-beauregard/agent-runner/internal/tools"
)

func known(names ...string) func(string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		_, ok := set[name]
		return ok
	}
}

func TestParseAction(t *testing.T) {
	has := known("read_file", "write_file", "run_command")
	cases := []struct {
		text     string
		wantName string
		wantArgs int
		ok       bool
	}{
		{"read_file", "read_file", 0, true},
		{"read_file {}", "read_file", 0, true},
		{`read_file {"path": "x"}`, "read_file", 1, true},
		{"  read_file  ", "read_file", 0, true},
		{"DONE", "", 0, false},
		{"unknown {}", "", 0, false},
		{"read_file {bad", "", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		a, ok := ParseAction(c.text, has)
		if ok != c.ok {
			t.Errorf("ParseAction(%q) ok = %v, want %v", c.text, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if a.Name != c.wantName {
			t.Errorf("ParseAction(%q) name = %q, want %q", c.text, a.Name, c.wantName)
		}
		if len(a.Args) != c.wantArgs {
			t.Errorf("ParseAction(%q) arg count = %d, want %d", c.text, len(a.Args), c.wantArgs)
		}
	}
}

// stubTools is a Toolset that never touches disk; tool calls observe "ok".
type stubTools struct{}

func (stubTools) Names() []string { return []string{"read_file", "write_file", "run_command"} }
func (stubTools) Has(name string) bool {
	switch name {
	case "read_file", "write_file", "run_command":
		return true
	}
	return false
}
func (stubTools) Run(ctx context.Context, name string, args map[string]any) (string, error) {
	return "ok", nil
}

// refusingTools refuses every call the way the registry refuses an escaping
// write: with a typed refusal rather than an error string.
type refusingTools struct{ stubTools }

func (refusingTools) Run(ctx context.Context, name string, args map[string]any) (string, error) {
	return "", refuse.New(refuse.OutsideWriteScope, "tool", "write_file would write outside the run directory")
}

// failingTools fails every call the way a command that exits non-zero does:
// with an ordinary error and no refusal behind it.
type failingTools struct{ stubTools }

func (failingTools) Run(ctx context.Context, name string, args map[string]any) (string, error) {
	return "", errors.New("exit status 1")
}

// TestRefusalStopsTheLoop is the difference between a tool that failed and a
// run that was refused: the model may adapt to the first, never to the second.
// The step count is the assertion that matters — a second call would mean the
// refusal had been handed back as an observation.
func TestRefusalStopsTheLoop(t *testing.T) {
	llm := &seqLLM{replies: []string{`write_file {"path":"../escape.txt","content":"x"}`, "DONE"}}
	res, err := Run(context.Background(), "task", llm, Options{MaxSteps: 10}, refusingTools{})
	if !errors.Is(err, refuse.ErrRefused) {
		t.Fatalf("Run error = %v, want a refusal", err)
	}
	if res.Done {
		t.Error("a refused run reported done")
	}
	if res.Steps != 1 {
		t.Errorf("steps = %d, want 1", res.Steps)
	}
	if llm.i != 1 {
		t.Errorf("model called %d times, want 1: the refusal was fed back as an observation", llm.i)
	}
	if len(res.Transcript) != 1 || res.Transcript[0].Role != "assistant" {
		t.Errorf("transcript = %+v, want the one reply that was refused", res.Transcript)
	}
}

// TestToolErrorIsAnObservation is the other side: an ordinary tool failure is
// something the model can read and work around, so the loop keeps going.
func TestToolErrorIsAnObservation(t *testing.T) {
	llm := &seqLLM{replies: []string{`run_command {"command":"ls -1"}`, "DONE"}}
	res, err := Run(context.Background(), "task", llm, Options{MaxSteps: 10}, failingTools{})
	if err != nil {
		t.Fatalf("Run error = %v, want the loop to carry on", err)
	}
	if !res.Done {
		t.Error("the loop did not reach DONE after a tool error")
	}
	if res.Steps != 2 {
		t.Errorf("steps = %d, want 2", res.Steps)
	}
	if got := res.Transcript[1].Observation; got != "ERROR: exit status 1" {
		t.Errorf("observation = %q, want the error as text", got)
	}
}

type fixedLLM struct{ reply string }

func (f fixedLLM) Complete(ctx context.Context, prompt string) (string, error) { return f.reply, nil }

type seqLLM struct {
	replies []string
	i       int
}

func (s *seqLLM) Complete(ctx context.Context, prompt string) (string, error) {
	if s.i >= len(s.replies) {
		return "DONE", nil
	}
	r := s.replies[s.i]
	s.i++
	return r, nil
}

func TestStepCap(t *testing.T) {
	// A valid, non-terminating action: the loop must stop at the cap, not loop.
	res, err := Run(context.Background(), "task", fixedLLM{"read_file"}, Options{MaxSteps: 3}, stubTools{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("expected not done")
	}
	if res.Steps != 3 {
		t.Fatalf("steps = %d, want 3", res.Steps)
	}
	if len(res.Transcript) != 6 {
		t.Fatalf("transcript length = %d, want 6 (3 assistant + 3 tool)", len(res.Transcript))
	}
}

func TestInvalidActionFedBack(t *testing.T) {
	// Invalid output is an observation, not a crash: the run keeps stepping.
	res, err := Run(context.Background(), "task", fixedLLM{"garbage"}, Options{MaxSteps: 2}, stubTools{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("expected not done")
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2", res.Steps)
	}
	for i, turn := range res.Transcript {
		if turn.Role != "assistant" {
			t.Errorf("turn %d role = %q, want assistant", i, turn.Role)
		}
	}
}

func TestMaxStepsFallback(t *testing.T) {
	for _, ms := range []int{0, -1, -100} {
		res, err := Run(context.Background(), "task", fixedLLM{"garbage"}, Options{MaxSteps: ms}, stubTools{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Steps != defaultMaxSteps {
			t.Errorf("maxSteps %d: steps = %d, want %d", ms, res.Steps, defaultMaxSteps)
		}
	}
}

func TestTranscript(t *testing.T) {
	llm := &seqLLM{replies: []string{`read_file {"path":"README.md"}`, "DONE"}}
	res, err := Run(context.Background(), "task", llm, Options{MaxSteps: 10}, stubTools{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("expected done")
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2", res.Steps)
	}
	want := []Turn{
		{Role: "assistant", Text: `read_file {"path":"README.md"}`},
		{Role: "tool", Tool: "read_file", Observation: "ok"},
		{Role: "assistant", Text: "DONE"},
	}
	if len(res.Transcript) != len(want) {
		t.Fatalf("transcript length = %d, want %d", len(res.Transcript), len(want))
	}
	for i := range want {
		if res.Transcript[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, res.Transcript[i], want[i])
		}
	}
	if got, want := res.Summary(), []string{"assistant", "read_file", "assistant"}; len(got) != len(want) {
		t.Fatalf("summary = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("summary[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
}

// TestRunOfflineDeterministic pins the offline run's shape: the mock planner
// always lists the files once and then reports DONE.
func TestRunOfflineDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := tools.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), "List the files", MockLLM{}, Options{}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatal("expected done")
	}
	if res.Steps != 2 {
		t.Fatalf("steps = %d, want 2", res.Steps)
	}
	if got, want := res.Summary(), []string{"assistant", "run_command", "assistant"}; len(got) != len(want) {
		t.Fatalf("summary = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("summary[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
}
