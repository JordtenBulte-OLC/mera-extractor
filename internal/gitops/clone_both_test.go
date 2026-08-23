// internal/gitops/clone_both_test.go
package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newFixtureRepo builds a throwaway two-commit repository on disk and returns
// a file:// URL for it plus the two commit hashes, oldest first.
//
// Two server-side settings matter and are easy to miss:
//
//   - uploadpack.allowAnySHA1InWant — without it, `git fetch origin <sha>`
//     is refused with "Server does not allow request for unadvertised object".
//     Team Server permits this, which is the only reason Clone works at all;
//     a stock local repo does not.
//   - uploadpack.allowFilter — without it the server silently ignores
//     --filter=blob:none, so the test would pass while exercising a full
//     fetch instead of the partial-clone path we actually ship.
func newFixtureRepo(t *testing.T) (repoURL, baseSha, headSha string) {
	t.Helper()
	dir := t.TempDir()

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=mera", "GIT_AUTHOR_EMAIL=mera@example.invalid",
			"GIT_COMMITTER_NAME=mera", "GIT_COMMITTER_EMAIL=mera@example.invalid",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("-c", "init.defaultBranch=main", "init")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	git("config", "uploadpack.allowFilter", "true")

	write("MERA.mpr", "base contents")
	git("add", ".")
	git("commit", "-m", "base")
	baseSha = git("rev-parse", "HEAD")

	write("MERA.mpr", "head contents")
	write("extra.txt", "added at head")
	git("add", ".")
	git("commit", "-m", "head")
	headSha = git("rev-parse", "HEAD")

	return "file://" + dir, baseSha, headSha
}

func headOf(t *testing.T, worktree string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse in %s: %v\n%s", worktree, err, out)
	}
	return strings.TrimSpace(string(out))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCloneBoth_MaterialisesBothCommits(t *testing.T) {
	repoURL, baseSha, headSha := newFixtureRepo(t)
	workRoot := t.TempDir()

	res, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
		RepoURL: repoURL, BaseSha: baseSha, HeadSha: headSha,
	})
	if err != nil {
		t.Fatalf("CloneBoth: %v", err)
	}
	t.Cleanup(func() { Cleanup(res.WorkDir) })

	if got := headOf(t, res.BaseDir); got != baseSha {
		t.Errorf("base worktree HEAD = %s, want %s", got, baseSha)
	}
	if got := headOf(t, res.HeadDir); got != headSha {
		t.Errorf("head worktree HEAD = %s, want %s", got, headSha)
	}

	// Blobs must be materialised, not just referenced — this is the assertion
	// that would fail if the partial clone's lazy fetch were broken.
	if got := readFile(t, filepath.Join(res.BaseDir, "MERA.mpr")); got != "base contents" {
		t.Errorf("base MERA.mpr = %q", got)
	}
	if got := readFile(t, filepath.Join(res.HeadDir, "MERA.mpr")); got != "head contents" {
		t.Errorf("head MERA.mpr = %q", got)
	}
	if _, err := os.Stat(filepath.Join(res.BaseDir, "extra.txt")); !os.IsNotExist(err) {
		t.Errorf("extra.txt should not exist in the base worktree")
	}

	// FindMpr is what mx.PrepareMpr calls on each side in Step 7.
	for _, dir := range []string{res.BaseDir, res.HeadDir} {
		if _, err := FindMpr(dir); err != nil {
			t.Errorf("FindMpr(%s): %v", dir, err)
		}
	}
}

func TestCloneBoth_SameShaBothSides(t *testing.T) {
	repoURL, _, headSha := newFixtureRepo(t)
	workRoot := t.TempDir()

	// Two detached worktrees at the same commit are legal — the "already
	// checked out" guard only applies to branches. An empty diff is a valid
	// review input, so this must not be an error.
	res, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
		RepoURL: repoURL, BaseSha: headSha, HeadSha: headSha,
	})
	if err != nil {
		t.Fatalf("CloneBoth with identical shas: %v", err)
	}
	t.Cleanup(func() { Cleanup(res.WorkDir) })

	if headOf(t, res.BaseDir) != headOf(t, res.HeadDir) {
		t.Error("both worktrees should sit at the same commit")
	}
}

func TestCloneBoth_CleanupLeavesNothing(t *testing.T) {
	repoURL, baseSha, headSha := newFixtureRepo(t)
	workRoot := t.TempDir()

	res, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
		RepoURL: repoURL, BaseSha: baseSha, HeadSha: headSha,
	})
	if err != nil {
		t.Fatalf("CloneBoth: %v", err)
	}

	// The worktree administrative entries live in repo.git/worktrees/, inside
	// WorkDir, so the existing recursive Cleanup is sufficient and no
	// `git worktree prune` step is needed. This is the assertion for that.
	entries, _ := os.ReadDir(filepath.Join(res.WorkDir, "repo.git", "worktrees"))
	if len(entries) != 2 {
		t.Fatalf("expected 2 worktree admin entries before cleanup, got %d", len(entries))
	}

	if err := Cleanup(res.WorkDir); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	left, err := os.ReadDir(workRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("workRoot not empty after Cleanup: %v", left)
	}
}

func TestCloneBoth_FailureCleansUpAndRedacts(t *testing.T) {
	repoURL, baseSha, _ := newFixtureRepo(t)
	workRoot := t.TempDir()

	const secret = "s3cr3t-pat-value"
	_, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
		RepoURL:  repoURL,
		Username: "pat",
		Pat:      secret,
		BaseSha:  baseSha,
		HeadSha:  "0000000000000000000000000000000000000000", // well formed, absent
	})
	if err == nil {
		t.Fatal("expected an error for a nonexistent headSha")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("PAT leaked into error: %v", err)
	}
	left, _ := os.ReadDir(workRoot)
	if len(left) != 0 {
		t.Errorf("workdir not cleaned up after failure: %v", left)
	}
}

func TestCloneBoth_RejectsMalformedSha(t *testing.T) {
	cases := []struct{ name, base, head string }{
		{"empty base", "", "abcdef1234567890abcdef1234567890abcdef12"},
		{"empty head", "abcdef1234567890abcdef1234567890abcdef12", ""},
		{"flag injection", "abcdef1234567890abcdef1234567890abcdef12", "--upload-pack=touch /tmp/pwned"},
		{"refname not hash", "abcdef1234567890abcdef1234567890abcdef12", "refs/heads/main"},
		{"too short", "abcdef1234567890abcdef1234567890abcdef12", "abc"},
	}
	workRoot := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
				RepoURL: "https://example.invalid/x.git", BaseSha: tc.base, HeadSha: tc.head,
			})
			if err == nil {
				t.Fatal("expected a validation error")
			}
			// Must be rejected before anything is created on disk.
			if left, _ := os.ReadDir(workRoot); len(left) != 0 {
				t.Errorf("validation should not touch the filesystem, found %v", left)
			}
		})
	}
}
