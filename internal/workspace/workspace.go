// Package workspace manages isolated per-package sandboxes (D16): the
// first slot to reach a package bootstraps its sandbox (source sync +
// dependency command); later slots reuse the prepared copy under
// exclusive access. Files are re-synced pristine from the source before
// each mutation so no mutation ever leaks into the next run.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/GoLens-Project/golens-mutant/internal/config"
	"github.com/GoLens-Project/golens-mutant/internal/discover"
)

const preparedMarker = ".mutant-prepared"

// Manager owns the sandbox cache keyed by package name.
type Manager struct {
	cfg *config.Config

	// bootMu serializes bootstrap commands across slots to avoid
	// dependency-cache locking (e.g. concurrent `flutter pub get`).
	bootMu sync.Mutex

	mu        sync.Mutex
	sandboxes map[string]*Sandbox
}

// Sandbox is a prepared per-package workspace directory.
type Sandbox struct {
	// Name is the package name.
	Name string

	// Dir is the absolute sandbox directory ({target_dir}).
	Dir string

	// SrcDir is the absolute source package directory.
	SrcDir string

	cfg *config.Config

	// bootMu is the manager-wide bootstrap lock, injected at creation.
	bootMu *sync.Mutex

	mu sync.Mutex // exclusive checkout (D16)

	// prepared is atomic so status can be read without contending with
	// the checkout lock (which is held through long bootstraps).
	prepared atomic.Bool
}

// NewManager creates the sandbox manager rooted at cfg's workspace dir.
func NewManager(cfg *config.Config) (*Manager, error) {
	m := &Manager{cfg: cfg, sandboxes: map[string]*Sandbox{}}
	return m, nil
}

// CacheRoot is the absolute directory holding all package sandboxes.
func (m *Manager) CacheRoot() string {
	abs, err := filepath.Abs(filepath.Join(m.cfg.Workspace.BaseDir, m.cfg.Workspace.CacheDir))
	if err != nil {
		return filepath.Join(m.cfg.Workspace.BaseDir, m.cfg.Workspace.CacheDir)
	}
	return abs
}

// SandboxPath returns the absolute sandbox directory a package would
// use, without creating anything (dry-run friendly).
func (m *Manager) SandboxPath(pkgName string) string {
	return filepath.Join(m.CacheRoot(), slug(pkgName))
}

// TryAcquire exclusively checks out a package's sandbox, preparing it
// (sync + bootstrap) on first use. It returns false when another slot
// currently holds the sandbox (D16).
func (m *Manager) TryAcquire(ctx context.Context, pkg *discover.Package) (*Sandbox, bool, error) {
	m.mu.Lock()
	s, ok := m.sandboxes[pkg.Name]
	if !ok {
		s = &Sandbox{
			Name:   pkg.Name,
			Dir:    filepath.Join(m.CacheRoot(), slug(pkg.Name)),
			SrcDir: pkg.Dir,
			cfg:    m.cfg,
			bootMu: &m.bootMu,
		}
		m.sandboxes[pkg.Name] = s
	}
	m.mu.Unlock()

	if !s.mu.TryLock() {
		return nil, false, nil
	}
	if err := s.prepare(ctx); err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	return s, true, nil
}

// PreparedCount reports how many distinct packages have a sandbox
// prepared this session (bootstrapped or reused from a previous run).
func (m *Manager) PreparedCount() int {
	m.mu.Lock()
	sandboxes := make([]*Sandbox, 0, len(m.sandboxes))
	for _, s := range m.sandboxes {
		sandboxes = append(sandboxes, s)
	}
	m.mu.Unlock()
	n := 0
	for _, s := range sandboxes {
		if s.prepared.Load() {
			n++
		}
	}
	return n
}

// IsPrepared reports whether pkgName's sandbox is ready, without
// contending with an in-flight checkout or bootstrap.
func (m *Manager) IsPrepared(pkgName string) bool {
	m.mu.Lock()
	s, ok := m.sandboxes[pkgName]
	m.mu.Unlock()
	return ok && s.prepared.Load()
}

// Release returns a sandbox to the pool.
func (s *Sandbox) Release() { s.mu.Unlock() }

// prepare makes the sandbox usable: full source sync on first ever use,
// bootstrap on first use in this session, and a re-sync when reusing a
// sandbox persisted by a previous run.
func (s *Sandbox) prepare(ctx context.Context) error {
	if s.prepared.Load() {
		return nil
	}
	marker := filepath.Join(s.Dir, preparedMarker)
	_, markerErr := os.Stat(marker)

	if os.IsNotExist(markerErr) {
		// First use ever: clean slate, sync, bootstrap.
		if err := os.RemoveAll(s.Dir); err != nil {
			return err
		}
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return err
		}
		if err := syncTree(s.SrcDir, s.Dir, s.cfg.Workspace.SyncExcludePatterns); err != nil {
			return fmt.Errorf("sync package %s: %w", s.Name, err)
		}
		if err := s.bootstrap(ctx); err != nil {
			return err
		}
		if err := os.WriteFile(marker, []byte(s.Name+"\n"), 0o644); err != nil {
			return err
		}
	} else {
		// Sandbox persisted by a previous run: re-sync sources (no
		// re-bootstrap) so changed files are picked up and any leaked
		// mutations overwritten.
		if err := syncTree(s.SrcDir, s.Dir, s.cfg.Workspace.SyncExcludePatterns); err != nil {
			return fmt.Errorf("resync package %s: %w", s.Name, err)
		}
	}
	s.prepared.Store(true)
	return nil
}

// bootstrap runs the configured dependency command inside the sandbox,
// serialized across all sandboxes to avoid dependency-cache locking.
// An empty bootstrap command is a no-op.
func (s *Sandbox) bootstrap(ctx context.Context) error {
	cmdline := strings.TrimSpace(config.Interpolate(s.cfg.Workspace.Bootstrap, config.Vars{
		TargetDir: s.Dir,
		Package:   s.Name,
	}))
	if cmdline == "" {
		return nil
	}
	s.bootMu.Lock()
	defer s.bootMu.Unlock()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = s.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootstrap %q in %s: %w\n%s", cmdline, s.Dir, err, out)
	}
	return nil
}

// FilePath is the absolute sandbox path of a package-relative file.
func (s *Sandbox) FilePath(rel string) string { return filepath.Join(s.Dir, filepath.FromSlash(rel)) }

// RestorePristine re-copies one source file into the sandbox so the next
// mutation starts from unmutated content.
func (s *Sandbox) RestorePristine(rel string) error {
	src := filepath.Join(s.SrcDir, filepath.FromSlash(rel))
	dst := s.FilePath(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyFile(src, dst)
}

// syncTree recursively copies src to dst, skipping paths matched by the
// exclude patterns (a slash-less pattern matches any one path segment;
// a pattern with slashes is matched against the package-relative path).
func syncTree(src, dst string, excludes []string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excluded(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, devices, etc.
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if sameFile(path, target, info) {
			return nil
		}
		return copyFile(path, target)
	})
}

func excluded(rel string, excludes []string) bool {
	for _, pat := range excludes {
		if !strings.Contains(pat, "/") {
			if slices.Contains(strings.Split(rel, "/"), pat) {
				return true
			}
		} else if config.MatchGlob(pat, rel) {
			return true
		}
	}
	return false
}

// sameFile reports whether dst already holds byte-identical content,
// making re-syncs cheap. Content (not mtime) is compared so a leaked
// mutation from an interrupted run is never mistaken for a synced file.
func sameFile(src, dst string, srcInfo os.FileInfo) bool {
	dstInfo, err := os.Stat(dst)
	if err != nil || dstInfo.IsDir() || dstInfo.Size() != srcInfo.Size() {
		return false
	}
	a, err1 := os.ReadFile(src)
	b, err2 := os.ReadFile(dst)
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".mutant-tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func slug(name string) string {
	return strings.ReplaceAll(strings.Trim(name, "/"), "/", "__")
}
