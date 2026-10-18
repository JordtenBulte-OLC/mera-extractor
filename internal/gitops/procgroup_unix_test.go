//go:build unix

// internal/gitops/procgroup_unix_test.go
package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out git must take its whole process tree down with it, and runGit
// must return promptly. Reproduces the 2026-10-18 incident's shape: the
// top-level git spawns a long-lived child that inherits stdout/stderr (as
// `worktree add` → `reset --hard` → `fetch` does), so killing only the
// parent would leave both a running child and a Wait blocked on its pipes.
func TestRunGit_TimeoutKillsTheWholeTree(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "child.pid")
	fake := "#!/bin/sh\nsleep 60 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Long enough that passing proves the group kill, not the backstop.
	old := gitWaitDelay
	gitWaitDelay = 30 * time.Second
	t.Cleanup(func() { gitWaitDelay = old })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runGit(ctx, t.TempDir(), "", "fetch")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("runGit took %s after a 300ms deadline; the child's pipes kept it waiting", elapsed)
	}

	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake git never started its child: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	// The killed child may sit briefly as a zombie until its new parent reaps
	// it; signal 0 succeeds on a zombie, so poll /proc state where available.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if !alive(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild pid %d still running after the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true // no /proc (macOS): signal 0 succeeded, call it alive
	}
	// Field 3 is the state; "Z" is a zombie, i.e. already dead.
	f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(f) == 0 || f[0] != "Z"
}
