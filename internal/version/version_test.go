package version

import (
	"strings"
	"testing"
)

// TestStringReportsCommittedVersion guards the embed: the committed
// VERSION file — "dev" until the release workflow's first bump — is the
// single source of truth, and its value must surface un-padded.
func TestStringReportsCommittedVersion(t *testing.T) {
	got := String()
	if got == "" {
		t.Fatal("version is empty")
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("version = %q, want no surrounding whitespace", got)
	}
}
