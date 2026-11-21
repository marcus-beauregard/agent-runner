package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus-beauregard/agent-runner/internal/report"
)

// probeReturning is a Probe that answers without touching the host, and records
// whether it was asked at all: an explicit worktree must not probe, and a test
// that cannot tell "not asked" from "asked" cannot prove it.
func probeReturning(ok bool, reason string, asked *bool) Probe {
	return func(context.Context) (bool, string) {
		if asked != nil {
			*asked = true
		}
		return ok, reason
	}
}

// TestResolveAuto is the ticket's fallback rule: auto takes the container when
// the host allows it, and otherwise gets worktree with a warning that names what
// is unenforced — never a silent downgrade.
func TestResolveAuto(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		ok        bool
		reason    string
		want      Mode
		wantWarn  string // a substring the warning must contain; empty means none
	}{
		{name: "default name takes container", requested: "", ok: true, want: Container},
		{name: "auto takes container", requested: "auto", ok: true, want: Container},
		{
			name:      "auto falls back and says why",
			requested: "auto",
			ok:        false,
			reason:    "bubblewrap is not installed",
			want:      Worktree,
			wantWarn:  "bubblewrap is not installed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mode, warn, err := Resolve(context.Background(), tc.requested, probeReturning(tc.ok, tc.reason, nil))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if mode != tc.want {
				t.Errorf("mode = %q, want %q", mode, tc.want)
			}
			if tc.wantWarn == "" {
				if warn != "" {
					t.Errorf("warning = %q, want none", warn)
				}
				return
			}
			if !strings.Contains(warn, tc.wantWarn) {
				t.Errorf("warning = %q, want it to contain %q", warn, tc.wantWarn)
			}
			if !strings.Contains(warn, Unenforced()) {
				t.Errorf("warning = %q, want it to name what is unenforced (%q)", warn, Unenforced())
			}
		})
	}
}

// TestResolveExplicitWorktreeDoesNotProbe pins that the weaker mode is a choice:
// asking for it is not a request permission is needed for, so detection never
// runs and no host fact can talk the run out of what it was told to do.
func TestResolveExplicitWorktreeDoesNotProbe(t *testing.T) {
	asked := false
	mode, warn, err := Resolve(context.Background(), "worktree", probeReturning(true, "", &asked))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if asked {
		t.Error("probe was called for an explicit worktree")
	}
	if mode != Worktree {
		t.Errorf("mode = %q, want %q", mode, Worktree)
	}
	if !strings.Contains(warn, Unenforced()) {
		t.Errorf("warning = %q, want it to name what is unenforced", warn)
	}
}

// TestResolveExplicitContainerRefused is the ticket's refusal: a container that
// the host cannot build exits 3 with the reason, and the reason has to reach the
// message — "refused" alone leaves an operator guessing.
func TestResolveExplicitContainerRefused(t *testing.T) {
	const reason = "unshare --net is not permitted"
	mode, warn, err := Resolve(context.Background(), "container", probeReturning(false, reason, nil))
	if err == nil {
		t.Fatal("Resolve returned no error for a container the host cannot build")
	}
	if !errors.Is(err, report.ErrRefused) {
		t.Errorf("error %v does not wrap report.ErrRefused", err)
	}
	if got := report.ExitCode(err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	var un ErrUnavailable
	if !errors.As(err, &un) {
		t.Errorf("error %v is not an ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error %q does not name the reason %q", err, reason)
	}
	if mode != "" {
		t.Errorf("mode = %q, want empty: no mode was granted", mode)
	}
	if warn != "" {
		t.Errorf("warning = %q, want none on a refusal", warn)
	}
}

// TestResolveRejectsUnknownMode keeps a typo from becoming a run: it is a usage
// error (exit 1), not a refusal, and no record should carry it.
func TestResolveRejectsUnknownMode(t *testing.T) {
	asked := false
	_, _, err := Resolve(context.Background(), "chroot", probeReturning(true, "", &asked))
	if err == nil {
		t.Fatal("Resolve accepted an unknown mode")
	}
	if errors.Is(err, report.ErrRefused) {
		t.Errorf("error %v is a refusal; a typo is a usage error", err)
	}
	if asked {
		t.Error("probe was called for an unknown mode")
	}
	if !strings.Contains(err.Error(), "chroot") {
		t.Errorf("error %q does not name the bad value", err)
	}
}

// stubTools puts exactly these scripts on PATH, alone, so detection is tested
// against a host state the test built rather than whatever this machine has.
func stubTools(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir)
}

// TestDetectRefusesWhenNoNamespaceCanBeBuilt covers the three ways detection can
// answer no. Each names the tool that failed, because "container unavailable" on
// its own cannot be acted on.
func TestDetectRefusesWhenNoNamespaceCanBeBuilt(t *testing.T) {
	tests := []struct {
		name     string
		stubs    func(t *testing.T)
		wantName string
	}{
		{
			name:     "bubblewrap not installed",
			stubs:    func(t *testing.T) { t.Setenv("PATH", t.TempDir()) },
			wantName: "bubblewrap",
		},
		{
			name: "bubblewrap cannot build a namespace",
			stubs: func(t *testing.T) {
				stubTools(t, map[string]string{
					"bwrap": "#!/bin/sh\necho 'bwrap: No permissions to create a new namespace'\nexit 1\n",
				})
			},
			wantName: "bubblewrap",
		},
		{
			name: "network namespace not permitted",
			stubs: func(t *testing.T) {
				stubTools(t, map[string]string{
					"bwrap":   "#!/bin/sh\nexit 0\n",
					"unshare": "#!/bin/sh\necho 'unshare: unshare failed: Operation not permitted'\nexit 1\n",
				})
			},
			wantName: "unshare --net",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.stubs(t)
			ok, reason := Detect(context.Background())
			if ok {
				t.Fatal("Detect reported a container is available")
			}
			if !strings.Contains(reason, tc.wantName) {
				t.Errorf("reason = %q, want it to name %q", reason, tc.wantName)
			}
		})
	}
}

// TestDetectAcceptsAWorkingHost is the other side: both probes succeed and
// detection says yes. Without it, a Detect that always returned false would pass
// every refusal test above.
func TestDetectAcceptsAWorkingHost(t *testing.T) {
	stubTools(t, map[string]string{
		"bwrap":   "#!/bin/sh\nexit 0\n",
		"unshare": "#!/bin/sh\nexit 0\n",
	})
	ok, reason := Detect(context.Background())
	if !ok {
		t.Fatalf("Detect said no: %s", reason)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty when the answer is yes", reason)
	}
}
