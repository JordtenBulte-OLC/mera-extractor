// internal/mxcli/list.go
package mxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// UnitSummary is the minimal shape we need from `mxcli show <type> --json`.
type UnitSummary struct {
	Module        string `json:"Module"`
	Name          string `json:"Name"`
	QualifiedName string `json:"Qualified Name"`
}

// DefaultUnitTypes is a starter set worth rendering for a naive,
// no-diff-yet extraction — skips settings/associations/constants/
// enumerations/odata for a first pass since they're lower-value for a
// code review. Extend as needed.
var DefaultUnitTypes = []string{
	"microflow",
	"nanoflow",
	"page",
	"entity",
	"javaaction",
	"workflow",
}

// pluralType maps the singular type Describe expects to the plural type
// show expects — confirmed inconsistent between the two commands.
var pluralType = map[string]string{
	"microflow":  "microflows",
	"nanoflow":   "nanoflows",
	"page":       "pages",
	"entity":     "entities",
	"javaaction": "javaactions",
	"workflow":   "workflows",
}

// ListUnits enumerates units of one type. unitType is the SINGULAR form
// (matching Describe's convention) — translated internally. module is
// optional: pass "" to list every unit of this type across the whole app,
// or a module name (matching the "Module" field in mxcli's own output,
// e.g. "Administration") to scope the listing to just that module — mxcli
// supports this as an extra positional argument, confirmed against the
// `show`/`list` help text's own examples ("mxcli show -p app.mpr
// microflows MyModule").
func ListUnits(ctx context.Context, mprPath, unitType, module string) ([]UnitSummary, error) {
	plural, ok := pluralType[unitType]
	if !ok {
		return nil, fmt.Errorf("no plural mapping for unit type %q — add it to pluralType", unitType)
	}

	args := []string{"show", "-p", mprPath, plural}
	if module != "" {
		args = append(args, module)
	}
	args = append(args, "--json")

	out, err := run(ctx, 30*time.Second, args...)
	if err != nil {
		return nil, err
	}

	var units []UnitSummary
	if err := json.Unmarshal([]byte(out), &units); err != nil {
		return nil, fmt.Errorf("parse mxcli show %s --json output: %w", plural, err)
	}
	return units, nil
}