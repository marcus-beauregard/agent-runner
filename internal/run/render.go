package run

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The event kinds the report reads. They are named next to their payloads
// because this file is the record's first reader, and the shape it reads is the
// shape writers have to write.
const (
	kindStage    = "stage"
	kindGate     = "gate"
	kindDecision = "decision"
)

// stageLabel names a stage in the document. Event.Stage is optional, and a gate
// or decision that recorded no stage is still evidence the run did something:
// dropping it would hide the only proof a gate ran, and silence can only make a
// report read more reassuring than the record. Such events are gathered under
// the empty name — never under a name a writer could also choose — and printed
// as unattributed.
func stageLabel(name string) string {
	if name == "" {
		return "(unattributed)"
	}
	return name
}

// stageData is the payload of a stage event. "start" and "finish" are the two
// phases §5 names, and hash is the output hash a finish carries — the only thing
// in the record that says a stage produced what it was supposed to.
type stageData struct {
	Event string `json:"event"`
	Hash  string `json:"hash"`
}

// gateData is one gate result (§9): the command quoted from the repo, what it
// returned, how long it took, and the tail of what it printed. skipped is set
// instead when the gate never ran, and carries the reason — a gate that did not
// run is recorded too, or the report would read it as a pass.
type gateData struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	OutputTail string `json:"output_tail"`
	Skipped    string `json:"skipped"`
}

// decisionData is a question the run had to answer and the answer it chose. An
// empty answer is work the run did not finish, which is why the report keeps it.
type decisionData struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// stageRow is one stage as the report sees it, assembled from every event that
// named it.
type stageRow struct {
	name      string
	started   time.Time
	finished  time.Time
	hash      string
	decisions []decisionData
	// namelessStage marks the bucket when a stage event carried no stage name.
	// Such an event names work nobody can attribute, so it is neither a stage
	// section nor silently absent from what is left to do.
	namelessStage bool
}

// gateRow is a gate result with the stage it ran in.
type gateRow struct {
	stage string
	gateData
}

// RenderReport renders the record in runDir as Markdown for a person: the run's
// header, one section per stage in the order the stages appear, the gates, and
// what is left to do. It only reads — a report that repaired its input would
// destroy the evidence a resume depends on — and every time it prints is UTC.
func RenderReport(runDir string) ([]byte, error) {
	rec, err := loadRecord(runDir)
	if err != nil {
		return nil, err
	}
	events, torn, err := readLedger(filepath.Join(runDir, "run.ndjson"))
	if err != nil {
		return nil, err
	}
	stages, gates, err := collect(events)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	writeHeader(&b, rec)
	if torn != "" {
		fmt.Fprintf(&b, "> Note: %s.\n\n", torn)
	}
	writeRefusal(&b, rec.Refusal)
	for _, s := range stages {
		writeStage(&b, s)
	}
	writeGates(&b, gates)
	writeLeft(&b, rec, stages, gates)
	return []byte(b.String()), nil
}

// readLedger reads every whole event in path, in file order. It never writes.
// A missing file is an empty record — a run that has just been created has a
// run.json and no stream yet — and a torn final line is dropped with a note:
// that write never completed and held no event. A complete line that does not
// parse is an error, because skipping it would let the report claim a stage
// finished while the evidence is unreadable.
func readLedger(path string) ([]Event, string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read ledger %s: %w", path, err)
	}
	defer f.Close()

	var events []Event
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, readErr := r.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, "", fmt.Errorf("read ledger %s: %w", path, readErr)
		}
		if line == "" {
			return events, "", nil
		}
		if !strings.HasSuffix(line, "\n") {
			return events, fmt.Sprintf("a torn final line (%d bytes) was ignored; it held no complete event", len(line)), nil
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(strings.TrimSuffix(line, "\n")), &e); err != nil {
			return nil, "", fmt.Errorf("read ledger %s: line %d: %w", path, n, err)
		}
		events = append(events, e)
	}
}

// collect walks the events once and returns the stages in the order they first
// appear, plus every gate. The order is the ledger's, not a sorted copy of it: a
// report that reorders the record can hide a stage running out of turn.
func collect(events []Event) ([]stageRow, []gateRow, error) {
	byName := make(map[string]int)
	var stages []stageRow
	var gates []gateRow

	for _, e := range events {
		index := func(name string) int {
			if i, ok := byName[name]; ok {
				return i
			}
			stages = append(stages, stageRow{name: name})
			byName[name] = len(stages) - 1
			return len(stages) - 1
		}

		switch e.Kind {
		case kindStage:
			var d stageData
			if err := decodeData(e, &d); err != nil {
				return nil, nil, err
			}
			if e.Stage == "" {
				// A stage event with no name records work the report cannot
				// attribute. Dropping it would let "Left to do" say nothing is
				// owed over an event the record plainly holds, so the fact is
				// kept in the bucket without inventing a stage for it.
				stages[index("")].namelessStage = true
				continue
			}
			s := &stages[index(e.Stage)]
			switch d.Event {
			case "start":
				if s.started.IsZero() {
					s.started = e.TS
				}
			case "finish":
				s.finished = e.TS
				s.hash = d.Hash
			}
		case kindGate:
			var d gateData
			if err := decodeData(e, &d); err != nil {
				return nil, nil, err
			}
			index(e.Stage)
			gates = append(gates, gateRow{stage: e.Stage, gateData: d})
		case kindDecision:
			var d decisionData
			if err := decodeData(e, &d); err != nil {
				return nil, nil, err
			}
			s := &stages[index(e.Stage)]
			s.decisions = append(s.decisions, d)
		}
	}
	return stages, gates, nil
}

// decodeData unmarshals one event's payload. An absent payload decodes to the
// zero value; a payload of the wrong shape is an error, not a blank line.
func decodeData(e Event, v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Data, v); err != nil {
		return fmt.Errorf("event %d (%s): %w", e.Seq, e.Kind, err)
	}
	return nil
}

// writeHeader prints what the run was, before what it did.
func writeHeader(b *strings.Builder, rec *record) {
	fmt.Fprintf(b, "# Run %s\n\n", rec.ID)
	fields := []struct{ label, value string }{
		{"Kind", rec.Kind},
		{"Subject", rec.Subject},
		{"Repo", rec.Repo},
		{"Branch", rec.Branch},
		{"Base", rec.BaseSHA},
		{"Budget", rec.Budget},
		{"Started", started(rec.CreatedAt)},
		{"State", rec.State},
	}
	for _, f := range fields {
		if f.value != "" {
			fmt.Fprintf(b, "- %s: %s\n", f.label, f.value)
		}
	}
	if rec.ExitCode != nil {
		fmt.Fprintf(b, "- Exit code: %d\n", *rec.ExitCode)
	} else {
		b.WriteString("- Exit code: none yet\n")
	}
	b.WriteString("\n")
}

// writeRefusal prints why the run stopped, before what it did: a reader who
// knows the run refused reads every later section in that light. The evidence
// is printed in key order so two renders of one record are the same bytes.
func writeRefusal(b *strings.Builder, ref *refusalRecord) {
	if ref == nil {
		return
	}
	b.WriteString("## Refused\n\n")
	if ref.Where != "" {
		fmt.Fprintf(b, "- Stage: %s\n", ref.Where)
	}
	fmt.Fprintf(b, "- Code: %s\n- Reason: %s\n", ref.Code, ref.Reason)
	if len(ref.Evidence) > 0 {
		keys := make([]string, 0, len(ref.Evidence))
		for k := range ref.Evidence {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("- Evidence:\n")
		for _, k := range keys {
			fmt.Fprintf(b, "  - %s: %s\n", k, cell(ref.Evidence[k]))
		}
	}
	b.WriteString("\n")
}

// writeStage prints one stage: whether it started, whether it finished, the hash
// it finished with, and the questions it recorded on the way.
func writeStage(b *strings.Builder, s stageRow) {
	if s.name == "" {
		// The unattributed bucket is not a stage: nothing in it started,
		// finished or produced an output hash, so it gets a heading and the
		// decisions it holds, and no stage lines that would imply otherwise.
		b.WriteString("## Unattributed events\n\n")
		if s.namelessStage {
			b.WriteString("- A stage event recorded no stage name; it cannot be attributed or confirmed.\n")
		}
	} else {
		fmt.Fprintf(b, "## %s\n\n", s.name)
		switch {
		case !s.started.IsZero() && !s.finished.IsZero():
			fmt.Fprintf(b, "- Started: %s\n- Finished: %s\n", stamp(s.started), stamp(s.finished))
		case !s.started.IsZero():
			fmt.Fprintf(b, "- Started: %s\n- Finished: not yet\n", stamp(s.started))
		case !s.finished.IsZero():
			fmt.Fprintf(b, "- Started: not recorded\n- Finished: %s\n", stamp(s.finished))
		default:
			b.WriteString("- No stage events recorded beyond the name\n")
		}
		if s.hash != "" {
			fmt.Fprintf(b, "- Output hash: %s\n", s.hash)
		}
	}
	for _, d := range s.decisions {
		if d.Answer == "" {
			fmt.Fprintf(b, "- Asked: %q — no answer recorded\n", d.Question)
			continue
		}
		fmt.Fprintf(b, "- Asked: %q — answered: %s\n", d.Question, d.Answer)
	}
	b.WriteString("\n")
}

// writeGates prints every gate as one table row, in the order the gates ran.
func writeGates(b *strings.Builder, gates []gateRow) {
	if len(gates) == 0 {
		return
	}
	b.WriteString("## Gates\n\n| Stage | Command | Exit | Time | Output tail |\n|---|---|---|---|---|\n")
	for _, g := range gates {
		exit, took, tail := fmt.Sprintf("%d", g.ExitCode), duration(g.DurationMS), g.OutputTail
		if g.Skipped != "" {
			exit, took, tail = "skipped", "—", g.Skipped
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			cell(stageLabel(g.stage)), cell(g.Command), exit, took, cell(tail))
	}
	b.WriteString("\n")
}

// writeLeft prints the closing section: everything in the record that says the
// run is not finished. It is assembled from the events rather than stored, so a
// resumed run and a report cannot disagree about what is still owed.
func writeLeft(b *strings.Builder, rec *record, stages []stageRow, gates []gateRow) {
	b.WriteString("## Left to do\n\n")
	var items []string
	for _, s := range stages {
		if s.name == "" && s.namelessStage {
			items = append(items, "A stage event recorded no stage name; its work cannot be confirmed from the record.")
		}
		// A stage known only from a gate or a decision has no start to resume
		// from; claiming it "started" would contradict the section above it.
		// The unattributed bucket is not a stage, so it is never named as one.
		if s.name != "" && s.finished.IsZero() {
			if s.started.IsZero() {
				items = append(items, fmt.Sprintf("Stage %s recorded no stage events; its work cannot be confirmed from the record.", s.name))
			} else {
				items = append(items, fmt.Sprintf("Stage %s started and never finished; resume from it.", s.name))
			}
		}
		for _, d := range s.decisions {
			if d.Answer != "" {
				continue
			}
			if s.name == "" {
				items = append(items, fmt.Sprintf("A decision recorded no stage and no answer: %q", d.Question))
				continue
			}
			items = append(items, fmt.Sprintf("Stage %s left a question open: %q", s.name, d.Question))
		}
	}
	for _, g := range gates {
		switch {
		case g.Skipped != "":
			items = append(items, fmt.Sprintf("Gate %s was skipped: %s", g.Command, g.Skipped))
		case g.ExitCode != 0:
			items = append(items, fmt.Sprintf("Gate %s failed with exit %d.", g.Command, g.ExitCode))
		}
	}
	switch {
	case rec.State == stateRunning:
		items = append(items, "The record still says running: the run is resumable, not finished.")
	case rec.State == "":
		// loadRecord accepts a record with no state. It is not done — nothing in
		// it says so — and a report that guessed would be inventing the one
		// field that decides whether the run is finished.
		items = append(items, "The record does not say what state it is in; treat it as unfinished.")
	case rec.State != stateDone:
		// Only done owes nothing. A refused, needs_human or failed record with a
		// tidy event stream still stopped early, and this section is the one a
		// reader trusts to say so — it must never print "Nothing" over a
		// non-zero exit code.
		line := fmt.Sprintf("The record ended %s", rec.State)
		if rec.ExitCode != nil {
			line += fmt.Sprintf(" with exit code %d", *rec.ExitCode)
		}
		items = append(items, line+": the run stopped before it completed.")
	case rec.ExitCode != nil && *rec.ExitCode != 0:
		// State and exit code are two fields in run.json, not values this
		// reader derives from each other. A record can say done and still carry
		// a non-zero code, and "Nothing" over that pair is the same lie the
		// cases above exist to prevent.
		items = append(items, fmt.Sprintf("The record says done but exited with code %d; the run did not finish cleanly.", *rec.ExitCode))
	}
	if len(items) == 0 {
		b.WriteString("Nothing: the record says done, every stage that started finished, and every gate that ran passed.\n")
		return
	}
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
}

// cell renders one table cell: whitespace collapsed to single spaces so a
// multi-line tail does not break the row, pipes escaped, and a long tail cut at
// a readable length.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

// duration prints a gate's wall clock the way a person reads it: milliseconds
// under a second, one decimal above it.
func duration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// stamp prints an event time in UTC. Event timestamps are already UTC and the
// conversion is the promise, not a correction: a report never shows a local time.
func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// started prints the created_at string from run.json in UTC. A string that does
// not parse is printed as it was recorded; rewriting it would lose the only
// value that exists.
func started(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return stamp(t)
}

// Locate returns the directory of run id under base, or ErrNoRecord when there
// is none. An empty base means the default state directory, the same one New
// writes to, so report and work --run-dir agree without a flag. An id is a name,
// never a path: anything that could walk out of runs/ is refused before a stat,
// so a caller cannot be talked into reading an arbitrary file.
func Locate(base, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("%w: %q", ErrNoRecord, id)
	}
	if base == "" {
		d, err := defaultBase()
		if err != nil {
			return "", err
		}
		base = d
	}
	dir := filepath.Join(base, "agent-runner", "runs", id)
	if _, err := os.Stat(filepath.Join(dir, "run.json")); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNoRecord, id)
	}
	return dir, nil
}
