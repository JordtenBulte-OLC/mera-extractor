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
	workDir, err := os.MkdirTemp(workRoot, "clone-*")
	if err != nil {
		return CloneResult{}, fmt.Errorf("create workdir under %q: %w", workRoot, err)
	}

	host, err := hostOf(req.RepoURL)
	if err != nil {
		os.RemoveAll(workDir)
		return CloneResult{}, err
	}

	// §1.7 in the manual: the PAT lives in a credential helper file on
	// disk, never in the clone URL — a URL-embedded credential ends up in
	// .git/config, in process listings, and in error messages.
	helperPath := filepath.Join(workDir, ".git-credentials")
	credLine := fmt.Sprintf("https://%s:%s@%s\n",
		url.QueryEscape(req.Username), url.QueryEscape(req.Pat), host)
	if err := os.WriteFile(helperPath, []byte(credLine), 0600); err != nil {
		os.RemoveAll(workDir)
		return CloneResult{}, fmt.Errorf("write credential helper: %w", err)
	}
	defer os.Remove(helperPath) // wipe it the moment git no longer needs it

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

	mprPath, err := findMpr(workDir)
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
func Cleanup(workDir string) error {
	return os.RemoveAll(workDir)
}

func runGit(ctx context.Context, dir, helperPath string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never hang on an interactive credential prompt
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=store --file="+helperPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %s", args, string(out))
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

// findMpr locates the model file without assuming a name — App.mpr is a
// common default, but your own test app is named MERA.mpr.
func findMpr(workDir string) (string, error) {
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