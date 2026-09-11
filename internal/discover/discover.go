// Package discover walks the configured target roots and produces the
// ordered package queue: every directory containing the package marker,
// with the files selected for mutation and their resolved test selectors
// (D13).
package discover

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoLens-Project/golens-mutant/internal/config"
)

// File is one mutation candidate inside a package.
type File struct {
	// Rel is the package-relative path, slash-separated (the {file} macro).
	Rel string `json:"rel"`

	// Tests are the selectors substituted for {tests}; empty when the
	// file is skipped by the tests mapping fallback.
	Tests []string `json:"tests"`

	// Run is false when the file was skipped because no tests mapping
	// matched and tests_fallback is "skip".
	Run bool `json:"run"`
}

// Package is one discovered lib/package work unit.
type Package struct {
	// Name is the slash-separated path relative to the working
	// directory (the {package} macro), e.g. "libs/core".
	Name string `json:"name"`

	// Dir is the absolute package directory (the sync source).
	Dir string `json:"dir"`

	// Files are the mutation candidates, in walk order.
	Files []File `json:"files"`

	// Exempt is true when the package's name matched an exemption
	// pattern; it is never run, sandboxed, or recorded.
	Exempt bool `json:"exempt"`
}

// RunnableFiles returns the files that will actually be mutated.
func (p *Package) RunnableFiles() []File {
	var out []File
	for _, f := range p.Files {
		if f.Run {
			out = append(out, f)
		}
	}
	return out
}

// Discover walks cfg's target roots and returns the package queue in the
// configured order (D6/D10). Nested packages are honored: a file belongs
// to its innermost ancestor package. Packages with no runnable files are
// dropped.
func Discover(cfg *config.Config) ([]*Package, error) {
	marker := cfg.Mutation.PackageMarker
	byDir := map[string]*Package{}
	seenFiles := map[string]bool{} // absolute paths, dedupes overlapping roots
	var order []*Package

	for _, root := range cfg.Mutation.TargetRoots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				return nil
			}
			if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
				if _, seen := byDir[path]; !seen {
					p := &Package{Name: pkgName(root, absRoot, path), Dir: path}
					byDir[path] = p
					order = append(order, p)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}

		// Assign every file to its innermost ancestor package.
		err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if seenFiles[path] {
				return nil // already claimed by an earlier root (F4)
			}
			pkg := innermostPackage(filepath.Dir(path), absRoot, byDir)
			if pkg == nil {
				return nil
			}
			rel, err := filepath.Rel(pkg.Dir, path)
			if err != nil {
				return nil
			}
			rel = slash(rel)
			if !matchAny(cfg.Mutation.FilePatterns, rel) {
				return nil
			}
			if matchAny(cfg.Mutation.FileExcludePatterns, rel) {
				return nil
			}
			tests, run := cfg.TestsFor(rel)
			pkg.Files = append(pkg.Files, File{Rel: rel, Tests: tests, Run: run})
			seenFiles[path] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Drop packages with nothing to mutate.
	queue := order[:0]
	for _, p := range order {
		if len(p.Files) > 0 {
			queue = append(queue, p)
		}
	}

	if cfg.Scheduling.Order == "largest_first" {
		sort.SliceStable(queue, func(i, j int) bool {
			return len(queue[i].RunnableFiles()) > len(queue[j].RunnableFiles())
		})
	}
	return queue, nil
}

// MarkExempt marks every package whose Name matches one of patterns (same
// glob syntax as file_exclude_patterns) as exempt and returns the patterns
// that matched no package, so the caller can warn about likely typos.
// Exempt packages keep their files so a dry run can still show them.
func MarkExempt(pkgs []*Package, patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	matched := make([]bool, len(patterns))
	for _, p := range pkgs {
		for i, pat := range patterns {
			if config.MatchGlob(pat, p.Name) {
				p.Exempt = true
				matched[i] = true
			}
		}
	}
	var unmatched []string
	for i, pat := range patterns {
		if !matched[i] {
			unmatched = append(unmatched, pat)
		}
	}
	return unmatched
}

// innermostPackage finds the nearest ancestor directory of dir (inclusive)
// that is a discovered package, stopping at root.
func innermostPackage(dir, root string, byDir map[string]*Package) *Package {
	for {
		if p, ok := byDir[dir]; ok {
			return p
		}
		if dir == root || filepath.Dir(dir) == dir {
			return nil
		}
		dir = filepath.Dir(dir)
	}
}

func matchAny(patterns []string, rel string) bool {
	for _, pat := range patterns {
		if config.MatchGlob(pat, rel) {
			return true
		}
	}
	return false
}

func slash(p string) string { return strings.ReplaceAll(p, string(filepath.Separator), "/") }

// pkgName derives the package's display name: the path relative to its
// target root, prefixed with the configured root path itself (root
// "libs" → "libs/pa"; root "apps/libs" → "apps/libs/pa"; root "." →
// "libs/pa"). The full root path — not just its base name — keeps
// same-named roots from colliding (F4).
func pkgName(configuredRoot, absRoot, pkgDir string) string {
	root := filepath.Clean(configuredRoot)
	rel, err := filepath.Rel(absRoot, pkgDir)
	if err != nil {
		return slash(pkgDir)
	}
	if root == "." || root == string(filepath.Separator) {
		return slash(rel)
	}
	if rel == "." {
		return slash(root)
	}
	return slash(filepath.Join(root, rel))
}
