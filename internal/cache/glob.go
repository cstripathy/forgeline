package cache

import (
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// excludedDirs are never hashed. .forgeline holds the cache itself; including it would make
// a "**" pattern change the key of every step on every run.
var excludedDirs = map[string]bool{".forgeline": true}

// matchGlob reports whether the slash-separated relative path matches pattern. Syntax is
// described in docs/design/002-spec-extensions.md: path.Match rules per segment, "**" as a
// whole segment matches zero or more segments, and a pattern also matches everything below
// a directory it names.
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return true // pattern consumed; any remainder is below a matched directory
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}

// matchFiles returns the sorted workspace-relative paths of regular files that match any
// pattern. Symlinks and other non-regular files are skipped so a link cannot pull in files
// from outside the workspace.
func matchFiles(workspace string, patterns []string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(workspace, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if excludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		slash := filepath.ToSlash(rel)
		for _, pat := range patterns {
			if matchGlob(pat, slash) {
				out = append(out, slash)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
