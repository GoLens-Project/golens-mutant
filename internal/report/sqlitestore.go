package report

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore is the alternative storage backend (D9). Results live in
// one database file; raw logs stay on disk next to it, shared shape with
// the JSON store's logs/ directory.
type SQLiteStore struct {
	db  *sql.DB
	dir string // logs directory

	mu sync.Mutex
}

// NewSQLiteStore opens (creating if needed) the database at path. An empty
// path falls back to the default ~/.config/golens-mutant location
// (resolved by the caller via config).
func NewSQLiteStore(path, logsDir string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// file: URI form (same escaping as the read-only path in
	// resumeread.go): modernc/sqlite only honors _pragma parameters with
	// the prefix, and the path itself must be escaped so '%', '?', or
	// '#' cannot corrupt the DSN.
	db, err := sql.Open("sqlite", "file:"+uriEscapePath(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite is safest with a single writer connection.
	db.SetMaxOpenConns(1)
	const schema = `
CREATE TABLE IF NOT EXISTS file_results (
	pkg        TEXT NOT NULL,
	file       TEXT NOT NULL,
	result     TEXT NOT NULL,
	exit_code  INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	started_at TIMESTAMP NOT NULL,
	PRIMARY KEY (pkg, file)
);
CREATE TABLE IF NOT EXISTS runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	started_at TIMESTAMP NOT NULL,
	finished_at TIMESTAMP,
	summary TEXT
);`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init sqlite schema: %w", err)
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db, dir: logsDir}, nil
}

// Record upserts one file result (D8: persisted immediately; last write
// wins on retry).
func (s *SQLiteStore) Record(r FileResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO file_results (pkg, file, result, exit_code, duration_ms, started_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(pkg, file) DO UPDATE SET result=excluded.result, exit_code=excluded.exit_code,
	duration_ms=excluded.duration_ms, started_at=excluded.started_at`,
		r.Package, r.File, r.Result, r.ExitCode, r.DurationMS, r.StartedAt)
	return err
}

// Log writes raw output verbatim, same layout as the JSON store.
func (s *SQLiteStore) Log(r FileResult, output string) error {
	return writeLog(s.dir, r, output)
}

// Resume returns package → file → result for all recorded files.
func (s *SQLiteStore) Resume() (map[string]map[string]string, error) {
	return scanResume(s.db)
}

// scanResume reads the file_results table into a resume map.
func scanResume(db *sql.DB) (map[string]map[string]string, error) {
	rows, err := db.Query(`SELECT pkg, file, result FROM file_results`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	state := map[string]map[string]string{}
	for rows.Next() {
		var pkg, file, result string
		if err := rows.Scan(&pkg, &file, &result); err != nil {
			return nil, err
		}
		if state[pkg] == nil {
			state[pkg] = map[string]string{}
		}
		state[pkg][file] = result
	}
	return state, rows.Err()
}

// Summarize aggregates all recorded results and records the run.
func (s *SQLiteStore) Summarize(start, finish time.Time) (Summary, error) {
	rows, err := s.db.Query(`SELECT pkg, result, duration_ms FROM file_results`)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	sum := Summary{StartedAt: start, FinishedAt: finish, Packages: map[string]*Counts{}}
	for rows.Next() {
		var pkg, result string
		var ms int64
		if err := rows.Scan(&pkg, &result, &ms); err != nil {
			return sum, err
		}
		if sum.Packages[pkg] == nil {
			sum.Packages[pkg] = &Counts{}
		}
		sum.Packages[pkg].add(result, ms)
	}
	if err := rows.Err(); err != nil {
		return sum, err
	}
	sum.Counts = rollupTotals(sum.Packages)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO runs (started_at, finished_at, summary) VALUES (?, ?, ?)`,
		start, finish, Render(sum))
	return sum, err
}

// Close releases the database handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }
