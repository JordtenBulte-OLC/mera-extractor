// internal/mx/prepare.go
package mx

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"mera-extractor/internal/gitops"
)

// selfReferenceMismatchRE matches mx's real error text for the internal
// self-reference/filename mismatch (confirmed this project: a checkout
// tracked as MERA.mpr internally self-referring to App.mpr) —
// "existing MPR contents refer to MPR file 'App.mpr'" — capturing the
// filename mx actually wants.
var selfReferenceMismatchRE = regexp.MustCompile(`existing MPR contents refer to MPR file '([^']+)'`)

func parseSelfReferenceMismatch(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	m := selfReferenceMismatchRE.FindStringSubmatch(err.Error())
	if m == nil {
		return "", false
	}
	return m[1], true
}

// copyFile copies src to dst, creating/truncating dst. This is a
// workaround for the self-reference mismatch, not a structural fix — the
// original file at src is left untouched.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("copyFile: open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("copyFile: create %s: %w", dst, err)
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("copyFile: close %s: %w", dst, cerr)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return fmt.Errorf("copyFile: copy %s -> %s: %w", src, dst, err)
	}
	return nil
}

// PrepareMpr resolves the checkout's .mpr file and runs Analyze against
// it, transparently working around the on-disk-filename vs.
// internal-self-reference mismatch (Step 3): if the first Analyze call
// fails with that specific error, it copies the .mpr to the name mx wants
// and retries exactly once.
func PrepareMpr(ctx context.Context, bin Binary, dir string) (mprPath string, result AnalyzeResult, err error) {
	mprPath, err = gitops.FindMpr(dir)
	if err != nil {
		return
	}
	result, err = Analyze(ctx, bin, mprPath)
	if wantName, ok := parseSelfReferenceMismatch(err); ok {
		renamed := filepath.Join(dir, wantName)
		if cpErr := copyFile(mprPath, renamed); cpErr != nil {
			return mprPath, result, err
		}
		mprPath = renamed
		result, err = Analyze(ctx, bin, mprPath) // retry once
	}
	return
}
