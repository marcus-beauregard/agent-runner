package envdecl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// parseGoMod reads the go and toolchain directives. Both become entries: when
// they disagree (go 1.22, toolchain go1.22.5) both are kept and the Source
// says which directive said which.
func parseGoMod(content string, d *Decl) error {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		switch firstField(line) {
		case "go":
			v := strings.TrimSpace(strings.TrimPrefix(line, "go"))
			if v == "" {
				return fmt.Errorf("go directive without a version")
			}
			d.addTool("go", v, "go.mod#go")
		case "toolchain":
			v := strings.TrimSpace(strings.TrimPrefix(line, "toolchain"))
			v = strings.TrimPrefix(v, "go")
			if v == "" {
				return fmt.Errorf("toolchain directive without a version")
			}
			d.addTool("go", v, "go.mod#toolchain")
		}
	}
	return nil
}

// packageJSON is the subset of package.json that declares an environment.
type packageJSON struct {
	Engines        map[string]string `json:"engines"`
	PackageManager string            `json:"packageManager"`
}

// parsePackageJSON reads engines.* (each a toolchain entry, verbatim spec) and
// packageManager ("pnpm@9.1.0" becomes name pnpm, version 9.1.0).
func parsePackageJSON(content string, d *Decl) error {
	var p packageJSON
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return err
	}
	for _, engine := range sortedKeys(p.Engines) {
		d.addTool(engine, p.Engines[engine], "package.json#engines."+engine)
	}
	if p.PackageManager != "" {
		name, version := splitNameVersion(p.PackageManager)
		d.addTool(name, version, "package.json#packageManager")
	}
	return nil
}

// splitNameVersion splits "pnpm@9.1.0" on the last @; a scoped name like
// "@pnpm/exe@9" keeps its scope. A bare name gets an empty version.
func splitNameVersion(s string) (string, string) {
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// parseRustToolchainTOML reads [toolchain] channel from rust-toolchain.toml.
func parseRustToolchainTOML(content string, d *Decl) error {
	sec, ok := tomlSection(content, "toolchain")
	if !ok {
		return nil
	}
	if channel, ok := sec["channel"]; ok && channel != "" {
		d.addTool("rust", channel, "rust-toolchain.toml#toolchain.channel")
	}
	return nil
}

// parseRustToolchain reads the legacy rust-toolchain file, whose whole content
// is a version string.
func parseRustToolchain(content string, d *Decl) error {
	v := strings.TrimSpace(content)
	if v == "" {
		return nil
	}
	d.addTool("rust", v, "rust-toolchain")
	return nil
}

// parsePythonVersion reads .python-version, one version per line.
func parsePythonVersion(content string, d *Decl) error {
	v := strings.TrimSpace(content)
	if v == "" {
		return nil
	}
	d.addTool("python", v, ".python-version")
	return nil
}

// parsePyproject reads project.requires-python, a spec like ">=3.11".
func parsePyproject(content string, d *Decl) error {
	sec, ok := tomlSection(content, "project")
	if !ok {
		return nil
	}
	if req, ok := sec["requires-python"]; ok && req != "" {
		d.addTool("python", req, "pyproject.toml#project.requires-python")
	}
	return nil
}

// parseDockerfile reads every FROM line. Each image becomes an entry named
// after the image with the tag as version, so "FROM node:18" and an engines
// rule of ">=20" sit side by side as the conflict they are. A tagless image
// is "latest"; stage names (AS builder) are ignored. The directive must be
// the whole word FROM (a line like "FROMAGE ..." is not one), and leading
// --flags (e.g. "FROM --platform=linux/amd64 node:18") are skipped so the
// image is the first non-flag argument.
func parseDockerfile(content string, d *Decl) error {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 4 && strings.EqualFold(line[:4], "from") && (line[4] == ' ' || line[4] == '\t') {
			fields := strings.Fields(line)[1:]
			image := ""
			for _, f := range fields {
				if strings.HasPrefix(f, "--") {
					continue // --platform=..., --pull=... etc.
				}
				image = f
				break
			}
			if image == "" {
				return fmt.Errorf("FROM without an image: %q", line)
			}
			// A tag is a ':' after the last '/', so a registry port
			// ("registry:5000/img") is not mistaken for one.
			name, version := image, "latest"
			if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
				name, version = image[:i], image[i+1:]
			}
			d.addTool(name, version, "Dockerfile#FROM "+image)
		}
	}
	return nil
}

// sortedKeys returns the keys of m in sorted order, so map iteration never
// leaks into the declaration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort is fine at this size and keeps the import list small
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
