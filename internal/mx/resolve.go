// internal/mx/resolve.go
package mx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ResolvedUnit struct {
	ID            string
	Type          string
	QualifiedName string
	Module        string // QualifiedName's prefix before the first "."
}

// DumpMprExitError carries dump-mpr's own exit code (0-4) and its meaning
// — a DIFFERENT table from mx diff's, deliberately not reusing diff.go's
// error types even though some numeric codes overlap.
type DumpMprExitError struct {
	ExitCode int
	Stderr   string
}

func (e *DumpMprExitError) Error() string {
	meaning := map[int]string{
		1: "wrong project file provided",
		2: "invalid unit type(s)",
		3: "unknown JSON export error",
		4: "project is in a different Mendix version",
	}[e.ExitCode]
	if meaning == "" {
		meaning = "unexpected exit code"
	}
	return fmt.Sprintf("mx dump-mpr: exit %d (%s): %s", e.ExitCode, meaning, e.Stderr)
}

// ResolveQualifiedNames runs ONE `mx dump-mpr` call against mprPath,
// filtered via a single comma-joined --unit-type value (confirmed: dump-mpr
// accepts 'Type1,Type2,...' in one invocation), and returns a map from id
// to ResolvedUnit for every id in wantIDs that was found. Decodes
// generically (json.Unmarshal into any) and walks recursively for any
// object carrying BOTH $ID and $QualifiedName — deliberately not modeling
// dump-mpr's exact nesting schema, since that's undocumented.
func ResolveQualifiedNames(ctx context.Context, bin Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]ResolvedUnit, error) {
	outPath := filepath.Join(os.TempDir(), fmt.Sprintf("mera-dump-%d.json", time.Now().UnixNano()))
	defer os.Remove(outPath)

	res, err := run(ctx, bin, 5*time.Minute, "dump-mpr", mprPath,
		"--unit-type", strings.Join(unitTypes, ","),
		"--output-file", outPath)
	if err != nil {
		return nil, err // real execution failure
	}
	if res.ExitCode != 0 {
		return nil, &DumpMprExitError{ExitCode: res.ExitCode, Stderr: res.Stderr}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("mx dump-mpr: reading %s: %w", outPath, err)
	}
	data = stripBOM(data)
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mx dump-mpr: parsing %s: %w", outPath, err)
	}

	found := make(map[string]ResolvedUnit)
	walkForUnits(raw, wantIDs, found)
	return found, nil
}

// walkForUnits recursively walks a generically-decoded dump-mpr JSON tree
// looking for any object carrying BOTH $ID and $QualifiedName — the
// top-level-document marker, per this project's confirmed finding that
// nested children (e.g. Microflows$StartEvent) have $ID but no
// $QualifiedName.
func walkForUnits(node any, wantIDs map[string]bool, found map[string]ResolvedUnit) {
	switch v := node.(type) {
	case map[string]any:
		id, hasID := v["$ID"].(string)
		qname, hasQName := v["$QualifiedName"].(string)
		if hasID && hasQName && wantIDs[id] {
			typ, _ := v["$Type"].(string)
			module := qname
			if i := strings.Index(qname, "."); i >= 0 {
				module = qname[:i]
			}
			found[id] = ResolvedUnit{ID: id, Type: typ, QualifiedName: qname, Module: module}
		}
		for _, child := range v {
			walkForUnits(child, wantIDs, found)
		}
	case []any:
		for _, child := range v {
			walkForUnits(child, wantIDs, found)
		}
	}
}
