// internal/mx/testutil_test.go
package mx

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeStubMx creates a fake mx executable that ignores its arguments and
// just prints stdout/stderr and exits with the given code. Exercises
// run()'s exit-code handling and each caller's own switch on top of it,
// without a real mx binary — possible because every internal/mx function
// takes an explicit Binary.Path rather than a hardcoded command name, so
// nothing here needs PATH manipulation or a real install. Reused across
// this package's tests (Analyze here; Diff/ResolveQualifiedNames later).
func writeStubMx(t *testing.T, stdout, stderr string, exitCode int) Binary {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mx")

	script := "#!/bin/sh\n"
	if stdout != "" {
		script += "cat <<'MERA_STDOUT_EOF'\n" + stdout + "\nMERA_STDOUT_EOF\n"
	}
	if stderr != "" {
		script += "cat <<'MERA_STDERR_EOF' >&2\n" + stderr + "\nMERA_STDERR_EOF\n"
	}
	script += fmt.Sprintf("exit %d\n", exitCode)

	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writeStubMx: %v", err)
	}
	return Binary{Version: "stub", Path: path}
}