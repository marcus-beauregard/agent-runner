// Package gate answers one question about a gate command before a run starts:
// could it run on this host at all? A gate that names a program the host does
// not have is a refusal (PRD §11), not a failed gate (PRD §13): the code was
// never given the chance to be wrong, and saying the tests failed would blame
// the patch for the machine.
package gate

import (
	"os/exec"
	"strings"
)

// shellWords are the words a shell runs itself, so a segment that starts with
// one of them needs no program on PATH. Keywords are here because a gate may be
// a short shell fragment: "if go test ./...; then ... fi" is a command line a
// maintainer copied from CI, not a program.
var shellWords = map[string]bool{
	"test": true, "[": true, "[[": true, ":": true, ".": true,
	"cd": true, "echo": true, "printf": true, "export": true,
	"set": true, "unset": true, "source": true, "command": true,
	"exec": true, "eval": true, "true": true, "false": true,
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
}

// keywords open a command rather than name one; the word after them is the
// program, so "if go test" is a check on go.
var keywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true,
	"for": true, "while": true, "until": true, "do": true, "!": true,
}

// Missing returns the programs the gate command names that this host cannot
// find, in the order they appear and without repeats.
//
// It reads the first word of each command segment, which is where a shell looks
// for a program, and it deliberately does not unravel quoting or substitution:
// a program hidden inside "$(...)" is not one the gate can be refused for, and
// guessing there would refuse a gate that runs fine. A miss is silent; a false
// refusal is not.
func Missing(cmd string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, seg := range segments(cmd) {
		prog := program(seg)
		if prog == "" || shellWords[prog] || seen[prog] {
			continue
		}
		seen[prog] = true
		if _, err := exec.LookPath(prog); err != nil {
			missing = append(missing, prog)
		}
	}
	return missing
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
// redirection — because a gate that cannot be read is not a gate that cannot
// run.
func program(seg string) string {
	for _, f := range strings.Fields(seg) {
		if keywords[f] || isAssignment(f) {
			continue
		}
		if !isProgramName(f) {
			return ""
		}
		return f
	}
	return ""
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
