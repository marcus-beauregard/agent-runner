// Package agent implements the plan -> act -> observe -> reflect loop: a
// step-capped conversation that records every turn in a transcript.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// LLM is the model the loop drives. Complete returns the next reply for a
// prompt.
type LLM interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// Action is a parsed single-line tool call.
type Action struct {
	Name string
	Args map[string]any
}

// Turn is one transcript entry: an assistant reply or a tool observation.
type Turn struct {
	Role        string
	Text        string
	Tool        string
	Observation string
}

// Result is the outcome of a run.
type Result struct {
	Done       bool
	Steps      int
	Transcript []Turn
}

// Toolset is the slice of the tool registry the loop needs. It is implemented
// by *tools.Registry.
type Toolset interface {
	Names() []string
	Has(name string) bool
	Run(ctx context.Context, name string, args map[string]any) (string, error)
}

// Options tunes a run.
type Options struct {
	MaxSteps int
}

const defaultMaxSteps = 10

// doneRe matches a bare DONE, case-insensitively, at the start of a trimmed
// reply — the only way a model signals completion.
var doneRe = regexp.MustCompile(`(?i)^DONE`)

// ParseAction parses one line of the form `tool_name {"json": "args"}` or a
// bare `tool_name`. It reports false when the line is empty, names an unknown
// tool, or carries arguments that are not a JSON object.
func ParseAction(text string, hasTool func(string) bool) (*Action, bool) {
	line := strings.TrimSpace(text)
	if line == "" {
		return nil, false
	}
	idx := strings.IndexFunc(line, unicode.IsSpace)
	var name, rest string
	if idx < 0 {
		name = line
	} else {
		name = strings.TrimSpace(line[:idx])
		rest = strings.TrimSpace(line[idx+1:])
	}
	if !hasTool(name) {
		return nil, false
	}
	if rest == "" {
		return &Action{Name: name, Args: map[string]any{}}, true
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rest), &args); err != nil {
		return nil, false
	}
	return &Action{Name: name, Args: args}, true
}

// Run drives the loop to completion or the step cap. Invalid actions and tool
// errors are observations, not failures; only a model transport error aborts.
func Run(ctx context.Context, task string, llm LLM, opts Options, ts Toolset) (Result, error) {
	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	transcript := make([]Turn, 0)
	prompt := strings.Join([]string{
		"You are an autonomous agent completing a task.",
		"Available tools: " + strings.Join(ts.Names(), ", "),
		"Each turn reply with exactly ONE line:",
		`  tool_name {"arg": "value"}`,
		"or reply DONE when finished.",
		"Task: " + task,
	}, "\n")

	for step := 0; step < maxSteps; step++ {
		reply, err := llm.Complete(ctx, prompt)
		if err != nil {
			return Result{}, fmt.Errorf("model: %w", err)
		}
		transcript = append(transcript, Turn{Role: "assistant", Text: reply})
		if doneRe.MatchString(strings.TrimSpace(reply)) {
			return Result{Done: true, Steps: step + 1, Transcript: transcript}, nil
		}
		action, ok := ParseAction(reply, ts.Has)
		if !ok {
			prompt += "\n(Invalid action. Use tool_name {\"arg\":\"value\"} or DONE.)"
			continue
		}
		observation, err := ts.Run(ctx, action.Name, action.Args)
		if err != nil {
			observation = "ERROR: " + err.Error()
		}
		transcript = append(transcript, Turn{Role: "tool", Tool: action.Name, Observation: observation})
		prompt += "\nObservation: " + observation
	}
	return Result{Done: false, Steps: maxSteps, Transcript: transcript}, nil
}

// Summary reduces the transcript to the shape index.js printed: the tool name
// for tool turns, otherwise the role.
func (r Result) Summary() []string {
	out := make([]string, len(r.Transcript))
	for i, t := range r.Transcript {
		if t.Tool != "" {
			out[i] = t.Tool
		} else {
			out[i] = t.Role
		}
	}
	return out
}

// MockLLM is a deterministic offline planner so the runtime runs without a key
// and every run is reproducible. Swap it for a real client to go live.
type MockLLM struct{}

func (MockLLM) Complete(ctx context.Context, prompt string) (string, error) {
	if strings.Contains(prompt, "run_command") && !strings.Contains(prompt, "Observation") {
		return `run_command {"command": "ls -1"}`, nil
	}
	if strings.Contains(prompt, "Observation:") {
		return "DONE", nil
	}
	return `read_file {"path": "README.md"}`, nil
}
