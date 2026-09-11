// Package report persists per-file mutation results, per-package reports,
// the aggregated summary, and resume state (D8, D9). Every completed file
// is persisted immediately so an interrupted run loses at most one file.
package report

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileResult is the outcome of one file mutation.
type FileResult struct {
	Package    string    `json:"package"`
	File       string    `json:"file"`
	Result     string    `json:"result"` // config.Result* or "skipped"
	ExitCode   int       `json:"exit_code"`
	DurationMS int64     `json:"duration_ms"`
	StartedAt  time.Time `json:"started_at"`
}

// PackageReport aggregates one package's file results.
type PackageReport struct {
	Package string       `json:"package"`
	Files   []FileResult `json:"files"`
}

// Counts tallies results by class.
type Counts struct {
	Total      int   `json:"total"`
	Killed     int   `json:"killed"`
	Survived   int   `json:"survived"`
	BuildError int   `json:"build_error"`
	Timeout    int   `json:"timeout"`
	OOM        int   `json:"oom"`
	Error      int   `json:"error"`
	Skipped    int   `json:"skipped"`
	DurationMS int64 `json:"duration_ms"`
}

// Summary is the aggregated run summary rendered at the end (D9).
type Summary struct {
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Counts     Counts             `json:"counts"`
	Packages   map[string]*Counts `json:"packages"`
}

// MutationScore is killed / (killed + survived); NaN-safe: returns 0 when
// the denominator is zero.
func (c Counts) MutationScore() float64 {
	d := c.Killed + c.Survived
	if d == 0 {
		return 0
	}
	return float64(c.Killed) / float64(d)
}

func (c *Counts) add(result string, ms int64) {
	c.Total++
	switch result {
	case "killed":
		c.Killed++
	case "survived":
		c.Survived++
	case "build_error":
		c.BuildError++
	case "timeout":
		c.Timeout++
	case "oom":
		c.OOM++
	case "error":
		c.Error++
	case "skipped":
		c.Skipped++
	}
	c.DurationMS += ms
}

// writeLog writes a file's raw command output verbatim under dir,
// shared by both storage backends.
func writeLog(dir string, r FileResult, output string) error {
	p := filepath.Join(dir, slug(r.Package), slug(r.File)+".log")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(output), 0o644)
}

// rollupTotals sums package counts into a grand total, shared by both
// storage backends' Summarize.
func rollupTotals(packages map[string]*Counts) Counts {
	var t Counts
	for _, c := range packages {
		t.Total += c.Total
		t.Killed += c.Killed
		t.Survived += c.Survived
		t.BuildError += c.BuildError
		t.Timeout += c.Timeout
		t.OOM += c.OOM
		t.Error += c.Error
		t.Skipped += c.Skipped
		t.DurationMS += c.DurationMS
	}
	return t
}

// Store persists results and resume state. All methods must be safe for
// concurrent use.
type Store interface {
	// Record persists one completed file result (called after every
	// file, D8).
	Record(r FileResult) error

	// Log writes a file's raw command output verbatim.
	Log(r FileResult, output string) error

	// Resume returns package → file → result for every recorded file.
	Resume() (map[string]map[string]string, error)

	// Summarize aggregates all recorded results.
	Summarize(start, finish time.Time) (Summary, error)

	Close() error
}

// Render produces the human-readable end-of-run summary (D9).
func Render(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mutation run finished in %s\n", s.FinishedAt.Sub(s.StartedAt).Round(time.Second))
	fmt.Fprintf(&b, "  files:    %d\n", s.Counts.Total)
	fmt.Fprintf(&b, "  killed:   %d\n", s.Counts.Killed)
	fmt.Fprintf(&b, "  survived: %d\n", s.Counts.Survived)
	if s.Counts.BuildError+s.Counts.Timeout+s.Counts.OOM+s.Counts.Error+s.Counts.Skipped > 0 {
		fmt.Fprintf(&b, "  other:    %d build_error, %d timeout, %d oom, %d error, %d skipped\n",
			s.Counts.BuildError, s.Counts.Timeout, s.Counts.OOM, s.Counts.Error, s.Counts.Skipped)
	}
	fmt.Fprintf(&b, "  mutation score: %.1f%%\n", s.Counts.MutationScore()*100)

	names := make([]string, 0, len(s.Packages))
	for name := range s.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := s.Packages[name]
		fmt.Fprintf(&b, "  %-40s %4d files, score %.1f%%\n", name, c.Total, c.MutationScore()*100)
	}
	return b.String()
}
