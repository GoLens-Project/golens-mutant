package report

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// ReadResume loads persisted resume state without creating anything —
// dry-run friendly. Missing state (no file / no database yet) returns an
// empty map, not an error.
func ReadResume(storage, reportsDir, sqlitePath string) (map[string]map[string]string, error) {
	if storage == "sqlite" {
		return readSQLiteResume(sqlitePath)
	}
	return readJSONResume(filepath.Join(reportsDir, "resume-state.json"))
}

func readJSONResume(path string) (map[string]map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state map[string]map[string]string
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return state, nil
}

func readSQLiteResume(path string) (map[string]map[string]string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return map[string]map[string]string{}, nil
	} else if err != nil {
		return nil, err
	}
	// mode=ro via a file: URI — the modernc driver only honors URI
	// parameters with the prefix, so a bare "?mode=ro" would open
	// read-write and recover/checkpoint the WAL in place. The three
	// characters special to the DSN are percent-encoded; slashes stay
	// literal ('#' would truncate the URI and silently drop mode=ro).
	dsn := "file:" + uriEscapePath(path) + "?mode=ro"
	state, err := openSQLiteResume(dsn)
	if err == nil {
		return state, nil
	}
	// A read-only open fails when a hot WAL journal remains from an
	// unclean close — recovery needs write access. Recover on a
	// throwaway copy so the original (and its sidecar files) stay
	// untouched.
	if _, walErr := os.Stat(path + "-wal"); walErr == nil {
		return readSQLiteResumeFromCopy(path)
	}
	return nil, err
}

func openSQLiteResume(dsn string) (map[string]map[string]string, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return scanResume(db)
}

// uriEscapePath percent-encodes the characters that would otherwise be
// parsed as DSN structure: '%' (introduces an escape), '?' (starts the
// query), and '#' (starts a fragment, truncating everything after it).
func uriEscapePath(path string) string {
	return strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
	).Replace(path)
}

// readSQLiteResumeFromCopy copies the database and its WAL sidecars to a
// temp dir, opens the copy read-write (letting SQLite recover the WAL
// there), and reads the resume state from it. The original files are
// never modified.
func readSQLiteResumeFromCopy(path string) (map[string]map[string]string, error) {
	tmp, err := os.MkdirTemp("", "mutant-resume-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	copied := filepath.Join(tmp, "mutant.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := path + suffix
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		b, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(copied+suffix, b, 0o644); err != nil {
			return nil, err
		}
	}
	return openSQLiteResume(copied)
}
