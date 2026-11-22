// Package gate answers one question about a gate command before a run starts:
// could it run on this host at all? A gate that names a program the host does
// not have is a refusal (PRD §11), not a failed gate (PRD §13): the code was
// never given the chance to be wrong, and saying the tests failed would blame
// the patch for the machine.
//
// The question is put to the shell that runs the gate, not to a list of names
// kept here: mapfile is a builtin in bash and not in dash, and only the
// interpreter knows which one it is.
package gate

import (
	"errors"
	"os/exec"
	"strings"
)

// shell is the interpreter a gate runs under. It is an absolute path because
// the caller's PATH is the thing being questioned.
const shell = "/bin/sh"

// keywords open a command rather than name one; the word after them is the
// program, so "if go test" is a check on go. "function" is here for the
// definition itself: the name it defines is not a program until it is called.
var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true,
	"for": true, "while": true, "until": true, "do": true, "!": true,
	"time": true, "function": true,
}

// openers begin a compound command whose next word is a variable or a
// condition, not a program: "for f in ./cmd/*" is a loop, and refusing it for
// want of a program called f would stop a gate that runs. The body of every one
// of these is its own segment, so bailing on the opening word loses nothing.
var openers = map[string]bool{
	"for": true, "while": true, "until": true, "select": true,
	"case": true, "esac": true,
}

// Missing returns the programs the gate command names that this host cannot
// run, in the order they appear and without repeats.
//
// It reads the first word of each command segment, which is where a shell looks
// for a program, and asks the shell whether it can resolve that word. It
// deliberately does not unravel quoting or substitution: a program hidden
// inside "$(...)" is not one the gate can be refused for, and guessing there
// would refuse a gate that runs fine.
//
// Two things it does not see, both by design, because the cost of a wrong
// refusal is a run that never starts: a program behind a wrapper ("timeout 300
// go test" is a miss, not a refusal), and a path such as "./gradlew", which the
// shell resolves against a worktree that does not exist yet. A miss is silent;
// a false refusal is not.
func Missing(cmd string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, seg := range segments(cmd) {
		if definesFunction(seg) {
			return nil // a gate that defines its own commands cannot be judged from its words
		}
		prog := program(seg)
		if prog == "" || seen[prog] {
			continue
		}
		seen[prog] = true
		if !resolvable(prog) {
			missing = append(missing, prog)
		}
	}
	return missing
}

// resolvable asks the gate's own shell whether it can resolve one word: a
// program on PATH or a builtin. A builtin is found here and nowhere else —
// ulimit exists only because the shell has it — and a subprocess is the
// cheapest honest answer. The word is passed as an argument, never into the
// script text.
func resolvable(prog string) bool {
	err := exec.Command(shell, "-c", `command -v -- "$1" >/dev/null 2>&1`, "sh", prog).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false // the shell looked and did not find it
	}
	return true // resolved, or the shell could not be asked: never refuse on a guess
}

// definesFunction reports whether a segment defines a shell function, as in
// "ci() { go test ./...; }". The name is a program only after the definition
// has run, so a gate that defines one is not a gate this package can read, and
// a call to it later in the same line would look like a missing program.
func definesFunction(seg string) bool {
	open := strings.IndexByte(seg, '(')
	if open <= 0 {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(seg[open+1:]), ")") {
		return false
	}
	name := strings.TrimSpace(seg[:open])
	return name != "" && !strings.ContainsAny(name, " \t") && isProgramName(name)
}

// segments splits a command line where a shell starts a new command: a newline,
// a semicolon, or a pipe. "&&" is two pipes for this purpose; a single "&" ends
// a segment too, except in a redirection such as "2>&1", which is part of the
// word before it. Quoted separators are kept inside their segment, because a
// quoted "a|b" is an argument, not two commands.
func segments(cmd string) []string {
	rs := []rune(cmd)
	var segs []string
	start := 0
	var quote rune
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '\n', ';':
			segs = append(segs, string(rs[start:i]))
			start = i + 1
		case '|':
			segs = append(segs, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '|' {
				i++
			}
			start = i + 1
		case '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				segs = append(segs, string(rs[start:i]))
				i++
				start = i + 1
				continue
			}
			if i > 0 && rs[i-1] == '>' {
				continue
			}
			segs = append(segs, string(rs[start:i]))
			start = i + 1
		}
	}
	return append(segs, string(rs[start:]))
}

// program is the word a segment names as its program: leading assignments
// (CGO_ENABLED=0), keywords and redirections are skipped. It returns "" for a
// word this package cannot judge — a quoted or substituted name, a subshell, a
// redirection, a path — because a gate that cannot be read is not a gate that
// cannot run.
//
// Paths are skipped rather than looked up: "./gradlew" is resolved by the shell
// against the worktree, and the plan stage has no worktree yet, so a lookup here
// would answer a question about the operator's cwd. A miss is silent; a false
// refusal stops a gate that works.
func program(seg string) string {
	words := words(seg)
	if len(words) > 0 && openers[words[0]] {
		return ""
	}
	for _, f := range words {
		if keywords[f] || isAssignment(f) {
			continue
		}
		// An option belongs to the word before it, and a comment is not a
		// command: neither can name a program, and both appear in a gate copied
		// from a CI file ("time -p go test", a leading "# run the suite").
		if strings.HasPrefix(f, "-") || strings.HasPrefix(f, "#") {
			return ""
		}
		if !isProgramName(f) || strings.ContainsRune(f, '/') {
			return ""
		}
		return f
	}
	return ""
}

// words splits a segment into the words a shell would hand a program, keeping a
// quoted run together: GOFLAGS="-mod=vendor -tags ci" is one word, so its tail
// is an argument and not a program called -tags. Quotes stay in the word, so
// isProgramName can still refuse a name only the shell could produce.
func words(seg string) []string {
	var out []string
	var cur []rune
	var quote rune
	started := false
	flush := func() {
		if started {
			out = append(out, string(cur))
			cur = nil
			started = false
		}
	}
	for _, r := range seg {
		switch {
		case quote != 0:
			cur = append(cur, r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			cur = append(cur, r)
			started = true
		case r == ' ' || r == '\t' || r == '\r':
			flush()
		default:
			cur = append(cur, r)
			started = true
		}
	}
	flush()
	return out
}

// isAssignment reports whether f is a leading VAR=value assignment.
func isAssignment(f string) bool {
	i := strings.IndexByte(f, '=')
	if i <= 0 {
		return false
	}
	for _, r := range f[:i] {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// isProgramName reports whether f can be looked up as a program word: a plain
// name, possibly with a path, and nothing a shell would have to expand first.
func isProgramName(f string) bool {
	if f == "" {
		return false
	}
	for _, r := range f {
		switch r {
		case '"', '\'', '$', '(', ')', '{', '}', '<', '>', '\\', 0x60:
			return false
		}
	}
	return true
}
