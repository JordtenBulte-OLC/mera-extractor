// internal/api/extract.go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"sync"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
	"mera-extractor/internal/mxcli"
)

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

type unitRequest struct {
	UnitType      string `json:"unitType"`
	QualifiedName string `json:"qualifiedName"`
}

type extractRequest struct {
	RequestID string `json:"requestId"` // echoed back; idempotency is not implemented yet
	RepoURL   string `json:"repoUrl"`
	Username  string `json:"username"`
	Pat       string `json:"pat"`
	BaseSha   string `json:"baseSha"`
	HeadSha   string `json:"headSha"`

	// Escape hatch. When either is set, the diff path is skipped entirely and
	// the old naive enumeration runs against the HEAD worktree, returning the
	// pre-Stage-8 response shape unchanged. Units takes precedence over Modules.
	Units   []unitRequest `json:"units"`
	Modules []string      `json:"modules"`
}

// ---------------------------------------------------------------------------
// Response — manual §1.4's frozen shape, minus referenceGraph (a later phase)
// ---------------------------------------------------------------------------

type changeUnit struct {
	Module        string `json:"module"`
	UnitType      string `json:"unitType"`      // the mx diff type, e.g. "Microflows$Microflow"
	QualifiedName string `json:"qualifiedName"` // head-side name, falling back to base-side

	// PreviousQualifiedName is set only when the base and head sides resolved
	// to different names — i.e. the unit was renamed in this commit. Not in
	// manual §1.4; additive and omitempty, so no existing consumer sees a change.
	PreviousQualifiedName string `json:"previousQualifiedName,omitempty"`

	// QualifiedNameSynthesized marks a name inferred from the containment
	// chain rather than read from a real $QualifiedName — see mx.ResolvedUnit.
	QualifiedNameSynthesized bool `json:"qualifiedNameSynthesized,omitempty"`

	ChangeKind      string          `json:"changeKind"` // Added | Modified | Deleted
	StructuralDelta json.RawMessage `json:"structuralDelta,omitempty"`
	BeforeMdl       string          `json:"beforeMdl,omitempty"`
	AfterMdl        string          `json:"afterMdl,omitempty"`
	TokenEstimate   int             `json:"tokenEstimate"`
}

type textDiff struct {
	Path        string `json:"path"`
	ChangeKind  string `json:"changeKind"`
	UnifiedDiff string `json:"unifiedDiff"`
}

type extractResponse struct {
	RequestID string `json:"requestId,omitempty"`

	MendixVersion string `json:"mendixVersion,omitempty"`
	StorageFormat string `json:"storageFormat,omitempty"`
	MxcliVersion  string `json:"mxcliVersion,omitempty"`
	MxVersion     string `json:"mxVersion,omitempty"`

	ChangeUnits []changeUnit `json:"changeUnits"`
	TextDiffs   []textDiff   `json:"textDiffs,omitempty"` // Step 10; always empty for now
	Warnings    []string     `json:"warnings,omitempty"`
}

// legacyExtractResponse is the pre-Stage-8 shape, kept byte-identical for the
// Units/Modules escape hatch. Deliberately a separate type rather than shared
// omitempty fields on extractResponse — that way "the old response is
// unchanged" is guaranteed by the type system instead of by careful tagging.
type legacyExtractResponse struct {
	MprPath  string       `json:"mprPath"`
	Units    []unitResult `json:"units"`
	Warnings []string     `json:"warnings,omitempty"`
}

type unitResult struct {
	QualifiedName string `json:"qualifiedName"`
	Mdl           string `json:"mdl,omitempty"`
	Warning       string `json:"warning,omitempty"`
}

// ---------------------------------------------------------------------------
// diff type → mxcli unit type
// ---------------------------------------------------------------------------

// diffTypeToMxcli maps Mendix metamodel type strings — what `mx diff` puts in
// each unitDifference's `type` — onto the singular unit types that
// `mxcli describe` accepts (v0.19.0's list).
//
// The two tools share a vocabulary: `mx dump-mpr`'s `$Type` and `mx diff`'s
// `type` produce identical strings, which is why the keys below could be read
// straight off a real dump. "Microflows$Microflow" and "Images$ImageCollection"
// appear in BOTH a real diff and a real dump, which is the direct evidence for
// that claim.
//
// Completeness is not required for correctness. A type that is neither here nor
// in knownNotDescribable produces exactly one warning naming the string mx
// actually emitted, so a real run tells you precisely which key to add rather
// than silently dropping the unit.
var diffTypeToMxcli = map[string]string{
	// CONFIRMED — observed as a top-level unit in real dump-mpr output
	// (internal/mx/testdata/dump-reviewmanagement-trim.json) and matched to a
	// real `mxcli describe` type.
	"Microflows$Microflow":         "microflow",
	"Pages$Page":                   "page",
	"Pages$Snippet":                "snippet",
	"Constants$Constant":           "constant",
	"Enumerations$Enumeration":     "enumeration",
	"JavaActions$JavaAction":       "javaaction",
	"JsonStructures$JsonStructure": "jsonstructure",
	"ImportMappings$ImportMapping": "importmapping",
	"ExportMappings$ExportMapping": "exportmapping",

	// CONFIRMED type string, from real `mx diff` output rather than the dump
	// fixture. mxcli does have a describe type for it — an earlier revision of
	// this map wrongly assumed it did not.
	"Images$ImageCollection": "imagecollection",

	// CONFIRMED type strings that appear NESTED in dump-mpr rather than at top
	// level, but do carry a real $QualifiedName and do have an mxcli type.
	// Whether mx diff ever reports at this granularity is unknown — a domain
	// model edit may well surface as DomainModels$DomainModel instead. Harmless
	// if never hit.
	"DomainModels$Entity":           "entity",
	"DomainModels$Association":      "association",
	"DomainModels$CrossAssociation": "association",
	"Security$ModuleRole":           "modulerole",

	// UNVERIFIED — the namespace is inferred from the confirmed naming pattern
	// (<PluralNamespace>$<TypeName>), the mxcli type is from its own --help.
	// A wrong key never matches and costs nothing; a wrong value degrades to a
	// per-unit describe warning. Confirm and drop the marker, or delete.
	"Microflows$Nanoflow": "nanoflow",
	"Workflows$Workflow":  "workflow",
	"Pages$Layout":        "layout",
	"Pages$BuildingBlock": "buildingblock",

	// Deliberately NOT mapped, though mxcli has types for them: the remaining
	// entries in `mxcli describe --help` (settings, projectsecurity, userrole,
	// demouser, navigation, queue, scheduledevent, restclient, odataclient,
	// odataservice, regularexpression, databaseconnection, businesseventservice,
	// agent, aimodel, knowledgebase, consumedmcpservice, datatransformer, menu)
	// have no confirmed metamodel type string yet. Guessing ~18 namespaces would
	// put fiction in a lookup table that reads as authoritative. Let the warning
	// mechanism surface the real strings, then add them here as CONFIRMED.
}

// knownNotDescribable lists metamodel types that legitimately produce no MDL,
// with the reason. Separating these from genuinely unknown types is what keeps
// the warning list actionable: a warning should mean "confirm this and extend
// the map", not "this is working as designed".
var knownNotDescribable = map[string]string{
	// mxcli's `module` renders the module's ENTIRE contents. Calling it here
	// would reintroduce exactly the full-app enumeration Stage 8 exists to
	// eliminate — a module-level change would drag in all 87 of its units.
	// The individual changed units are reported separately anyway.
	"Projects$Module": "mxcli `module` would render the whole module's contents",

	"Projects$Folder":          "folders have no mxcli describe type",
	"Projects$ModuleSettings":  "mxcli `settings` is project-level, not module-level",
	"DomainModels$DomainModel": "no mxcli describe type; entity and association changes are reported as their own units",
	"Security$ModuleSecurity":  "no mxcli describe type; `modulerole` covers the roles individually",
}

// output for mxcli describe --help (v0.19.0). Use to extend diffTypeToMxcli map when combined with the outcome of mx diff output

// Types:
//   module           Describe a module (all contents)
//   entity           Describe an entity
//   externalentity   Describe an external entity (alias for entity)
//   association      Describe an association
//   enumeration      Describe an enumeration
//   constant         Describe a constant
//   microflow        Describe a microflow
//   nanoflow         Describe a nanoflow
//   workflow         Describe a workflow
//   page             Describe a page
//   snippet          Describe a snippet
//   buildingblock    Describe a building block (read-only, also: "building block")
//   layout           Describe a layout
//   javaaction       Describe a java action
//   jsonstructure    Describe a JSON structure (also: "json structure")
//   importmapping    Describe an import mapping (also: "import mapping")
//   exportmapping    Describe an export mapping (also: "export mapping")
//   restclient       Describe a consumed REST service (also: "rest client")
//   odataclient      Describe a consumed OData service
//   odataservice     Describe a published OData service
//   imagecollection  Describe an image collection (also: "image collection")
//   menu             Describe a standalone menu document
//   queue            Describe a task queue
//   scheduledevent   Describe a scheduled event (also: "scheduled event")
//   regularexpression  Describe a regular expression (also: "regular expression")
//   businesseventservice  Describe a business event service (also: "business event service")
//   databaseconnection    Describe a database connection (also: "database connection")
//   agent            Describe an AI agent (also: "agent")
//   aimodel          Describe an AI model (also: "model", "ai model")
//   knowledgebase    Describe a knowledge base (also: "knowledge base")
//   consumedmcpservice  Describe a consumed MCP service (also: "consumed mcp service")
//   datatransformer  Describe a data transformer (also: "data transformer")
//   modulerole       Describe a module role
//   userrole         Describe a user role
//   projectsecurity  Show project security settings
//   settings         Describe project settings
//   demouser         Describe a demo user
//   navigation       Describe a navigation profile
//   systemoverview   Module dependency graph (requires --format elk)

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	var req extractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}
	if req.BaseSha == "" || req.HeadSha == "" {
		respondError(w, http.StatusBadRequest, errors.New("baseSha and headSha are both required"))
		return
	}

	d := s.Deps.withDefaults()
	ctx := r.Context()

	clone, err := d.CloneBoth(ctx, s.WorkRoot, gitops.CloneBothRequest{
		RepoURL:  req.RepoURL,
		Username: req.Username,
		Pat:      req.Pat,
		BaseSha:  req.BaseSha,
		HeadSha:  req.HeadSha,
	})
	if err != nil {
		// A malformed (as opposed to missing) SHA also lands here as a 502,
		// because gitops validates internally and exports no predicate to ask
		// first. Worth revisiting if it turns out to confuse callers.
		respondError(w, http.StatusBadGateway, err)
		return
	}
	// Same reasoning as before Stage 8: the whole lifecycle fits in one
	// request, so defer is correct and keeps the container's disk from
	// filling up over many requests.
	defer d.Cleanup(clone.WorkDir)

	if len(req.Units) > 0 || len(req.Modules) > 0 {
		s.extractNaive(ctx, w, d, req, clone)
		return
	}
	s.extractDiff(ctx, w, d, req, clone)
}

// extractDiff is the Stage 8 default path: real change detection via mx diff.
func (s *Server) extractDiff(ctx context.Context, w http.ResponseWriter, d Deps, req extractRequest, clone gitops.CloneBothResult) {
	var warnings []string

	// A bootstrap binary, used only to read each side's Mendix version.
	// analyze-mpr is confirmed version-agnostic, which is what breaks the
	// "need the version to pick a binary, need a binary to read the version"
	// cycle. A failure here is a server misconfiguration (no binaries
	// installed under MxRoot), not a bad request.
	boot, err := d.MxHighest(s.MxRoot)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Errorf("no usable mx binary under %q: %w", s.MxRoot, err))
		return
	}

	headMpr, headInfo, err := d.PrepareMpr(ctx, boot, clone.HeadDir)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Errorf("prepare head .mpr: %w", err))
		return
	}
	baseMpr, baseInfo, err := d.PrepareMpr(ctx, boot, clone.BaseDir)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Errorf("prepare base .mpr: %w", err))
		return
	}

	// Fail fast on a version-migration commit rather than letting a raw
	// "$ID"/"Associations" parse exception surface out of diff or dump-mpr.
	// Either side is disqualifying: mx cannot parse the snapshot at all.
	if headInfo.HasProjectConversion {
		respondError(w, http.StatusUnprocessableEntity,
			&mx.ErrUnsupportedVersionMigrationCommit{MendixVersion: headInfo.MendixVersion})
		return
	}
	if baseInfo.HasProjectConversion {
		respondError(w, http.StatusUnprocessableEntity,
			&mx.ErrUnsupportedVersionMigrationCommit{MendixVersion: baseInfo.MendixVersion})
		return
	}

	// The head is what's under review, so its version selects the binary.
	// A mismatch is legal — a version bump that is not itself a migration
	// commit — but the reviewer should know the two sides disagree.
	if baseInfo.MendixVersion != headInfo.MendixVersion {
		warnings = append(warnings, fmt.Sprintf(
			"base is Mendix %s but head is %s; using the head binary for both",
			baseInfo.MendixVersion, headInfo.MendixVersion))
	}

	bin, err := d.MxResolve(s.MxRoot, headInfo.MendixVersion)
	if err != nil {
		// manual §1.3: fail loudly, never substitute a nearby version.
		respondError(w, http.StatusUnprocessableEntity,
			fmt.Errorf("unsupportedMendixVersion %s: %w", headInfo.MendixVersion, err))
		return
	}

	diff, err := d.Diff(ctx, bin, baseMpr, headMpr, filepath.Join(clone.WorkDir, "diff.json"))
	if err != nil {
		respondError(w, diffErrorStatus(err), err)
		return
	}
	if diff.ConflictsFound {
		// Exit code 2. Per manual, the output is still usable.
		warnings = append(warnings, "mx diff reported conflicts (exit 2); results are still usable")
	}

	baseNames, headNames, resolveWarnings := resolveBothSides(ctx, d, bin, baseMpr, headMpr, diff.UnitDifferences)
	warnings = append(warnings, resolveWarnings...)

	plans, planWarnings := planChangeUnits(diff.UnitDifferences, baseNames, headNames)
	warnings = append(warnings, planWarnings...)

	units, describeWarnings := describeChangeUnits(ctx, d, baseMpr, headMpr, plans)
	warnings = append(warnings, describeWarnings...)

	// Provenance. A failure to read the mxcli version must not sink an
	// otherwise good extraction.
	mxcliVer, err := d.MxcliVersion(ctx)
	if err != nil {
		warnings = append(warnings, "could not read mxcli version: "+err.Error())
	}

	writeJSON(w, http.StatusOK, extractResponse{
		RequestID:     req.RequestID,
		MendixVersion: headInfo.MendixVersion,
		// StorageFormat is intentionally empty: mx.AnalyzeResult does not
		// expose it, and inventing "MPRv2" would be a fabricated provenance
		// field. Populate once analyze-mpr's raw output is confirmed to carry it.
		StorageFormat: "",
		MxcliVersion:  mxcliVer,
		MxVersion:     bin.Version,
		ChangeUnits:   units,
		Warnings:      warnings,
	})
}

// extractNaive is the pre-Stage-8 path, reached only via the explicit
// Units/Modules escape hatch. It runs against the HEAD worktree — the request
// no longer carries a single `sha`, and head is the only defensible reading of
// "the app" when two commits are in play.
func (s *Server) extractNaive(ctx context.Context, w http.ResponseWriter, d Deps, req extractRequest, clone gitops.CloneBothResult) {
	mprPath, err := d.FindMpr(clone.HeadDir)
	if err != nil {
		respondError(w, http.StatusBadGateway, err)
		return
	}

	units := req.Units
	if len(units) == 0 {
		enumerated, err := enumerate(ctx, d, mprPath, req.Modules)
		if err != nil {
			respondError(w, http.StatusBadGateway, err)
			return
		}
		units = enumerated
	}

	unitResults, warnings := describeAll(ctx, d, mprPath, units)

	writeJSON(w, http.StatusOK, legacyExtractResponse{
		MprPath:  mprPath,
		Units:    unitResults,
		Warnings: warnings,
	})
}

// diffErrorStatus maps mx diff's typed errors onto HTTP status codes.
//
// The split that matters: a .mpr this binary cannot parse is the caller's
// problem (they picked the commits), while a generic diff failure or an
// exit code outside the documented table is ours.
func diffErrorStatus(err error) int {
	var unsupported *mx.ErrUnsupportedMendixVersion
	if errors.As(err, &unsupported) {
		return http.StatusUnprocessableEntity
	}
	return http.StatusInternalServerError
}

// ---------------------------------------------------------------------------
// Name resolution
// ---------------------------------------------------------------------------

// resolveBothSides runs dump-mpr once per side — head for everything that
// still exists there, base for everything that existed before. A unit renamed
// within the same commit therefore resolves to its old name in base and its
// new one in head, each valid in its own snapshot.
//
// A whole-call failure degrades to a warning rather than a 502. That follows
// manual §1.4's partial-success rule: units still carry type, changeKind and
// structuralDelta, which is a usable (if diminished) review payload, and
// failing the request would throw away a successful clone and diff.
func resolveBothSides(ctx context.Context, d Deps, bin mx.Binary, baseMpr, headMpr string, diffs []mx.UnitDifference) (base, head map[string]mx.ResolvedUnit, warnings []string) {
	types := distinctTypes(diffs)

	wantBase := map[string]bool{}
	wantHead := map[string]bool{}
	for _, ud := range diffs {
		if ud.Change != "Added" {
			wantBase[ud.ID] = true
		}
		if ud.Change != "Deleted" {
			wantHead[ud.ID] = true
		}
	}

	base = map[string]mx.ResolvedUnit{}
	head = map[string]mx.ResolvedUnit{}

	if len(wantBase) > 0 {
		got, err := d.ResolveQualifiedNames(ctx, bin, baseMpr, types, wantBase)
		if err != nil {
			warnings = append(warnings, "could not resolve base-side names: "+err.Error())
		} else {
			base = got
		}
	}
	if len(wantHead) > 0 {
		got, err := d.ResolveQualifiedNames(ctx, bin, headMpr, types, wantHead)
		if err != nil {
			warnings = append(warnings, "could not resolve head-side names: "+err.Error())
		} else {
			head = got
		}
	}
	return base, head, warnings
}

// distinctTypes returns the diff's unit types, sorted so the dump-mpr command
// line is deterministic — which makes failures reproducible and lets a test
// assert on the argument.
func distinctTypes(diffs []mx.UnitDifference) []string {
	seen := map[string]bool{}
	var out []string
	for _, ud := range diffs {
		if ud.Type != "" && !seen[ud.Type] {
			seen[ud.Type] = true
			out = append(out, ud.Type)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

// changePlan is the internal shape carrying BOTH sides' names through to the
// describe stage. The response has one qualifiedName; describing a renamed
// unit correctly needs two, so they cannot be collapsed before Step 8 runs.
type changePlan struct {
	unit      changeUnit
	mxcliType string // "" when this diff type has no mxcli equivalent
	baseName  string
	headName  string
}

func planChangeUnits(diffs []mx.UnitDifference, baseNames, headNames map[string]mx.ResolvedUnit) ([]changePlan, []string) {
	var warnings []string
	unmapped := map[string]bool{} // one warning per distinct type, not per unit

	plans := make([]changePlan, 0, len(diffs))
	for _, ud := range diffs {
		baseUnit, hasBase := baseNames[ud.ID]
		headUnit, hasHead := headNames[ud.ID]

		// Head is the current truth; fall back to base for a deleted unit.
		name, module, synthesized := headUnit.QualifiedName, headUnit.Module, headUnit.QualifiedNameSynthesized
		if !hasHead {
			name, module, synthesized = baseUnit.QualifiedName, baseUnit.Module, baseUnit.QualifiedNameSynthesized
		}

		var previous string
		if hasBase && hasHead && baseUnit.QualifiedName != headUnit.QualifiedName {
			previous = baseUnit.QualifiedName
		}

		if !hasBase && !hasHead {
			warnings = append(warnings, fmt.Sprintf(
				"could not resolve a qualified name for %s id %s (%s)", ud.Type, ud.ID, ud.Change))
		}

		// Three cases, and the difference between the last two is the whole
		// point: an expected no-MDL type stays silent, an unrecognised one
		// asks to be looked at.
		mxcliType, describable := diffTypeToMxcli[ud.Type]
		if !describable {
			if _, expected := knownNotDescribable[ud.Type]; !expected && !unmapped[ud.Type] {
				unmapped[ud.Type] = true
				warnings = append(warnings, fmt.Sprintf(
					"unrecognised change unit type %q — reported without MDL; "+
						"add it to diffTypeToMxcli or knownNotDescribable", ud.Type))
			}
		}

		plans = append(plans, changePlan{
			unit: changeUnit{
				Module:                   module,
				UnitType:                 ud.Type,
				QualifiedName:            name,
				PreviousQualifiedName:    previous,
				QualifiedNameSynthesized: synthesized,
				ChangeKind:               ud.Change,
				StructuralDelta:          ud.Raw,
			},
			mxcliType: mxcliType,
			baseName:  baseUnit.QualifiedName,
			headName:  headUnit.QualifiedName,
		})
	}
	return plans, warnings
}

// ---------------------------------------------------------------------------
// Step 8 — targeted describe, before and after
// ---------------------------------------------------------------------------

type describeOutcome struct {
	unit     changeUnit
	warnings []string
}

// describeChangeUnits renders each changed unit from the side(s) where it
// exists. One goroutine per unit, each doing up to two Describe calls; both
// block on internal/mxcli's global semaphore, so no limit is applied here —
// see MERA-extractor-design-notes.md §3 for why the cap lives there.
//
// Each goroutine writes only to its own index, so no mutex is needed and
// output order matches input order regardless of completion order.
func describeChangeUnits(ctx context.Context, d Deps, baseMpr, headMpr string, plans []changePlan) ([]changeUnit, []string) {
	units := make([]changeUnit, 0, len(plans))
	if len(plans) == 0 {
		return units, nil
	}

	outcomes := make([]describeOutcome, len(plans))

	var wg sync.WaitGroup
	wg.Add(len(plans))
	for i, p := range plans {
		go func(i int, p changePlan) {
			defer wg.Done()
			outcomes[i] = describeOne(ctx, d, baseMpr, headMpr, p)
		}(i, p)
	}
	wg.Wait()

	var warnings []string
	for _, o := range outcomes {
		units = append(units, o.unit)
		warnings = append(warnings, o.warnings...)
	}
	return units, warnings
}

func describeOne(ctx context.Context, d Deps, baseMpr, headMpr string, p changePlan) describeOutcome {
	out := describeOutcome{unit: p.unit}

	// Nothing to render for a type mxcli doesn't know. Already warned once
	// per distinct type in planChangeUnits — don't repeat it per unit.
	if p.mxcliType == "" {
		out.unit.TokenEstimate = tokenEstimate(string(p.unit.StructuralDelta))
		return out
	}

	if p.unit.ChangeKind != "Added" && p.baseName != "" {
		mdl, err := d.Describe(ctx, mxcli.DescribeRequest{
			MprPath: baseMpr, UnitType: p.mxcliType, QualifiedName: p.baseName,
		})
		if err != nil {
			// manual §1.4's core rule: one bad unit never fails the batch.
			out.warnings = append(out.warnings, "describe base "+p.baseName+": "+err.Error())
		} else {
			out.unit.BeforeMdl = mdl
		}
	}

	if p.unit.ChangeKind != "Deleted" && p.headName != "" {
		mdl, err := d.Describe(ctx, mxcli.DescribeRequest{
			MprPath: headMpr, UnitType: p.mxcliType, QualifiedName: p.headName,
		})
		if err != nil {
			out.warnings = append(out.warnings, "describe head "+p.headName+": "+err.Error())
		} else {
			out.unit.AfterMdl = mdl
		}
	}

	out.unit.TokenEstimate = tokenEstimate(out.unit.BeforeMdl, out.unit.AfterMdl, string(p.unit.StructuralDelta))
	return out
}

// ---------------------------------------------------------------------------
// Step 9 — token estimate
// ---------------------------------------------------------------------------

// tokenEstimateDivisor is manual §1.5's explicit placeholder — "measure on
// your own corpus later". It only has to be right enough to make batching
// decisions; the model API returns exact counts anyway.
const tokenEstimateDivisor = 3.6

func tokenEstimate(parts ...string) int {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	return int(float64(total) / tokenEstimateDivisor)
}

// ---------------------------------------------------------------------------
// Naive path (unchanged behaviour, now reached only via the escape hatch)
// ---------------------------------------------------------------------------

type legacyDescribeOutcome struct {
	result  unitResult
	warning string
}

func describeAll(ctx context.Context, d Deps, mprPath string, units []unitRequest) ([]unitResult, []string) {
	if len(units) == 0 {
		return nil, nil
	}

	outcomes := make([]legacyDescribeOutcome, len(units))

	var wg sync.WaitGroup
	wg.Add(len(units))
	for i, u := range units {
		go func(i int, u unitRequest) {
			defer wg.Done()
			mdl, err := d.Describe(ctx, mxcli.DescribeRequest{
				MprPath: mprPath, UnitType: u.UnitType, QualifiedName: u.QualifiedName,
			})
			res := unitResult{QualifiedName: u.QualifiedName}
			var warn string
			if err != nil {
				res.Warning = err.Error()
				warn = u.QualifiedName + ": " + err.Error()
			} else {
				res.Mdl = mdl
			}
			outcomes[i] = legacyDescribeOutcome{result: res, warning: warn}
		}(i, u)
	}
	wg.Wait()

	results := make([]unitResult, len(outcomes))
	var warnings []string
	for i, o := range outcomes {
		results[i] = o.result
		if o.warning != "" {
			warnings = append(warnings, o.warning)
		}
	}
	return results, warnings
}

// enumerate lists units across mxcli.DefaultUnitTypes — the naive path.
//
// modules is optional. Empty means every module — one unscoped ListUnits call
// per type. A non-empty list scopes each type to just those modules, which
// costs one ListUnits call per (type, module) pair but returns far fewer units
// downstream, since every extra unit here is an extra `mxcli describe`
// subprocess spawn later in the same request.
func enumerate(ctx context.Context, d Deps, mprPath string, modules []string) ([]unitRequest, error) {
	scopes := modules
	if len(scopes) == 0 {
		scopes = []string{""} // one unscoped pass per type — every module
	}

	var all []unitRequest
	for _, unitType := range mxcli.DefaultUnitTypes {
		for _, module := range scopes {
			units, err := d.ListUnits(ctx, mprPath, unitType, module)
			if err != nil {
				return nil, fmt.Errorf("list %s (module=%q): %w", unitType, module, err)
			}
			for _, u := range units {
				all = append(all, unitRequest{UnitType: unitType, QualifiedName: u.QualifiedName})
			}
		}
	}
	return all, nil
}
