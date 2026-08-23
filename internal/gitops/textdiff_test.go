// internal/gitops/textdiff_test.go
package gitops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// textFixture builds a repo whose second commit exercises every change kind
// TextDiffs has to classify, plus the cases most likely to break parsing:
// a path with a space, a binary file, and a file outside the pathspecs.
func textFixture(t *testing.T) (dir, baseSha, headSha string) {
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

	write("javasource/sales/Keep.java", "package sales;\nclass Keep {}\n")
	// Gone.java and New.java must be substantially DISSIMILAR, or `git diff -M`
	// pairs the delete with the add and reports one Renamed entry instead of
	// two. That is correct git behaviour and worth knowing about: an unrelated
	// add+delete of near-identical files does get reported as a rename.
	write("javasource/sales/Gone.java", "package sales;\n"+strings.Repeat("// obsolete helper\n", 40))
	write("javasource/sales/Old.java", strings.Repeat("// stable line\n", 20))
	// A long file with a single mid-file edit, to measure --unified=5.
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = "// line " + string(rune('a'+i%26))
	}
	write("javasource/sales/Context.java", strings.Join(lines, "\n")+"\n")
	write("theme/web/styles with space.css", "body { color: red; }\n")
	write("theme/web/logo.png", "\x89PNG\r\n\x1a\n\x00binary\x00bytes")
	write("config.json", "{\"a\":1}\n")
	write("MERA.mpr", "model v1")             // outside the pathspecs
	write("docs/readme.md", "not in scope\n") // outside the pathspecs
	git("add", "-A")
	git("commit", "-m", "base")
	baseSha = git("rev-parse", "HEAD")

	write("javasource/sales/Keep.java", "package sales;\nclass Keep { int x; }\n")
	os.Remove(filepath.Join(dir, "javasource/sales/Gone.java"))
	git("mv", "javasource/sales/Old.java", "javasource/sales/Renamed.java")
	write("javasource/sales/New.java", "package sales;\nimport java.util.List;\npublic class New implements Runnable {\n  public void run() { throw new IllegalStateException(\"todo\"); }\n}\n")
	lines[14] = "// CHANGED LINE"
	write("javasource/sales/Context.java", strings.Join(lines, "\n")+"\n")
	write("theme/web/styles with space.css", "body { color: blue; }\n")
	write("theme/web/logo.png", "\x89PNG\r\n\x1a\n\x00DIFFERENT\x00bytes")
	write("config.json", "{\"a\":2}\n")
	write("MERA.mpr", "model v2")
	write("docs/readme.md", "still not in scope, but changed\n")
	git("add", "-A")
	git("commit", "-m", "head")
	headSha = git("rev-parse", "HEAD")

	return dir, baseSha, headSha
}

func byPath(diffs []TextDiff) map[string]TextDiff {
	m := map[string]TextDiff{}
	for _, d := range diffs {
		m[d.Path] = d
	}
	return m
}

func TestTextDiffs_ClassifiesEveryChangeKind(t *testing.T) {
	dir, base, head := textFixture(t)

	diffs, err := TextDiffs(context.Background(), dir, base, head, nil)
	if err != nil {
		t.Fatalf("TextDiffs: %v", err)
	}
	got := byPath(diffs)

	want := map[string]string{
		"javasource/sales/Keep.java":      "Modified",
		"javasource/sales/New.java":       "Added",
		"javasource/sales/Gone.java":      "Deleted",
		"javasource/sales/Renamed.java":   "Renamed",
		"theme/web/styles with space.css": "Modified",
		"theme/web/logo.png":              "Modified",
		"config.json":                     "Modified",
	}
	for path, kind := range want {
		d, ok := got[path]
		if !ok {
			t.Errorf("missing %q; got %v", path, keys(got))
			continue
		}
		if d.ChangeKind != kind {
			t.Errorf("%s changeKind = %q, want %q", path, d.ChangeKind, kind)
		}
	}
}

func TestTextDiffs_RespectsPathspecs(t *testing.T) {
	dir, base, head := textFixture(t)

	diffs, err := TextDiffs(context.Background(), dir, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(diffs)

	// The .mpr is mx diff's job, and docs/ is not in the default pathspecs.
	// Both changed in this commit, so their absence is meaningful.
	for _, absent := range []string{"MERA.mpr", "docs/readme.md"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%q is outside DefaultTextDiffPaths and must not appear", absent)
		}
	}

	// An explicit pathspec narrows further.
	only, err := TextDiffs(context.Background(), dir, base, head, []string{"config.json"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Path != "config.json" {
		t.Errorf("explicit pathspec = %v, want just config.json", keys(byPath(only)))
	}
}

func TestTextDiffs_UnifiedContentIsReal(t *testing.T) {
	dir, base, head := textFixture(t)
	diffs, err := TextDiffs(context.Background(), dir, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := byPath(diffs)

	keep := got["javasource/sales/Keep.java"].UnifiedDiff
	if !strings.Contains(keep, "@@") {
		t.Errorf("expected a hunk header:\n%s", keep)
	}
	if !strings.Contains(keep, "+class Keep { int x; }") || !strings.Contains(keep, "-class Keep {}") {
		t.Errorf("both sides of the change should be present:\n%s", keep)
	}
	if !strings.HasPrefix(keep, "diff --git ") {
		t.Errorf("each chunk should start at its own header:\n%s", keep[:min(80, len(keep))])
	}
	// Chunks must not bleed into one another.
	if strings.Contains(keep, "config.json") {
		t.Errorf("chunk for Keep.java leaked another file:\n%s", keep)
	}

	// A binary file has no textual hunk; git says so rather than emitting one.
	png := got["theme/web/logo.png"].UnifiedDiff
	if !strings.Contains(png, "Binary files") {
		t.Errorf("binary diff should be reported as such:\n%s", png)
	}
}

// --unified=5 is manual §1.5's choice; git's default is 3. The extra context
// is what makes a Java diff reviewable, so confirm it actually took effect
// rather than trusting the flag was spelled right.
func TestTextDiffs_UsesFiveLinesOfContext(t *testing.T) {
	dir, base, head := textFixture(t)
	diffs, err := TextDiffs(context.Background(), dir, base, head, []string{"javasource"})
	if err != nil {
		t.Fatal(err)
	}
	d, ok := byPath(diffs)["javasource/sales/Context.java"]
	if !ok {
		t.Fatalf("Context.java missing from %v", keys(byPath(diffs)))
	}
	// One changed line at index 14 of 30, with 5 lines of context either side,
	// is an 11-line window. At git's default of 3 it would be 7.
	if !strings.Contains(d.UnifiedDiff, "@@ -10,11 +10,11 @@") {
		t.Errorf("want an 11-line hunk window (5 lines of context), got:\n%s", d.UnifiedDiff)
	}
}

func TestTextDiffs_TruncatesAHugeSingleFile(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=m", "GIT_AUTHOR_EMAIL=m@e.invalid",
			"GIT_COMMITTER_NAME=m", "GIT_COMMITTER_EMAIL=m@e.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	os.MkdirAll(filepath.Join(dir, "theme"), 0o755)
	os.WriteFile(filepath.Join(dir, "theme/big.css"), []byte(strings.Repeat("a\n", 50000)), 0o644)
	git("-c", "init.defaultBranch=main", "init")
	git("add", "-A")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(dir, "theme/big.css"), []byte(strings.Repeat("b\n", 50000)), 0o644)
	git("add", "-A")
	git("commit", "-m", "head")
	head := git("rev-parse", "HEAD")

	old := maxUnifiedDiffBytes
	maxUnifiedDiffBytes = 2048
	defer func() { maxUnifiedDiffBytes = old }()

	diffs, err := TextDiffs(context.Background(), dir, base, head, []string{"theme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 {
		t.Fatalf("want 1 diff, got %d", len(diffs))
	}
	if !strings.Contains(diffs[0].UnifiedDiff, "[truncated at 2048 bytes]") {
		t.Error("an oversized diff must be marked as truncated")
	}
	if len(diffs[0].UnifiedDiff) > 2048+64 {
		t.Errorf("truncated diff is %d bytes, cap is 2048", len(diffs[0].UnifiedDiff))
	}
}

func TestTextDiffs_IdenticalCommitsYieldNothing(t *testing.T) {
	dir, _, head := textFixture(t)
	diffs, err := TextDiffs(context.Background(), dir, head, head, nil)
	if err != nil {
		t.Fatalf("an empty diff is not an error: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("want no diffs, got %v", keys(byPath(diffs)))
	}
}

func TestTextDiffs_RejectsMalformedSha(t *testing.T) {
	dir, _, head := textFixture(t)
	if _, err := TextDiffs(context.Background(), dir, "--output=/tmp/pwned", head, nil); err == nil {
		t.Error("a flag-shaped revision must be rejected before reaching argv")
	}
	if _, err := TextDiffs(context.Background(), dir, "", head, nil); err == nil {
		t.Error("an empty baseSha must be rejected")
	}
}

// TextDiffs must work against CloneBoth's bare repo, which is how /extract
// will call it — diffing two commits needs no working tree, and every blob is
// already local because both worktrees were checked out.
func TestTextDiffs_WorksAgainstCloneBothsBareRepo(t *testing.T) {
	src, base, head := textFixture(t)
	// Enable the two server-side settings a raw-SHA partial fetch needs.
	for _, kv := range [][2]string{{"uploadpack.allowAnySHA1InWant", "true"}, {"uploadpack.allowFilter", "true"}} {
		cmd := exec.Command("git", "config", kv[0], kv[1])
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v\n%s", err, out)
		}
	}

	res, err := CloneBoth(context.Background(), t.TempDir(), CloneBothRequest{
		RepoURL: "file://" + src, BaseSha: base, HeadSha: head,
	})
	if err != nil {
		t.Fatalf("CloneBoth: %v", err)
	}
	t.Cleanup(func() { Cleanup(res.WorkDir) })

	if res.RepoDir == "" {
		t.Fatal("CloneBothResult must expose RepoDir for TextDiffs to use")
	}
	diffs, err := TextDiffs(context.Background(), res.RepoDir, base, head, nil)
	if err != nil {
		t.Fatalf("TextDiffs against the bare repo: %v", err)
	}
	got := byPath(diffs)
	if _, ok := got["javasource/sales/Keep.java"]; !ok {
		t.Errorf("expected the java change, got %v", keys(got))
	}
	if !strings.Contains(got["javasource/sales/Keep.java"].UnifiedDiff, "int x") {
		t.Error("blob content must be available locally under blob:none")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
