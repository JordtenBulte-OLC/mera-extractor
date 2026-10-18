// internal/gitops/clone_both_sparse_test.go
package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sparseFixture is a two-commit repo shaped like a real Mendix app: an MPRv2
// model (.mpr + mprcontents/), Java, theme, and a heavy binary in userlib/.
// The head commit changes the model, one Java file and the jar, and leaves a
// second Java file alone — the cases the sparse checkout must tell apart.
func sparseFixture(t *testing.T) (dir, baseSha, headSha string) {
	t.Helper()
	dir = t.TempDir()
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
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("-c", "init.defaultBranch=main", "init")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	git("config", "uploadpack.allowFilter", "true")

	write("App.mpr", "model v1")
	write("mprcontents/ab/cd/unit.mxunit", "unit v1")
	write("javasource/m/Changed.java", "class Changed {}\n")
	write("javasource/m/Untouched.java", "class Untouched {}\n")
	write("theme/web/main.scss", "body { color: red; }\n")
	write("userlib/heavy.jar", strings.Repeat("jar-v1 ", 4096))
	git("add", "-A")
	git("commit", "-m", "base")
	baseSha = git("rev-parse", "HEAD")

	write("App.mpr", "model v2")
	write("mprcontents/ab/cd/unit.mxunit", "unit v2")
	write("javasource/m/Changed.java", "class Changed { int x; }\n")
	write("userlib/heavy.jar", strings.Repeat("jar-v2 ", 4096))
	git("add", "-A")
	git("commit", "-m", "head")
	headSha = git("rev-parse", "HEAD")
	return dir, baseSha, headSha
}

// missingBlobPaths lists the paths whose blobs (in either commit) are NOT in
// the partial clone's object store, i.e. were never downloaded.
func missingBlobPaths(t *testing.T, repoDir string, shas ...string) map[string]bool {
	t.Helper()
	args := append([]string{"rev-list", "--objects", "--missing=print"}, shas...)
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	missingIDs := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "?") {
			missingIDs[strings.TrimPrefix(line, "?")] = true
		}
	}
	// Map ids back to paths via the trees, which ARE local under blob:none.
	paths := map[string]bool{}
	for _, sha := range shas {
		cmd := exec.Command("git", "ls-tree", "-r", sha)
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("ls-tree: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			// "<mode> blob <id>\t<path>"
			meta, path, _ := strings.Cut(line, "\t")
			if f := strings.Fields(meta); len(f) == 3 && missingIDs[f[2]] {
				paths[path] = true
			}
		}
	}
	return paths
}

func TestCloneBoth_WorktreesHoldOnlyTheModel(t *testing.T) {
	src, base, head := sparseFixture(t)
	res, err := CloneBoth(context.Background(), t.TempDir(), CloneBothRequest{
		RepoURL: "file://" + src, BaseSha: base, HeadSha: head,
	})
	if err != nil {
		t.Fatalf("CloneBoth: %v", err)
	}
	t.Cleanup(func() { Cleanup(res.WorkDir) })

	for dir, want := range map[string]string{res.BaseDir: "v1", res.HeadDir: "v2"} {
		if got := readFile(t, filepath.Join(dir, "App.mpr")); got != "model "+want {
			t.Errorf("%s App.mpr = %q", dir, got)
		}
		if got := readFile(t, filepath.Join(dir, "mprcontents/ab/cd/unit.mxunit")); got != "unit "+want {
			t.Errorf("%s mprcontents unit = %q", dir, got)
		}
		for _, absent := range []string{"javasource", "theme", "userlib"} {
			if _, err := os.Stat(filepath.Join(dir, absent)); !os.IsNotExist(err) {
				t.Errorf("%s/%s should not be checked out", dir, absent)
			}
		}
	}
}

// The point of the sparse checkout: what isn't model and isn't a changed
// text-diff file never crosses the network. And the counterpart: everything
// TextDiffs will read IS local (the trap from gap analysis §5.2).
func TestCloneBoth_DownloadsOnlyWhatIsRead(t *testing.T) {
	src, base, head := sparseFixture(t)
	res, err := CloneBoth(context.Background(), t.TempDir(), CloneBothRequest{
		RepoURL: "file://" + src, BaseSha: base, HeadSha: head,
	})
	if err != nil {
		t.Fatalf("CloneBoth: %v", err)
	}
	t.Cleanup(func() { Cleanup(res.WorkDir) })

	missing := missingBlobPaths(t, res.RepoDir, base, head)
	for _, p := range []string{"userlib/heavy.jar", "javasource/m/Untouched.java"} {
		if !missing[p] {
			t.Errorf("%s was downloaded; nothing reads it", p)
		}
	}
	for _, p := range []string{"App.mpr", "mprcontents/ab/cd/unit.mxunit", "javasource/m/Changed.java"} {
		if missing[p] {
			t.Errorf("%s is missing; mx or TextDiffs needs it", p)
		}
	}
}
