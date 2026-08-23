// internal/gitops/clone_both_integration_test.go
package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Guarded integration test — real Team Server, real network, real PAT.
//
//	export MERA_PAT="..."                       # or: source ~/.mera-secrets.sh
//	export MERA_IT_REPO="https://git.api.mendix.com/b12ab91d-b0f7-42fa-b404-a2e86aa7f674.git"
//	export MERA_IT_BASE_SHA="..."               # the microflow-add pair
//	export MERA_IT_HEAD_SHA="..."
//	go test ./internal/gitops/ -run Integration -v
//
// Absent any of those it skips, so `go test ./...` stays hermetic.
func TestCloneBoth_Integration(t *testing.T) {
	repoURL := os.Getenv("MERA_IT_REPO")
	pat := os.Getenv("MERA_PAT")
	baseSha := os.Getenv("MERA_IT_BASE_SHA")
	headSha := os.Getenv("MERA_IT_HEAD_SHA")
	if repoURL == "" || pat == "" || baseSha == "" || headSha == "" {
		t.Skip("set MERA_IT_REPO, MERA_PAT, MERA_IT_BASE_SHA and MERA_IT_HEAD_SHA to run")
	}

	workRoot := t.TempDir()
	res, err := CloneBoth(context.Background(), workRoot, CloneBothRequest{
		RepoURL:  repoURL,
		Username: "pat",
		Pat:      pat,
		BaseSha:  baseSha,
		HeadSha:  headSha,
	})
	if err != nil {
		t.Fatalf("CloneBoth against the real repo: %v", err)
	}
	defer Cleanup(res.WorkDir)

	for name, dir := range map[string]string{"base": res.BaseDir, "head": res.HeadDir} {
		mpr, err := FindMpr(dir)
		if err != nil {
			t.Fatalf("%s worktree: %v", name, err)
		}
		fi, err := os.Stat(mpr)
		if err != nil || fi.Size() == 0 {
			t.Fatalf("%s .mpr at %s is missing or empty (blob:none lazy fetch did not materialise it)", name, mpr)
		}
		// mprcontents/ is where the per-unit BSON lives; if the partial clone
		// only faked the checkout, this is where it shows.
		if _, err := os.Stat(filepath.Join(dir, "mprcontents")); err != nil {
			t.Errorf("%s worktree has no mprcontents/: %v", name, err)
		}
		t.Logf("%s: %s (%d bytes)", name, mpr, fi.Size())
	}

	if got := headOf(t, res.BaseDir); got != baseSha {
		t.Errorf("base HEAD = %s, want %s", got, baseSha)
	}
	if got := headOf(t, res.HeadDir); got != headSha {
		t.Errorf("head HEAD = %s, want %s", got, headSha)
	}
}
