package report

import (
	"strings"
	"testing"
)

// TestUnwanted pins the closed label list and its matching rule: a label refuses
// when it is one of the three, whatever its case, and a label that merely begins
// with one of them is the repo's own vocabulary rather than a decision.
func TestUnwanted(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   []string
	}{
		{name: "no labels", labels: nil},
		{name: "unrelated labels", labels: []string{"bug", "enhancement"}},
		{name: "wontfix", labels: []string{"wontfix"}, want: []string{"wontfix"}},
		{name: "case is not a spelling", labels: []string{"WontFix", "bug"}, want: []string{"WontFix"}},
		{
			name:   "each of the three, in GitHub's order",
			labels: []string{"needs-discussion", "invalid", "wontfix"},
			want:   []string{"needs-discussion", "invalid", "wontfix"},
		},
		{name: "a prefix is not the label", labels: []string{"wontfix-candidate", "invalid-config"}},
		{name: "the repo's spelling is kept", labels: []string{"NEEDS-DISCUSSION"}, want: []string{"NEEDS-DISCUSSION"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unwanted(tc.labels)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("unwanted(%q) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}
