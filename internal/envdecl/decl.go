// Package envdecl reads a repository's environment declarations and hashes
// them, so a runner can answer "what does this repo need, and what is the
// hash of that answer" without executing or reaching the network.
//
// The declarations are the files a repo already carries: go.mod, package.json,
// rust-toolchain.toml, .python-version / pyproject.toml, a Dockerfile, the
// lockfiles that pin dependencies, and an optional agent-runner.env.toml for
// what those files cannot say (services, extra tools). Read parses them into a
// Decl; Digest hashes the Decl plus the bytes of every file that declared
// something, so an edit changes the digest only when it changes bytes that
// matter.
package envdecl

// Tool is one toolchain entry: a name, the version or version spec the repo
// asks for, and the exact place that asked for it.
type Tool struct {
	Name    string // "go", "node", "rust", an image name like "postgres", ...
	Version string // "1.22", ">=20", "18" — a pin or a spec, verbatim
	Source  string // "file#location", e.g. "package.json#engines.node"
}

// Decl is what a repository declares it needs. The zero value is a valid
// declaration — a repo that declares nothing is an empty Decl, not an error;
// refusing to run is env-init's decision, not this package's.
//
// Toolchain entries each carry their own Source. Packages and Services are
// plain names justified by the files listed in Source. When two files disagree
// (a Dockerfile on node 18, engines on >=20) both entries are kept: the
// disagreement is the information, and each entry's Source says where it came
// from.
type Decl struct {
	Toolchain []Tool
	Packages  []string // lockfiles that pin the dependency set, e.g. "go.sum"
	Services  []string // services expected beside the repo, e.g. "postgres (postgres:16)"
	Source    []string // every declaring file Read parsed, sorted

	// files maps each declaring file to the SHA-256 of its bytes, so Digest
	// covers the content, not just what was parsed out of it. It is set by
	// Read; a Decl built by hand hashes its declaration only.
	files map[string]string
}
