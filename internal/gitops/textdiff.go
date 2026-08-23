// internal/gitops/textdiff.go
package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// TextDiff is one file's unified diff between the base and head commits.
type TextDiff struct {
	Path        string `json:"path"`
	ChangeKind  string `json:"changeKind"` // Added | Modified | Deleted | Renamed | Copied
	UnifiedDiff string `json:"unifiedDiff"`
}

// DefaultTextDiffPaths is manual §1.5 step 11's pathspec list: the parts of a
// Mendix repo that are real text and therefore reviewable as text, as opposed
// to the .mpr model, which mx diff covers.
var DefaultTextDiffPaths = []string{"javasource", "theme", "deployment", "*.json"}

// maxUnifiedDiffBytes caps a SINGLE file's diff. One regenerated theme bundle
// or vendored .json can run to megabytes and would otherwise be pasted whole
// into an HTTP response and then into a model's context window. Truncation is
// marked inline so a reader can tell it happened. Package-level so a test can
// shrink it.
var maxUnifiedDiffBytes = 256 * 1024

// TextDiffs returns per-file unified diffs between two commits.
//
// It runs against the repository directory — the bare repo from CloneBoth is
// fine, since diffing two commits needs no working tree. No credentials are
// passed: every object involved is already local, because both worktrees were
// checked out, which materialised every blob in both trees. That matters under
// the blob:none partial clone — if this ever did need a lazy fetch it would
// fail, since CloneBoth removes the credential file before returning.
func TextDiffs(ctx context.Context, repoDir, baseSha, headSha string, pathspecs []string) ([]TextDiff, error) {
	if err := validateSha("baseSha", baseSha); err != nil {
		return nil, err
	}
	if err := validateSha("headSha", headSha); err != nil {
		return nil, err
	}
	if len(pathspecs) == 0 {
		pathspecs = DefaultTextDiffPaths
	}

	// -M detects renames; core.quotePath=false keeps non-ASCII paths readable
	// rather than \nnn-escaped. The trailing "--" separates revisions from
	// pathspecs, so a pathspec can never be mistaken for a revision.
	args := func(mode ...string) []string {
		a := []string{"-c", "core.quotePath=false", "diff", "--no-color", "-M"}
		a = append(a, mode...)
		a = append(a, baseSha, headSha, "--")
		return append(a, pathspecs...)
	}

	kinds, err := textDiffKinds(ctx, repoDir, args("--name-status", "-z"))
	if err != nil {
		return nil, err
	}

	out, err := runGitOutput(ctx, repoDir, args("--unified=5")...)
	if err != nil {
		return nil, err
	}

	var diffs []TextDiff
	for _, chunk := range splitDiffChunks(out) {
		path := pathFromChunk(chunk)
		if path == "" {
			continue
		}
		kind, ok := kinds[path]
		if !ok {
			// A chunk with no matching --name-status entry shouldn't happen,
			// but reporting it as Modified beats dropping the file silently.
			kind = "Modified"
		}
		if len(chunk) > maxUnifiedDiffBytes {
			chunk = chunk[:maxUnifiedDiffBytes] + fmt.Sprintf("\n... [truncated at %d bytes]\n", maxUnifiedDiffBytes)
		}
		diffs = append(diffs, TextDiff{Path: path, ChangeKind: kind, UnifiedDiff: chunk})
	}
	return diffs, nil
}

// textDiffKinds runs --name-status -z and returns path → change kind.
//
// The -z format is NUL-separated, not newline-separated, specifically so that
// paths containing spaces or newlines survive intact. Records are
// <status>\0<path>\0, except renames and copies which are
// <status>\0<old>\0<new>\0 — the extra field is why this can't be a simple
// pairwise scan.
func textDiffKinds(ctx context.Context, repoDir string, args []string) (map[string]string, error) {
	out, err := runGitOutput(ctx, repoDir, args...)
	if err != nil {
		return nil, err
	}

	kinds := map[string]string{}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		switch status[0] {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return kinds, nil // truncated record; take what we have
			}
			kind := "Renamed"
			if status[0] == 'C' {
				kind = "Copied"
			}
			kinds[fields[i+2]] = kind // key on the NEW path
			i += 3
		default:
			if i+1 >= len(fields) {
				return kinds, nil
			}
			kinds[fields[i+1]] = nameStatusKind(status)
			i += 2
		}
	}
	return kinds, nil
}

func nameStatusKind(status string) string {
	switch status[0] {
	case 'A':
		return "Added"
	case 'D':
		return "Deleted"
	case 'M', 'T':
		return "Modified" // T is a type change (file ↔ symlink); Modified is close enough
	default:
		return "Modified"
	}
}

// splitDiffChunks breaks `git diff` output into one string per file. Splitting
// on the "diff --git " header is safe because git always emits it at the start
// of a line, and content lines inside a hunk are prefixed with a space, + or -.
func splitDiffChunks(out string) []string {
	const sep = "diff --git "
	if !strings.HasPrefix(out, sep) {
		if i := strings.Index(out, "\n"+sep); i >= 0 {
			out = out[i+1:]
		} else {
			return nil
		}
	}
	parts := strings.Split(out, "\n"+sep)
	chunks := make([]string, 0, len(parts))
	for i, p := range parts {
		if i > 0 {
			p = sep + p
		}
		if strings.TrimSpace(p) != "" {
			chunks = append(chunks, p)
		}
	}
	return chunks
}

// pathFromChunk reads the file path out of one chunk's ---/+++ lines,
// preferring the b-side (the name after the change). A deletion has
// "+++ /dev/null", so it falls back to the a-side.
//
// It deliberately does NOT parse the "diff --git a/x b/x" header: with a path
// containing a space that line is genuinely ambiguous, while the ---/+++ lines
// carry exactly one path each.
func pathFromChunk(chunk string) string {
	var aPath, bPath string
	for _, line := range strings.Split(chunk, "\n") {
		switch {
		case strings.HasPrefix(line, "--- a/"):
			aPath = trimDiffPath(strings.TrimPrefix(line, "--- a/"))
		case strings.HasPrefix(line, "+++ b/"):
			bPath = trimDiffPath(strings.TrimPrefix(line, "+++ b/"))
		case strings.HasPrefix(line, "@@"):
			// Past the header; nothing later can change the answer.
			goto done
		}
	}
done:
	if bPath != "" {
		return bPath
	}
	if aPath != "" {
		return aPath
	}
	// Pure mode changes and binary files carry no ---/+++ pair, so fall back
	// to the header. Unambiguous as long as the path has no space, which is
	// the only case that reaches here.
	for _, line := range strings.Split(chunk, "\n") {
		if rest, ok := strings.CutPrefix(line, "diff --git a/"); ok {
			if i := strings.Index(rest, " b/"); i >= 0 {
				return rest[i+3:]
			}
		}
	}
	return ""
}

// trimDiffPath removes the trailing TAB (and anything after it) that git
// appends to a ---/+++ line WHEN AND ONLY WHEN the path contains a space.
// Without this, "theme/web/styles with space.css" comes back with a trailing
// tab and never matches its --name-status entry — silently mis-keying exactly
// the paths most likely to be hand-authored theme files.
func trimDiffPath(p string) string {
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		return p[:i]
	}
	return p
}

// runGitOutput is runGit's sibling for commands whose stdout is the payload.
// It keeps stderr OUT of the returned string — runGit uses CombinedOutput,
// which is fine when the output is only ever an error message, but would
// corrupt parsed output with any stray git progress or advice text.
func runGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %v: %s", args, err, stderr.String())
	}
	return string(out), nil
}
