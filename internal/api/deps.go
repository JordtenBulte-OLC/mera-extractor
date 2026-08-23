// internal/api/deps.go
package api

import (
	"context"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
	"mera-extractor/internal/mxcli"
)

// Deps is the seam between the HTTP layer and everything that shells out to
// a subprocess or talks to the network.
//
// Why function fields rather than an interface: a test almost never wants to
// fake all nine, and function fields let it override exactly the two or three
// a given case is about while the rest fall back to the real thing. An
// interface would force every fake to implement the whole surface.
//
// The zero value is valid and means "use the real tools" — withDefaults fills
// in every nil field. That is deliberate: `&Server{WorkRoot: ..., MxRoot: ...}`
// and `NewServer(...)` both keep working untouched, so adding this seam costs
// exactly one new field on Server and changes no existing construction site.
type Deps struct {
	CloneBoth func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error)
	Cleanup   func(workDir string) error
	FindMpr   func(dir string) (string, error)
	TextDiffs func(ctx context.Context, repoDir, baseSha, headSha string, pathspecs []string) ([]gitops.TextDiff, error)

	MxListVersions        func(mxRoot string) ([]mx.Binary, error)
	MxHighest             func(mxRoot string) (mx.Binary, error)
	MxResolve             func(mxRoot, mendixVersion string) (mx.Binary, error)
	PrepareMpr            func(ctx context.Context, bin mx.Binary, dir string) (string, mx.AnalyzeResult, error)
	Diff                  func(ctx context.Context, bin mx.Binary, basePath, headPath, outPath string) (mx.DiffResult, error)
	ResolveQualifiedNames func(ctx context.Context, bin mx.Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]mx.ResolvedUnit, error)

	Describe     func(ctx context.Context, req mxcli.DescribeRequest) (string, error)
	ListUnits    func(ctx context.Context, mprPath, unitType, module string) ([]mxcli.UnitSummary, error)
	MxcliVersion func(ctx context.Context) (string, error)
}

// withDefaults returns a copy with every unset field pointing at the real
// implementation. Cheap enough to call per request (nine word-sized copies),
// and doing it per request rather than in NewServer means a Server built as a
// struct literal — which tests do — behaves identically to one from NewServer.
func (d Deps) withDefaults() Deps {
	if d.CloneBoth == nil {
		d.CloneBoth = gitops.CloneBoth
	}
	if d.Cleanup == nil {
		d.Cleanup = gitops.Cleanup
	}
	if d.FindMpr == nil {
		d.FindMpr = gitops.FindMpr
	}
	if d.TextDiffs == nil {
		d.TextDiffs = gitops.TextDiffs
	}
	if d.MxListVersions == nil {
		d.MxListVersions = mx.ListVersions
	}
	if d.MxHighest == nil {
		d.MxHighest = mx.Highest
	}
	if d.MxResolve == nil {
		d.MxResolve = mx.Resolve
	}
	if d.PrepareMpr == nil {
		d.PrepareMpr = mx.PrepareMpr
	}
	if d.Diff == nil {
		d.Diff = mx.Diff
	}
	if d.ResolveQualifiedNames == nil {
		d.ResolveQualifiedNames = mx.ResolveQualifiedNames
	}
	if d.Describe == nil {
		d.Describe = mxcli.Describe
	}
	if d.ListUnits == nil {
		d.ListUnits = mxcli.ListUnits
	}
	if d.MxcliVersion == nil {
		d.MxcliVersion = mxcli.Version
	}
	return d
}
