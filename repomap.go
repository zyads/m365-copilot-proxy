package main

// Repo map: a compact orientation snapshot (branch, language mix, tree,
// README head) injected as a Copilot context so the model knows the shape of
// the codebase before its first tool call — instead of burning three turns on
// `ls`. Root comes from REPO_ROOT, else is parsed out of the caller's system
// prompt (OpenCode and Claude Code both state their working directory there).

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, ".venv": true, "venv": true, "__pycache__": true, ".next": true,
	".cache": true, ".idea": true, ".vscode": true, "coverage": true, ".tox": true,
}

const (
	mapMaxDepth   = 3
	mapMaxEntries = 150
	mapReadmeHead = 25
	mapTTL        = 60 * time.Second
)

type repoMapEntry struct {
	text string
	at   time.Time
}

type RepoMapper struct {
	mu    sync.Mutex
	cache map[string]repoMapEntry
}

func NewRepoMapper() *RepoMapper { return &RepoMapper{cache: map[string]repoMapEntry{}} }

var cwdRe = regexp.MustCompile(`(?im)^\s*(?:primary )?(?:working directory|cwd|current directory|project root)\s*[:=]\s*` + "`?" + `(/[^\s` + "`" + `]+)`)

// detectRoot returns REPO_ROOT if set, else the first absolute path that the
// system prompt labels as a working directory, else "".
func detectRoot(cfg Config, system string) string {
	if cfg.RepoRoot != "" {
		return cfg.RepoRoot
	}
	if m := cwdRe.FindStringSubmatch(system); m != nil {
		if st, err := os.Stat(m[1]); err == nil && st.IsDir() {
			return m[1]
		}
	}
	return ""
}

// Context returns the repo map context for root, or nil when no root.
func (r *RepoMapper) Context(root string) *graphContext {
	if root == "" {
		return nil
	}
	r.mu.Lock()
	if e, ok := r.cache[root]; ok && time.Since(e.at) < mapTTL {
		r.mu.Unlock()
		return &graphContext{Description: "Repository map (auto-generated snapshot; may be slightly stale)", Text: e.text}
	}
	r.mu.Unlock()

	text := buildRepoMap(root)
	r.mu.Lock()
	r.cache[root] = repoMapEntry{text: text, at: time.Now()}
	r.mu.Unlock()
	return &graphContext{Description: "Repository map (auto-generated snapshot; may be slightly stale)", Text: text}
}

func buildRepoMap(root string) string {
	var sb strings.Builder
	sb.WriteString("Root: " + root + "\n")
	if br := gitBranch(root); br != "" {
		sb.WriteString("Git branch: " + br + "\n")
	}

	ext := map[string]int{}
	var lines []string
	total, truncated := 0, false
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if d.IsDir() {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			if depth > mapMaxDepth {
				return filepath.SkipDir
			}
		} else if e := strings.ToLower(filepath.Ext(d.Name())); e != "" {
			ext[e]++ // count every file for the language mix, even beyond the tree cap
		}
		if depth <= mapMaxDepth {
			if total < mapMaxEntries {
				name := rel
				if d.IsDir() {
					name += "/"
				}
				lines = append(lines, name)
			} else {
				truncated = true
			}
			total++
		}
		return nil
	})

	type kv struct {
		k string
		v int
	}
	var mix []kv
	for k, v := range ext {
		mix = append(mix, kv{k, v})
	}
	sort.Slice(mix, func(i, j int) bool { return mix[i].v > mix[j].v || (mix[i].v == mix[j].v && mix[i].k < mix[j].k) })
	if len(mix) > 8 {
		mix = mix[:8]
	}
	if len(mix) > 0 {
		var parts []string
		for _, m := range mix {
			parts = append(parts, m.k+"="+itoa(m.v))
		}
		sb.WriteString("Files by extension: " + strings.Join(parts, " ") + "\n")
	}

	sb.WriteString("\nTree (depth ≤ " + itoa(mapMaxDepth) + "):\n")
	for _, l := range lines {
		sb.WriteString("  " + l + "\n")
	}
	if truncated {
		sb.WriteString("  … (" + itoa(total-mapMaxEntries) + " more; use glob/grep tools)\n")
	}

	for _, name := range []string{"README.md", "README", "readme.md", "README.rst"} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			ls := strings.Split(string(b), "\n")
			if len(ls) > mapReadmeHead {
				ls = ls[:mapReadmeHead]
				ls = append(ls, "…")
			}
			sb.WriteString("\n" + name + " (head):\n" + strings.Join(ls, "\n") + "\n")
			break
		}
	}
	log.Printf("repomap: %s — %d entries, %d ext", root, total, len(ext))
	return sb.String()
}

func gitBranch(root string) string {
	b, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	return strings.TrimPrefix(s, "ref: refs/heads/")
}
