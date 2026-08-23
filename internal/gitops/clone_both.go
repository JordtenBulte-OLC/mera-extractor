// internal/gitops/clone_both.go
package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type CloneBothRequest struct {
	RepoURL  string
	Username string // "pat" for Mendix Team Server
	Pat      string
	BaseSha  string
	HeadSha  string
}

type CloneBothResult struct {
	WorkDir string `json:"workDir"` // caller is responsible for Cleanup(WorkDir)
	BaseDir string `json:"baseDir"` // worktree checked out at BaseSha
	HeadDir string `json:"headDir"` // worktree checked out at HeadSha
}

// cloneBothTimeout is generous compared with Clone's 120s: this does one
// fetch and two full checkouts of a Mendix app, and with a blob:none partial
// clone each `worktree add` pays its own lazy-blob round trip. Package-level
// so a test can shrink it.
var cloneBothTimeout = 300 * time.Second

// CloneBoth materialises two commits of the same repository side by side, so
// `mx diff` has a real base and a real head to compare.
//
// Layout under the returned WorkDir:
//
//	repo.git/   bare object store — the only thing that talks to the network
//	base/       worktree, detached at BaseSha
//	head/       worktree, detached at HeadSha
//
// The repo is bare on purpose. A worktree needs a real repository — a loose
// .mpr sitting in a directory is not enough, which cost this project a
// detour once already — but nothing ever reads the *main* working tree, and
// a bare repo means there isn't one to leave empty or to accidentally read
// from. It also sidesteps `git worktree add` against an unborn HEAD.
func CloneBoth(ctx context.Context, workRoot string, req CloneBothRequest) (CloneBothResult, error) {
	if err := validateSha("baseSha", req.BaseSha); err != nil {
		return CloneBothResult{}, err
	}
	if err := validateSha("headSha", req.HeadSha); err != nil {
		return CloneBothResult{}, err
	}

	workDir, err := os.MkdirTemp(workRoot, "clone-both-*")
	if err != nil {
		return CloneBothResult{}, fmt.Errorf("create workdir under %q: %w", workRoot, err)
	}
	fail := func(err error) (CloneBothResult, error) {
		os.RemoveAll(workDir)
		return CloneBothResult{}, redact(err, req.Pat)
	}

	repoDir := filepath.Join(workDir, "repo.git")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		return fail(fmt.Errorf("create repo dir: %w", err))
	}

	// The credential file sits beside the repo rather than inside it. With a
	// bare repo there is no working tree for it to pollute, and keeping the
	// secret out of anything git might enumerate or archive is free here.
	helperPath, err := writeCredentialHelper(workDir, req.RepoURL, req.Username, req.Pat)
	if err != nil {
		return fail(err)
	}
	if helperPath != "" {
		// Note the ordering: this must outlive the `worktree add` calls, not
		// just the fetch. Under --filter=blob:none the checkout itself is what
		// pulls the file contents, and that hits the network authenticated.
		defer os.Remove(helperPath)
	}

	ctx, cancel := context.WithTimeout(ctx, cloneBothTimeout)
	defer cancel()

	baseDir := filepath.Join(workDir, "base")
	headDir := filepath.Join(workDir, "head")

	steps := [][]string{
		{"init", "--bare"},
		{"remote", "add", "origin", req.RepoURL},
		// One fetch, both commits. Fetching by raw object name requires the
		// server to allow unadvertised wants; Team Server does, which is what
		// Clone has been relying on all along.
		{"fetch", "--filter=blob:none", "--no-tags", "origin", req.BaseSha, req.HeadSha},
		// --detach is explicit. Without it `worktree add <path> <commit-ish>`
		// will DWIM a branch named after the path's basename in some cases,
		// and we never want a branch here.
		{"worktree", "add", "--detach", baseDir, req.BaseSha},
		{"worktree", "add", "--detach", headDir, req.HeadSha},
	}
	for _, args := range steps {
		if err := runGit(ctx, repoDir, helperPath, args...); err != nil {
			return fail(err)
		}
	}

	// Fail here rather than three layers deep in mx.PrepareMpr, and say which
	// side is at fault while we still know.
	if _, err := FindMpr(baseDir); err != nil {
		return fail(fmt.Errorf("base worktree at %s: %w", req.BaseSha, err))
	}
	if _, err := FindMpr(headDir); err != nil {
		return fail(fmt.Errorf("head worktree at %s: %w", req.HeadSha, err))
	}

	return CloneBothResult{WorkDir: workDir, BaseDir: baseDir, HeadDir: headDir}, nil
}
