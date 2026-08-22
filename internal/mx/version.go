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

// Resolve does an EXACT match only — manual §1.3's "fail loudly, don't
// fall back" rule. No nearby-version substitution.
func Resolve(mxRoot, mendixVersion string) (Binary, error) {
	path := filepath.Join(mxRoot, mendixVersion, "modeler", "mx")
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return Binary{}, fmt.Errorf("mx: no binary installed for Mendix version %q under %q", mendixVersion, mxRoot)
	}
	return Binary{Version: mendixVersion, Path: path}, nil
}

// Highest returns the binary for whichever installed version sorts
// numerically highest — NOT a string sort (see compareVersions).
func Highest(mxRoot string) (Binary, error) {
	entries, err := os.ReadDir(mxRoot)
	if err != nil {
		return Binary{}, fmt.Errorf("mx: reading %q: %w", mxRoot, err)
	}

	var versions []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(mxRoot, e.Name(), "modeler", "mx")
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			continue
		}
		versions = append(versions, e.Name())
	}
	if len(versions) == 0 {
		return Binary{}, fmt.Errorf("mx: no versions installed under %q", mxRoot)
	}

	sort.Slice(versions, func(i, j int) bool {
		return compareVersions(versions[i], versions[j]) < 0
	})
	highest := versions[len(versions)-1]
	return Binary{Version: highest, Path: filepath.Join(mxRoot, highest, "modeler", "mx")}, nil
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
