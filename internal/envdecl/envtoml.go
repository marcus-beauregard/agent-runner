package envdecl

import (
	"fmt"
	"strings"
)

// envTOML is the shape of an optional agent-runner.env.toml: array-of-tables
// declaring tools the repo's own files cannot, packages to install, and
// services to run beside the repo.
//
//	[[tool]]
//	name = "go"
//	version = "1.23"
//
//	[[package]]
//	name = "postgresql-client"
//
//	[[service]]
//	name = "postgres"
//	image = "postgres:16"
type envTOML struct {
	Tools    []envTool
	Packages []envPackage
	Services []envService
}

type envTool struct {
	Name    string
	Version string
}

type envPackage struct {
	Name string
}

type envService struct {
	Name  string
	Image string
}

// parseEnvTOML parses the optional agent-runner.env.toml into the Decl.
func parseEnvTOML(content string, d *Decl) error {
	e, err := parseTOML(content)
	if err != nil {
		return err
	}
	for _, t := range e.Tools {
		if t.Name == "" {
			return fmt.Errorf("[[tool]] without a name")
		}
		d.addTool(t.Name, t.Version, "agent-runner.env.toml#tool."+t.Name)
	}
	for _, p := range e.Packages {
		if p.Name == "" {
			return fmt.Errorf("[[package]] without a name")
		}
		d.Packages = append(d.Packages, p.Name)
	}
	for _, s := range e.Services {
		if s.Name == "" {
			return fmt.Errorf("[[service]] without a name")
		}
		if s.Image != "" {
			d.Services = append(d.Services, s.Name+" ("+s.Image+")")
		} else {
			d.Services = append(d.Services, s.Name)
		}
	}
	return nil
}

// tomlSection returns the key = "value" pairs of the [name] section of a
// small TOML document, with comments and blank lines stripped. It covers the
// single-section reads pyproject.toml and rust-toolchain.toml need; array
// tables are left to parseTOML.
func tomlSection(content, name string) (map[string]string, bool) {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(stripTOMLComment(line))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			in = line == "["+name+"]"
			continue
		}
		if in {
			k, v, ok := splitTOMLkv(line)
			if !ok {
				continue
			}
			out[k] = v
		}
	}
	return out, len(out) > 0 || in
}

// parseTOML reads the array-of-tables agent-runner.env.toml uses. It is not a
// TOML implementation: sections other than tool/package/service, and value
// types other than strings, are rejected rather than misread.
func parseTOML(content string) (envTOML, error) {
	var out envTOML
	var curTool *envTool
	var curPkg *envPackage
	var curSvc *envService
	section := ""

	for n, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(stripTOMLComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]")
			switch section {
			case "tool":
				out.Tools = append(out.Tools, envTool{})
				curTool = &out.Tools[len(out.Tools)-1]
				curPkg, curSvc = nil, nil
			case "package":
				out.Packages = append(out.Packages, envPackage{})
				curPkg = &out.Packages[len(out.Packages)-1]
				curTool, curSvc = nil, nil
			case "service":
				out.Services = append(out.Services, envService{})
				curSvc = &out.Services[len(out.Services)-1]
				curTool, curPkg = nil, nil
			default:
				return envTOML{}, fmt.Errorf("line %d: unsupported table [[%s]]", n+1, section)
			}
			continue
		}
		if strings.HasPrefix(line, "[") {
			return envTOML{}, fmt.Errorf("line %d: unsupported section %s", n+1, line)
		}
		k, v, ok := splitTOMLkv(line)
		if !ok {
			return envTOML{}, fmt.Errorf("line %d: not a key = value pair: %q", n+1, line)
		}
		switch {
		case curTool != nil:
			switch k {
			case "name":
				curTool.Name = v
			case "version":
				curTool.Version = v
			default:
				return envTOML{}, fmt.Errorf("line %d: unsupported key %q in [[tool]]", n+1, k)
			}
		case curPkg != nil:
			if k != "name" {
				return envTOML{}, fmt.Errorf("line %d: unsupported key %q in [[package]]", n+1, k)
			}
			curPkg.Name = v
		case curSvc != nil:
			switch k {
			case "name":
				curSvc.Name = v
			case "image":
				curSvc.Image = v
			default:
				return envTOML{}, fmt.Errorf("line %d: unsupported key %q in [[service]]", n+1, k)
			}
		default:
			return envTOML{}, fmt.Errorf("line %d: key %q outside any table", n+1, k)
		}
	}
	return out, nil
}

// splitTOMLkv splits `key = "value"`. Values must be double-quoted strings;
// anything else is not what this package writes, so it is an error rather
// than a guess.
func splitTOMLkv(line string) (string, string, bool) {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:eq])
	v := strings.TrimSpace(line[eq+1:])
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return "", "", false
	}
	return k, v[1 : len(v)-1], true
}

// stripTOMLComment removes a trailing # comment, respecting quotes.
func stripTOMLComment(line string) string {
	inQuote := false
	for i, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '#' && !inQuote:
			return line[:i]
		}
	}
	return line
}
