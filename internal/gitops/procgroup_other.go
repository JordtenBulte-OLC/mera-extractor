//go:build !unix

// internal/gitops/procgroup_other.go
package gitops

import "os/exec"

// ownProcessGroup is a no-op off unix: the service only ships in a Linux
// container, and this keeps `go build` working on a Windows dev machine.
// WaitDelay (see newGitCmd) still stops a handler hanging on orphaned
// children's pipes; it just can't stop the children themselves.
func ownProcessGroup(cmd *exec.Cmd) {}
