// internal/mx/version.go
package mx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Binary struct {
	Version string
	Path    string // .../<version>/modeler/mx
}

// ListVersions returns every usable mx installation under mxRoot, sorted
// ascending by compareVersions. It is the single source of truth for "what
// can this container actually do" — Resolve, Highest and GET /health all go
// through it, so none of them can disagree with the others.
//
// That shared path is the point. /health reporting a supported version that
// Resolve then refuses is the worst shape this can fail in: a capability
// endpoint that lies, discovered three layers away in a 422.
//
// A directory qualifies only if BOTH hold:
//
//   - its name is version-shaped (see isVersionLike) — otherwise the stray
//     `tmp/` staging folder left in .mx-binaries gets advertised as a
//     supported Mendix version, and parseVersion would happily sort it as
//     0.0.0 because it ignores Atoi errors;
//   - <name>/modeler/mx exists and is a regular file — a half-extracted or
//     half-trimmed version directory is not a usable version.
//
// An unreadable mxRoot is an error. A readable but EMPTY one is not: it
// returns (nil, nil) so a caller can tell "misconfigured" from "nothing
// installed yet" and report each differently.
func ListVersions(mxRoot string) ([]Binary, error) {
	entries, err := os.ReadDir(mxRoot)
	if err != nil {
		abs, absErr := filepath.Abs(mxRoot)
		if absErr == nil && abs != mxRoot {
			return nil, fmt.Errorf("mx: reading %q (resolved to %q): %w", mxRoot, abs, err)
		}
		return nil, fmt.Errorf("mx: reading %q: %w", mxRoot, err)
	}

	var found []Binary
	for _, e := range entries {
		if !e.IsDir() || !isVersionLike(e.Name()) {
			continue
		}
		path := filepath.Join(mxRoot, e.Name(), "modeler", "mx")
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		found = append(found, Binary{Version: e.Name(), Path: path})
	}

	sort.Slice(found, func(i, j int) bool {
		return compareVersions(found[i].Version, found[j].Version) < 0
	})
	return found, nil
}

// isVersionLike accepts names whose first two dot-separated segments are
// plain digit runs: "11.13.0" and "11.6" yes, "tmp" and "beta.1" no.
//
// Deliberately stricter than parseVersion, which never fails — it discards
// Atoi errors, so every non-numeric name silently becomes 0.0.0. That is
// fine for ordering two known versions and wrong for deciding whether a
// directory is a version at all.
func isVersionLike(name string) bool {
	parts := strings.SplitN(name, ".", 4)
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts[:2] {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// Resolve does an EXACT match only — manual §1.3's "fail loudly, don't
// fall back" rule. No nearby-version substitution.
//
// It matches within ListVersions rather than stat-ing a path directly, so it
// accepts exactly the set /health advertises. The error names what IS
// installed, because "no binary for 11.11.0" and "no binaries at all" call
// for very different fixes and used to look identical.
func Resolve(mxRoot, mendixVersion string) (Binary, error) {
	installed, err := ListVersions(mxRoot)
	if err != nil {
		return Binary{}, err
	}
	for _, b := range installed {
		if b.Version == mendixVersion {
			return b, nil
		}
	}
	if len(installed) == 0 {
		return Binary{}, fmt.Errorf("mx: no versions installed under %q (wanted %q)", mxRoot, mendixVersion)
	}
	return Binary{}, fmt.Errorf("mx: no binary installed for Mendix version %q under %q; installed: %s",
		mendixVersion, mxRoot, strings.Join(versionStrings(installed), ", "))
}

// Highest returns the binary for whichever installed version sorts
// numerically highest — NOT a string sort (see compareVersions).
func Highest(mxRoot string) (Binary, error) {
	installed, err := ListVersions(mxRoot)
	if err != nil {
		return Binary{}, err
	}
	if len(installed) == 0 {
		return Binary{}, fmt.Errorf("mx: no versions installed under %q", mxRoot)
	}
	return installed[len(installed)-1], nil // ListVersions sorts ascending
}

// versionStrings is the projection /health needs, and what Resolve's error
// text uses.
func versionStrings(bins []Binary) []string {
	out := make([]string, len(bins))
	for i, b := range bins {
		out[i] = b.Version
	}
	return out
}

// parsedVersion is a.b[.c[.d]] — major and minor are always present and
// numeric; patch is optional and numeric; addition is optional, only ever
// present alongside patch, and never assumed numeric (it can be a build
// number, a tag like "beta-1", or a hash).
type parsedVersion struct {
	major, minor, patch int
	addition            string
}

func parseVersion(v string) parsedVersion {
	parts := strings.SplitN(v, ".", 4)
	var pv parsedVersion
	if len(parts) > 0 {
		pv.major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) > 1 {
		pv.minor, _ = strconv.Atoi(parts[1])
	}
	if len(parts) > 2 {
		pv.patch, _ = strconv.Atoi(parts[2])
	}
	if len(parts) > 3 {
		pv.addition = parts[3]
	}
	return pv
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// compareVersions compares two Mendix version strings numerically on
// major/minor/patch and, only as a final tiebreaker, string-compares the
// optional addition (d). Absent patch sorts as 0 ("11.6" == "11.6.0").
// Absent addition sorts BELOW any present addition at the same
// major.minor.patch — confirmed with Jord: d used to carry a build
// number (higher = later build), and is now mostly seen as a beta tag
// like "beta-1" (alphabetical ordering is fine there). Neither will
// realistically show up in MERA's version matrix — it targets deployed
// apps, not betas — so correctness of d's ordering is low-stakes;
// robustness against garbage in that field is the actual requirement.
func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)

	if pa.major != pb.major {
		return cmpInt(pa.major, pb.major)
	}
	if pa.minor != pb.minor {
		return cmpInt(pa.minor, pb.minor)
	}
	if pa.patch != pb.patch {
		return cmpInt(pa.patch, pb.patch)
	}
	switch {
	case pa.addition == "" && pb.addition == "":
		return 0
	case pa.addition == "":
		return -1
	case pb.addition == "":
		return 1
	default:
		return strings.Compare(pa.addition, pb.addition)
	}
}
