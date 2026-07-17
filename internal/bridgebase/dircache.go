package bridgebase

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// workspaceCacheTTL bounds how long a workspace subdir scan stays cached.
// Subdirectories change rarely (a user adds a project occasionally), so a
// short TTL keeps the picker fresh without forking a scan on every /cd.
const workspaceCacheTTL = 60 * time.Second

// DirCache holds a cached snapshot of a workspace root's immediate
// subdirectories, shared by every backend bridge's /cd picker.
type DirCache struct {
	root      string
	mu        sync.Mutex
	dirs      []string
	fetchedAt time.Time
}

// NewDirCache builds a cache over root. An empty root disables the picker:
// List then errors so the caller surfaces "not configured" to the user.
func NewDirCache(root string) *DirCache {
	return &DirCache{root: root}
}

// List returns the absolute paths of immediate subdirectories under the
// configured root, sorted by name. Results are cached for workspaceCacheTTL.
func (c *DirCache) List() ([]string, error) {
	if c.root == "" {
		return nil, fmt.Errorf("未配置 WORKSPACE_ROOT 环境变量")
	}
	now := time.Now()
	c.mu.Lock()
	if c.dirs != nil && now.Sub(c.fetchedAt) < workspaceCacheTTL {
		out := c.dirs
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, fmt.Errorf("读取 workspace 目录失败：%w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(c.root, e.Name()))
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return filepath.Base(dirs[i]) < filepath.Base(dirs[j])
	})
	snapshot := make([]string, len(dirs))
	copy(snapshot, dirs)
	c.mu.Lock()
	c.dirs, c.fetchedAt = snapshot, time.Now()
	c.mu.Unlock()
	return dirs, nil
}

// Validate checks that dir is an immediate or nested subdirectory of the
// cache's root, refusing escapes. An empty root refuses everything (the
// operator has not opted into /cd selection). The check uses filepath.Rel:
// a result starting with ".." escapes the root.
func (c *DirCache) Validate(dir string) error {
	if c.root == "" {
		return fmt.Errorf("未配置 WORKSPACE_ROOT 环境变量，无法校验目录")
	}
	cleaned := filepath.Clean(dir)
	root := filepath.Clean(c.root)
	rel, err := filepath.Rel(root, cleaned)
	if err != nil {
		return fmt.Errorf("目录不在 workspace 范围内：%s", dir)
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("目录不在 workspace 范围内（%s 不在 %s 下）：%s", dir, root, dir)
	}
	return nil
}
