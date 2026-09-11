package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkResults() []FileResult {
	base := time.Now()
	return []FileResult{
		{Package: "libs/a", File: "lib/one.dart", Result: "killed", ExitCode: 1, DurationMS: 100, StartedAt: base},
		{Package: "libs/a", File: "lib/two.dart", Result: "survived", DurationMS: 200, StartedAt: base},
		{Package: "libs/b", File: "lib/one.dart", Result: "timeout", DurationMS: 300, StartedAt: base},
	}
}

func TestJSONStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mkResults() {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
		if err := s.Log(r, "raw output for "+r.File); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.Summarize(time.Now().Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts.Total != 3 || sum.Counts.Killed != 1 || sum.Counts.Survived != 1 || sum.Counts.Timeout != 1 {
		t.Errorf("counts = %+v", sum.Counts)
	}
	if got := sum.Packages["libs/a"].MutationScore(); got != 0.5 {
		t.Errorf("libs/a score = %v, want 0.5", got)
	}

	// Per-package report file exists.
	if _, err := os.Stat(filepath.Join(dir, "packages", "libs__a.json")); err != nil {
		t.Errorf("package report missing: %v", err)
	}
	// Raw log captured verbatim.
	b, err := os.ReadFile(filepath.Join(dir, "logs", "libs__a", "lib__one.dart.log"))
	if err != nil || string(b) != "raw output for lib/one.dart" {
		t.Errorf("log = %q, %v", b, err)
	}
	// Summary file exists.
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
		t.Errorf("summary missing: %v", err)
	}
}

func TestJSONStoreResumeSeedsPreviousResults(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewJSONStore(dir)
	for _, r := range mkResults() {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}

	// A fresh store over the same directory (simulated new session) must
	// see the previous state.
	s2, _ := NewJSONStore(dir)
	state, err := s2.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if state["libs/a"]["lib/one.dart"] != "killed" {
		t.Errorf("resume state = %+v", state)
	}
	sum, err := s2.Summarize(time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts.Total != 3 {
		t.Errorf("resumed summary total = %d, want 3", sum.Counts.Total)
	}
}

func TestRender(t *testing.T) {
	s, _ := NewJSONStore(t.TempDir())
	for _, r := range mkResults() {
		s.Record(r)
	}
	sum, _ := s.Summarize(time.Now().Add(-time.Minute), time.Now())
	out := Render(sum)
	for _, want := range []string{"killed:", "survived:", "mutation score: 50.0%", "libs/a"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestSQLiteStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "mutant.db"), filepath.Join(dir, "logs"))
	if err != nil {
		t.Skipf("sqlite unavailable in this environment: %v", err)
	}
	defer s.Close()
	for _, r := range mkResults() {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if state["libs/b"]["lib/one.dart"] != "timeout" {
		t.Errorf("resume = %+v", state)
	}
	sum, err := s.Summarize(time.Now().Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts.Total != 3 || sum.Counts.Killed != 1 {
		t.Errorf("counts = %+v", sum.Counts)
	}
}

func TestNewJSONStoreError(t *testing.T) {
	// A file where a directory is expected: MkdirAll must fail.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJSONStore(filepath.Join(f, "reports")); err == nil {
		t.Error("NewJSONStore under a file path unexpectedly succeeded")
	}
}

func TestRecordFailsWhenUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission checks do not apply")
	}
	dir := t.TempDir()
	s, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(mkResults()[0]); err != nil {
		t.Fatal(err)
	}
	// Make the packages directory unwritable: the next Record must
	// surface the write failure instead of losing it silently.
	if err := os.Chmod(filepath.Join(dir, "packages"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "packages"), 0o755)
	if err := s.Record(mkResults()[1]); err == nil {
		t.Error("Record unexpectedly succeeded with unwritable packages dir")
	}
	// Packages dir writable again, but the store root is not: the
	// package report writes, and the resume-state write must fail.
	if err := os.Chmod(filepath.Join(dir, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := s.Record(mkResults()[1]); err == nil {
		t.Error("Record unexpectedly succeeded with unwritable store root")
	}
	// Log needs to create a directory inside logs/, so make that
	// unwritable too.
	if err := os.Chmod(filepath.Join(dir, "logs"), 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "logs"), 0o755)
	if err := s.Log(mkResults()[1], "x"); err == nil {
		t.Error("Log unexpectedly succeeded with unwritable logs dir")
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

func TestResumeCorruptStateIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume-state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(); err == nil {
		t.Error("Resume on corrupt state unexpectedly succeeded")
	}
}

func TestResumeWithMissingPackageReport(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume-state.json"),
		[]byte(`{"libs/gone": {"lib/a.dart": "killed"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewJSONStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Resume()
	if err != nil {
		t.Fatalf("Resume with missing package report: %v", err)
	}
	if state["libs/gone"]["lib/a.dart"] != "killed" {
		t.Errorf("state = %+v", state)
	}
	// The package report file is gone: seeding skips it without error.
	sum, err := s.Summarize(time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts.Total != 0 {
		t.Errorf("total = %d, want 0 (nothing seedable)", sum.Counts.Total)
	}
}

func TestAllResultClasses(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewJSONStore(dir)
	base := time.Now()
	classes := []string{"killed", "survived", "build_error", "timeout", "oom", "error", "skipped"}
	for i, class := range classes {
		r := FileResult{
			Package: "libs/all", File: "lib/f" + string(rune('0'+i)) + ".dart",
			Result: class, DurationMS: 10, StartedAt: base,
		}
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
		if err := s.Log(r, "out "+class); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := s.Summarize(base, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	c := sum.Packages["libs/all"]
	if c.Total != len(classes) || c.Killed != 1 || c.Survived != 1 || c.BuildError != 1 ||
		c.Timeout != 1 || c.OOM != 1 || c.Error != 1 || c.Skipped != 1 {
		t.Errorf("counts = %+v", c)
	}
	if c.DurationMS != int64(len(classes)*10) {
		t.Errorf("duration = %d", c.DurationMS)
	}
	out := Render(sum)
	for _, want := range []string{"build_error,", "timeout,", "oom,", "error,", "skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestSQLiteStoreErrorsAndLog(t *testing.T) {
	// A file where the database's parent directory would be: creation
	// must fail.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSQLiteStore(filepath.Join(f, "sub", "mutant.db"), t.TempDir()); err == nil {
		t.Error("NewSQLiteStore under a file path unexpectedly succeeded")
	}
	// Logs directory under a file path: the logs MkdirAll must fail.
	if _, err := NewSQLiteStore(filepath.Join(t.TempDir(), "mutant.db"), filepath.Join(f, "logs")); err == nil {
		t.Error("NewSQLiteStore with unwritable logs dir unexpectedly succeeded")
	}
	// Database path that is a directory: schema creation must fail.
	if _, err := NewSQLiteStore(t.TempDir(), t.TempDir()); err == nil {
		t.Error("NewSQLiteStore on a directory path unexpectedly succeeded")
	}

	dir := t.TempDir()
	s, err := NewSQLiteStore(filepath.Join(dir, "mutant.db"), filepath.Join(dir, "logs"))
	if err != nil {
		t.Skipf("sqlite unavailable in this environment: %v", err)
	}
	defer s.Close()
	r := mkResults()[0]
	if err := s.Record(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Log(r, "verbatim output"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "logs", "libs__a", "lib__one.dart.log"))
	if err != nil || string(b) != "verbatim output" {
		t.Errorf("log = %q, %v", b, err)
	}
}

func TestReadResumeSideEffectFree(t *testing.T) {
	t.Run("json missing file", func(t *testing.T) {
		dir := t.TempDir()
		state, err := ReadResume("json", dir, "")
		if err != nil || len(state) != 0 {
			t.Errorf("ReadResume(missing) = %v, %v; want empty, nil", state, err)
		}
		// Must not have created the state file (the temp dir itself
		// exists by construction — assert the file, not the dir).
		if _, err := os.Stat(filepath.Join(dir, "resume-state.json")); !os.IsNotExist(err) {
			t.Error("ReadResume created the resume-state file")
		}
	})

	t.Run("json present", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "resume-state.json"),
			[]byte(`{"libs/a": {"lib/x.dart": "killed"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		state, err := ReadResume("json", dir, "")
		if err != nil || state["libs/a"]["lib/x.dart"] != "killed" {
			t.Errorf("ReadResume = %v, %v", state, err)
		}
	})

	t.Run("sqlite missing file", func(t *testing.T) {
		db := filepath.Join(t.TempDir(), "absent.db")
		state, err := ReadResume("sqlite", "", db)
		if err != nil || len(state) != 0 {
			t.Errorf("ReadResume(sqlite missing) = %v, %v; want empty, nil", state, err)
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Error("ReadResume created the sqlite database")
		}
	})

	t.Run("sqlite present", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "mutant.db")
		s, err := NewSQLiteStore(db, filepath.Join(dir, "logs"))
		if err != nil {
			t.Skipf("sqlite unavailable: %v", err)
		}
		if err := s.Record(mkResults()[0]); err != nil {
			t.Fatal(err)
		}
		s.Close()
		// A clean close checkpoints the WAL away; the read-only open
		// must succeed without touching the database.
		before, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		state, err := ReadResume("sqlite", "", db)
		if err != nil || state["libs/a"]["lib/one.dart"] != "killed" {
			t.Errorf("ReadResume = %v, %v", state, err)
		}
		after, _ := os.ReadFile(db)
		if !bytes.Equal(before, after) {
			t.Error("ReadResume modified the database file")
		}
	})

	t.Run("sqlite special-path dsn", func(t *testing.T) {
		// Review blocker: a sqlite_path containing DSN-significant
		// characters must still open read-only. '%', '?', and '#' would
		// corrupt a raw-concatenated file: URI ('#' also drops mode=ro).
		for _, special := range []string{"50%.db", "what?.db", "frag#ment.db"} {
			db := filepath.Join(t.TempDir(), special)
			s, err := NewSQLiteStore(db, filepath.Join(t.TempDir(), "logs"))
			if err != nil {
				t.Skipf("sqlite unavailable: %v", err)
			}
			if err := s.Record(mkResults()[0]); err != nil {
				t.Fatal(err)
			}
			s.Close()
			state, err := ReadResume("sqlite", "", db)
			if err != nil || state["libs/a"]["lib/one.dart"] != "killed" {
				t.Errorf("ReadResume(%q) = %v, %v", special, state, err)
			}
		}
	})

	t.Run("sqlite hot wal", func(t *testing.T) {
		dir := t.TempDir()
		db := filepath.Join(dir, "mutant.db")
		s, err := NewSQLiteStore(db, filepath.Join(dir, "logs"))
		if err != nil {
			t.Skipf("sqlite unavailable: %v", err)
		}
		if err := s.Record(mkResults()[0]); err != nil {
			t.Fatal(err)
		}
		// Simulate an unclean close: snapshot db + hot WAL sidecars
		// while the connection is still open, then throw the original
		// away. The snapshot needs WAL recovery.
		snap := filepath.Join(t.TempDir(), "crash.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			b, err := os.ReadFile(db + suffix)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snap+suffix, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(snap + "-wal"); err != nil {
			t.Skip("no WAL sidecar materialized; store checkpointed early")
		}
		s.Close()

		before, _ := os.ReadFile(snap)
		state, err := ReadResume("sqlite", "", snap)
		if err != nil {
			t.Fatalf("ReadResume over hot WAL = %v, want recovery-on-copy", err)
		}
		if state["libs/a"]["lib/one.dart"] != "killed" {
			t.Errorf("recovered state = %v", state)
		}
		// The original crash snapshot must be untouched.
		after, _ := os.ReadFile(snap)
		if !bytes.Equal(before, after) {
			t.Error("WAL recovery modified the original database")
		}
	})

	t.Run("json corrupt", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "resume-state.json"), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadResume("json", dir, ""); err == nil {
			t.Error("ReadResume on corrupt state unexpectedly succeeded")
		}
	})
}

func TestCountsScore(t *testing.T) {
	if got := (Counts{}).MutationScore(); got != 0 {
		t.Errorf("empty score = %v, want 0", got)
	}
	if got := (Counts{Killed: 3, Survived: 1}).MutationScore(); got != 0.75 {
		t.Errorf("score = %v, want 0.75", got)
	}
}
