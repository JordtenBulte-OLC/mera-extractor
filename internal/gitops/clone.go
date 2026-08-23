// internal/gitops/clone.go
package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type CloneRequest struct {
	RepoURL  string
	Username string // "pat" for Mendix Team Server
	Pat      string
	Sha      string
}

type CloneResult struct {
	WorkDir string `json:"workDir"` // caller is responsible for Cleanup(WorkDir) when done
	MprPath string `json:"mprPath"` // the .mpr file actually found in the checkout
}

// Clone fetches a single commit into a fresh directory under workRoot and
// returns the path to it, plus whichever .mpr file it found there.
//
// workRoot is passed in, not hardcoded, so the same code works whether
// it's running via `go run .` locally (workRoot = os.TempDir()) or inside
// the container (workRoot = "/work", set via WORKDIR in the Dockerfile).
func Clone(ctx context.Context, workRoot string, req CloneRequest) (CloneResult, error) {
	if err := validateSha("sha", req.Sha); err != nil {
		return CloneResult{}, err
	}

	workDir, err := os.MkdirTemp(workRoot, "clone-*")
	if err != nil {
		return CloneResult{}, fmt.Errorf("create workdir under %q: %w", workRoot, err)
	}

	// §1.7 in the manual: the PAT lives in a credential helper file on
	// disk, never in the clone URL — a URL-embedded credential ends up in
	// .git/config, in process listings, and in error messages.
	helperPath, err := writeCredentialHelper(workDir, req.RepoURL, req.Username, req.Pat)
	if err != nil {
		os.RemoveAll(workDir)
		return CloneResult{}, err
	}
	if helperPath != "" {
		defer os.Remove(helperPath) // wipe it the moment git no longer needs it
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	steps := [][]string{
		{"init"},
		{"remote", "add", "origin", req.RepoURL},
		{"fetch", "--filter=blob:none", "--no-tags", "origin", req.Sha},
		{"checkout", req.Sha},
	}
	for _, args := range steps {
		if err := runGit(ctx, workDir, helperPath, args...); err != nil {
			os.RemoveAll(workDir)
			return CloneResult{}, redact(err, req.Pat)
		}
	}

	mprPath, err := FindMpr(workDir)
	if err != nil {
		os.RemoveAll(workDir)
		return CloneResult{}, err
	}

	return CloneResult{WorkDir: workDir, MprPath: mprPath}, nil
}

// Cleanup removes everything Clone created. Call it once you're done
// reading from WorkDir — via `defer` if the whole lifecycle fits in one
// request (that's what /extract does), or explicitly later if the caller
// needs the checkout to outlive a single call (that's the leased-workspace
// idea in manual §1.8 — not built yet, this is the seed of it).
//
// This is also correct for CloneBoth: the bare repo, its worktrees, and the
// .git/worktrees administrative entries that link them all live under the
// same WorkDir, so one recursive remove takes the lot. No `git worktree
// prune` is needed — there is nothing left behind to prune against.
func Cleanup(workDir string) error {
	return os.RemoveAll(workDir)
}

func runGit(ctx context.Context, dir, helperPath string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never hang on an interactive credential prompt
	)
	// An empty helperPath means no credentials were supplied (public repo, or
	// a file:// fixture repo in a test). Configuring `store --file=` with an
	// empty path would make git error rather than fall through to anonymous.
	if helperPath != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=store --file="+helperPath,
		)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %s", args, string(out))
	}
	return nil
}

// writeCredentialHelper drops a 0600 .git-credentials file in dir and returns
// its path, or ("", nil) when no PAT was supplied. The caller owns removing it.
func writeCredentialHelper(dir, repoURL, username, pat string) (string, error) {
	if pat == "" {
		return "", nil
	}
	host, err := hostOf(repoURL)
	if err != nil {
		return "", err
	}
	helperPath := filepath.Join(dir, ".git-credentials")
	credLine := fmt.Sprintf("https://%s:%s@%s\n",
		url.QueryEscape(username), url.QueryEscape(pat), host)
	if err := os.WriteFile(helperPath, []byte(credLine), 0600); err != nil {
		return "", fmt.Errorf("write credential helper: %w", err)
	}
	return helperPath, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// validateSha rejects anything that is not a plain hex object name before it
// reaches an argv. Without this, a caller-supplied value beginning with "-"
// is read by git as a flag — `--upload-pack=...` on fetch is the classic one.
// The 7..64 range covers abbreviated names through a future SHA-256 object ID.
func validateSha(field, sha string) error {
	if sha == "" {
		return fmt.Errorf("%s is required", field)
	}
	if !shaPattern.MatchString(sha) {
		return fmt.Errorf("%s %q is not a valid commit hash", field, sha)
	}
	return nil
}

func hostOf(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid repoUrl %q", repoURL)
	}
	return u.Host, nil
}

// FindMpr locates the model file without assuming a name — App.mpr is a
// common default, but your own test app is named MERA.mpr.
func FindMpr(workDir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(workDir, "*.mpr"))
	if err != nil {
		return "", fmt.Errorf("glob for .mpr in %q: %w", workDir, err)
	}
	if len(matches) == 0 {
		return "", errors.New("no .mpr file found in checkout")
	}
	return matches[0], nil
}

// redact strips the PAT out of an error message before it can reach a log
// or an HTTP response. git sometimes echoes the remote URL — including
// embedded credentials, if any ever leaked in — back in its own errors.
func redact(err error, secret string) error {
	if secret == "" || err == nil {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}
