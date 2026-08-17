package fs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var errOutsideRoot = errors.New("path outside configured roots")

// Manager enforces access only to configured roots.
type Manager struct {
	roots []string
}

// NewManager accepts root directories, normalizes them to absolute, and deduplicates.
func NewManager(roots []string) (*Manager, error) {
	if len(roots) == 0 {
		return nil, errors.New("no roots provided")
	}

	unique := make(map[string]struct{})
	absRoots := make([]string, 0, len(roots))

	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		abs = filepath.Clean(abs)
		if _, exists := unique[abs]; exists {
			continue
		}
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return nil, err
			}
		}
		unique[abs] = struct{}{}
		absRoots = append(absRoots, abs)
	}

	sort.Strings(absRoots)

	return &Manager{roots: absRoots}, nil
}

// Roots returns the configured roots in sorted order.
func (m *Manager) Roots() []string {
	return append([]string(nil), m.roots...)
}

// IsAllowed reports whether the provided path is inside any configured root.
func (m *Manager) IsAllowed(path string) bool {
	return m.isAllowed(path)
}

// Resolve joins rel with a root and ensures the resulting absolute path stays inside the root set.
func (m *Manager) Resolve(rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) {
		if m.isAllowed(cleaned) {
			return cleaned, nil
		}
		return "", errOutsideRoot
	}

	for _, root := range m.roots {
		candidate := filepath.Join(root, cleaned)
		if m.isAllowed(candidate) {
			return candidate, nil
		}
	}

	return "", errOutsideRoot
}

func (m *Manager) isAllowed(path string) bool {
	checked := filepath.Clean(path)
	for _, root := range m.roots {
		rel, err := filepath.Rel(root, checked)
		if err != nil {
			continue
		}
		if rel == "." || rel == "" {
			return true
		}
		if !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
			return true
		}
	}
	return false
}
