package main

// Self-update. When the binary lives inside a git checkout (the normal
// install), it checks origin/main on startup, every UPDATE_INTERVAL, and on
// POST /update. If main moved: pull --ff-only, go build to a temp file, swap
// it in, and re-exec once no request is in flight. The token cache and the
// service manager make this invisible: the new process comes up signed in.
// AUTO_UPDATE=off disables everything except the manual endpoint.

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Updater struct {
	cfg      Config
	repo     string // checkout directory ("" = not a git install)
	exe      string
	inflight atomic.Int64
	mu       sync.Mutex // one update at a time
}

func NewUpdater(cfg Config) *Updater {
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	u := &Updater{cfg: cfg, exe: exe}
	dir := cfg.RepoDir
	if dir == "" {
		dir = filepath.Dir(exe)
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		u.repo = strings.TrimSpace(string(out))
	}
	return u
}

func (u *Updater) git(args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", u.repo}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Check reports (localHead, remoteHead, updateAvailable).
func (u *Updater) Check() (string, string, bool, error) {
	if u.repo == "" {
		return "", "", false, nil
	}
	if out, err := u.git("fetch", "--quiet", "origin", "main"); err != nil {
		return "", "", false, errorf("git fetch: %s", out)
	}
	local, _ := u.git("rev-parse", "HEAD")
	remote, _ := u.git("rev-parse", "origin/main")
	return local, remote, local != remote && remote != "", nil
}

// Apply pulls, rebuilds, swaps the binary, and re-execs. Returns only on
// failure or when there was nothing to do.
func (u *Updater) Apply() (updated bool, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	local, remote, avail, err := u.Check()
	if err != nil || !avail {
		return false, err
	}
	log.Printf("update: %s -> %s", short(local), short(remote))
	if out, err := u.git("pull", "--ff-only", "--quiet", "origin", "main"); err != nil {
		return false, errorf("git pull: %s", out)
	}
	tmp := u.exe + ".new"
	goBin, err := findGo()
	if err != nil {
		return false, errorf("%v (set GO_BIN=/path/to/go/bin in the env file)", err)
	}
	build := exec.Command(goBin, "build", "-o", tmp, ".")
	build.Dir = u.repo
	// Make sure the toolchain can find itself even under a bare service PATH.
	build.Env = append(os.Environ(), "PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := build.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		// Roll the checkout back so the next attempt is clean.
		_, _ = u.git("reset", "--hard", local)
		return false, errorf("go build failed, rolled back to %s: %s", short(local), string(out))
	}
	if err := os.Rename(tmp, u.exe); err != nil {
		return false, errorf("swap binary: %v", err)
	}
	log.Printf("update: built %s, restarting when idle", short(remote))
	go u.restartWhenIdle()
	return true, nil
}

// restartWhenIdle waits (bounded) for in-flight requests to drain, then
// re-execs the new binary in place.
func (u *Updater) restartWhenIdle() {
	deadline := time.Now().Add(90 * time.Second)
	for u.inflight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("update: re-exec %s", u.exe)
	if err := reexec(u.exe); err != nil {
		log.Printf("update: re-exec failed (%v); exiting so the service manager restarts us", err)
		os.Exit(3)
	}
}

// Loop runs periodic checks.
func (u *Updater) Loop(ctx context.Context) {
	if u.repo == "" || !u.cfg.AutoUpdate {
		return
	}
	for {
		if _, err := u.Apply(); err != nil {
			log.Printf("update: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.cfg.UpdateInterval):
		}
	}
}

// track wraps a handler so restarts wait for it.
func (u *Updater) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.inflight.Add(1)
		defer u.inflight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// handler: GET = status, POST = check+apply now.
func (u *Updater) handler(w http.ResponseWriter, r *http.Request) {
	local, remote, avail, err := u.Check()
	resp := map[string]any{"git_install": u.repo != "", "local": short(local), "remote": short(remote), "update_available": avail}
	if err != nil {
		resp["error"] = err.Error()
	}
	if r.Method == http.MethodPost && avail && err == nil {
		updated, err := u.Apply()
		resp["updated"] = updated
		if err != nil {
			resp["error"] = err.Error()
		}
	}
	writeJSON(w, 200, resp)
}

// findGo locates the go binary for self-update. Service managers (systemd,
// launchd, setsid/nohup) often start us with a bare PATH that lacks it.
// Order: GO_BIN env → PATH → GOROOT of the running binary → well-known dirs.
func findGo() (string, error) {
	exe := "go"
	if runtime.GOOS == "windows" {
		exe = "go.exe"
	}
	if d := os.Getenv("GO_BIN"); d != "" {
		for _, c := range []string{d, filepath.Join(d, exe)} {
			if isExec(c) {
				return c, nil
			}
		}
	}
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	if gr := runtime.GOROOT(); gr != "" && isExec(filepath.Join(gr, "bin", exe)) {
		return filepath.Join(gr, "bin", exe), nil
	}
	home, _ := os.UserHomeDir()
	for _, d := range []string{
		"/usr/local/go/bin", "/usr/lib/go/bin", "/usr/lib/go-1.22/bin", "/usr/lib/go-1.23/bin", "/usr/lib/go-1.24/bin",
		"/opt/homebrew/bin", "/usr/local/bin", "/snap/bin", "/usr/bin",
		filepath.Join(home, "go", "bin"), filepath.Join(home, ".go", "bin"), filepath.Join(home, "sdk"),
		`C:\Program Files\Go\bin`, `C:\Go\bin`,
	} {
		if isExec(filepath.Join(d, exe)) {
			return filepath.Join(d, exe), nil
		}
	}
	return "", errorf("go toolchain not found on PATH, GOROOT, or common install dirs")
}

func isExec(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errorf(format string, a ...any) error {
	return strErr(sprintf(format, a...))
}
