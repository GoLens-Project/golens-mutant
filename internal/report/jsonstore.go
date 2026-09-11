package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// JSONStore is the default storage backend (D9): per-package JSON
// reports, raw logs, resume state, and the summary under one directory.
type JSONStore struct {
	dir string

	mu      sync.Mutex
	results map[string][]FileResult // package → results, insertion order
}

// NewJSONStore creates the store rooted at dir.
func NewJSONStore(dir string) (*JSONStore, error) {
	for _, d := range []string{dir, filepath.Join(dir, "packages"), filepath.Join(dir, "logs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &JSONStore{dir: dir, results: map[string][]FileResult{}}, nil
}

// Record persists one file result: it upserts into the in-memory package
// report (a re-recorded file replaces its earlier entry rather than
// duplicating it), rewrites that package's JSON file, and updates the
// resume state immediately (D8).
func (s *JSONStore) Record(r FileResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := s.results[r.Package]
	replaced := false
	for i, prev := range files {
		if prev.File == r.File {
			files[i] = r
			replaced = true
			break
		}
	}
	if !replaced {
		s.results[r.Package] = append(files, r)
	}
	if err := writeJSON(packagePath(s.dir, r.Package), PackageReport{Package: r.Package, Files: s.results[r.Package]}); err != nil {
		return err
	}
	return s.writeResumeLocked()
}

// Log writes the raw command output verbatim as one text file per mutated
// file.
func (s *JSONStore) Log(r FileResult, output string) error {
	return writeLog(filepath.Join(s.dir, "logs"), r, output)
}

// Resume reads the persisted per-file state left by an interrupted run.
func (s *JSONStore) Resume() (map[string]map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readResumeLocked()
}

// Summarize aggregates all results recorded so far.
func (s *JSONStore) Summarize(start, finish time.Time) (Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := Summary{StartedAt: start, FinishedAt: finish, Packages: map[string]*Counts{}}
	for pkg, files := range s.results {
		c := &Counts{}
		for _, f := range files {
			c.add(f.Result, f.DurationMS)
		}
		sum.Packages[pkg] = c
	}
	sum.Counts = rollupTotals(sum.Packages)
	if err := writeJSON(filepath.Join(s.dir, "summary.json"), sum); err != nil {
		return sum, err
	}
	return sum, nil
}

func (s *JSONStore) writeResumeLocked() error {
	state := map[string]map[string]string{}
	for pkg, files := range s.results {
		m := map[string]string{}
		for _, f := range files {
			m[f.File] = f.Result
		}
		state[pkg] = m
	}
	return writeJSON(filepath.Join(s.dir, "resume-state.json"), state)
}

func (s *JSONStore) readResumeLocked() (map[string]map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "resume-state.json"))
	if os.IsNotExist(err) {
		return map[string]map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state map[string]map[string]string
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	// Seed the in-memory results from persisted package reports so a
	// resumed session's summary includes earlier files. Seeding is
	// idempotent: packages already in memory (from Record calls or a
	// previous Resume) are left alone.
	for pkg := range state {
		if _, exists := s.results[pkg]; exists {
			continue
		}
		var pr PackageReport
		b, err := os.ReadFile(packagePath(s.dir, pkg))
		if err == nil && json.Unmarshal(b, &pr) == nil {
			s.results[pkg] = append(s.results[pkg], pr.Files...)
		}
	}
	return state, nil
}

// Close is a no-op: the JSON store needs no teardown.
func (s *JSONStore) Close() error { return nil }

func packagePath(dir, pkg string) string {
	return filepath.Join(dir, "packages", slug(pkg)+".json")
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func slug(name string) string {
	return strings.ReplaceAll(strings.Trim(name, "/"), "/", "__")
}
