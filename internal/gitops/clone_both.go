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
	RepoDir string `json:"repoDir"` // the bare object store; what TextDiffs runs against
	BaseDir string `json:"baseDir"` // worktree checked out at BaseSha
	HeadDir string `json:"headDir"` // worktree checked out at HeadSha
}

// cloneBothTimeout is generous compared with Clone's 120s: this does one
// fetch, two model-only checkouts and one text-diff prefetch, each lazy-blob
// step its own network round trip. Package-level so a test can shrink it;
// SetCloneBothTimeout is the startup knob (MERA_CLONE_TIMEOUT).
var cloneBothTimeout = 300 * time.Second

// SetCloneBothTimeout replaces the CloneBoth deadline. Call once at startup,
// before serving. Whatever calls /extract must wait LONGER than this, or it
// gives up before it can see the extractor's own clean timeout error.
func SetCloneBothTimeout(d time.Duration) {
	if d > 0 {
		cloneBothTimeout = d
	}
}

// modelPaths is the sparse-checkout set for both worktrees, in non-cone
// (gitignore-style) syntax: everything mx and mxcli read, and nothing else.
// mprcontents/ is the MPRv2 per-unit store; absent on MPRv1 apps, harmless.
//
// ▶ Why not javasource/, theme/ etc. as well: nothing reads them from a
// worktree. TextDiffs works on the object store, and needs only the blobs
// that CHANGED — prefetchTextDiffBlobs pulls exactly those. Checking the
// folders out would download every unchanged Java file, every jar in
// userlib/ and every widget .mpk, twice, which is what a marketplace-module
// update turned into a 300s timeout on 2026-10-18.
//
// UNVERIFIED against a real app beyond the .mpr itself: if mx or mxcli turns
// out to read something else from the project directory (widgets/*.mpk is
// the likeliest candidate), it fails loudly in PrepareMpr/Diff/Describe, and
// the fix is one more entry here.
var modelPaths = []string{"*.mpr", "mprcontents/"}

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

	type step struct {
		dir  string
		args []string
	}
	steps := []step{
		{repoDir, []string{"init", "--bare"}},
		{repoDir, []string{"remote", "add", "origin", req.RepoURL}},
		// One fetch, both commits. Fetching by raw object name requires the
		// server to allow unadvertised wants; Team Server does, which is what
		// Clone has been relying on all along.
		{repoDir, []string{"fetch", "--filter=blob:none", "--no-tags", "origin", req.BaseSha, req.HeadSha}},
	}
	for _, side := range []struct{ dir, sha string }{{baseDir, req.BaseSha}, {headDir, req.HeadSha}} {
		steps = append(steps,
			// --detach is explicit. Without it `worktree add <path> <commit-ish>`
			// will DWIM a branch named after the path's basename in some cases,
			// and we never want a branch here. --no-checkout because the
			// sparse set must be in place BEFORE the first checkout, or git
			// downloads the whole tree anyway.
			step{repoDir, []string{"worktree", "add", "--no-checkout", "--detach", side.dir, side.sha}},
			// Per-worktree: git enables extensions.worktreeConfig on its own,
			// so base and head each get their own sparse file and the bare
			// repo stays unaffected. Confirmed on git 2.39 (the image's).
			step{side.dir, append([]string{"sparse-checkout", "set", "--no-cone"}, modelPaths...)},
			// The actual checkout. Lazily fetches the model blobs, in one
			// batched request per side.
			step{side.dir, []string{"reset", "--hard", "--quiet"}},
		)
	}
	steps = append(steps, step{repoDir, prefetchTextDiffBlobsArgs(req.BaseSha, req.HeadSha)})

	for _, s := range steps {
		if err := runGit(ctx, s.dir, helperPath, s.args...); err != nil {
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

	return CloneBothResult{WorkDir: workDir, RepoDir: repoDir, BaseDir: baseDir, HeadDir: headDir}, nil
}

// prefetchTextDiffBlobsArgs is a `git diff --numstat` over the same paths and
// rename detection TextDiffs uses. Its output is discarded; it runs for its
// side effect: computing line counts makes git batch-fetch every blob the real
// diff will need, while the credential file still exists.
//
// ▶ Why this step exists: TextDiffs runs later with NO credentials, so any
// blob it lacks becomes a lazy fetch that fails as an auth error far from any
// auth code (gap analysis §5.2's trap). Before sparse checkout the full
// worktree checkouts happened to fetch everything. Now this is the guarantee,
// and it must stay in step with TextDiffs' pathspecs and -M.
func prefetchTextDiffBlobsArgs(baseSha, headSha string) []string {
	args := []string{"diff", "--numstat", "-M", baseSha, headSha, "--"}
	return append(args, DefaultTextDiffPaths...)
}
