//go:build unix

// internal/gitops/procgroup_unix.go
package gitops

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts cmd in a process group of its own and makes context
// cancellation kill the whole group rather than just cmd's own pid.
//
// ▶ Why: a git command is rarely one process. `worktree add` runs
// `reset --hard`, which under blob:none runs `fetch` → `remote-https` →
// `fetch-pack` → `index-pack` to lazily pull blobs. exec.CommandContext's
// default Cancel SIGKILLs only the top one; the rest are re-parented (to the
// extractor itself, which is PID 1 in the container) and keep downloading
// long after the request's deadline. Seen for real on 2026-10-18: a 300s
// timeout that left git fetching for 9+ minutes. Killing -pgid takes the tree.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
