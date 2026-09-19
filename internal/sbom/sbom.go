// Package sbom parses the software bill of materials attached to a bootc
// image and diffs two of them, so ChairLift can answer "what actually
// changes if I take this update?" from the registry rather than from a
// hand-written changelog.
//
// Everything in this file is pure: it turns bytes into a package map and two
// package maps into a diff. The registry round-trip that produces those bytes
// lives in fetch.go behind a seam, so the diff is covered by fixtures rather
// than by a network call.
package sbom

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Packages maps a package name to its version.
type Packages map[string]string

// syftDocument is the shape Universal Blue actually attaches.
type syftDocument struct {
	Artifacts []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"artifacts"`
}

// spdxDocument is the shape the artifact type advertises.
type spdxDocument struct {
	Packages []struct {
		Name        string `json:"name"`
		VersionInfo string `json:"versionInfo"`
	} `json:"packages"`
}

// Parse reads an attached SBOM into a package map.
//
// The referrer is advertised as `application/vnd.spdx+json`, but Universal
// Blue attaches Syft JSON under that artifact type — verified against
// ghcr.io/ublue-os/bluefin:stable on 2026-08-17, whose blob has a top-level
// `artifacts` array and no `packages` at all. Both shapes are therefore
// accepted, and a document matching neither is an error rather than an empty
// map: every field in both schemas is optional, so a silent zero-package
// parse would render an empty changelog on every machine with nothing
// anywhere in the chain reporting a failure. finupdate shipped exactly that
// bug and documents it in its own source.
func Parse(data []byte) (Packages, error) {
	packages := make(Packages)

	var syft syftDocument
	if err := json.Unmarshal(data, &syft); err == nil {
		for _, artifact := range syft.Artifacts {
			if artifact.Name == "" {
				continue
			}
			packages[artifact.Name] = artifact.Version
		}
	}
	if len(packages) > 0 {
		return packages, nil
	}

	var spdx spdxDocument
	if err := json.Unmarshal(data, &spdx); err != nil {
		return nil, fmt.Errorf("parsing SBOM: %w", err)
	}
	for _, pkg := range spdx.Packages {
		if pkg.Name == "" {
			continue
		}
		packages[pkg.Name] = pkg.VersionInfo
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("SBOM contains no packages in either Syft or SPDX form")
	}
	return packages, nil
}

// Change is one package that differs between two images.
type Change struct {
	Name string
	// From is the version on the running image, empty for an addition.
	From string
	// To is the version on the staged image, empty for a removal.
	To string
}

// Result is the categorized difference between two images.
type Result struct {
	// Upgraded, Downgraded, and Changed partition the packages present in
	// both images with differing versions. Changed holds the pairs whose
	// order could not be established — two git hashes, or versions in
	// unrelated formats — because presenting an unknown direction as an
	// upgrade is how a rollback comes to look like an update.
	Upgraded   []Change
	Downgraded []Change
	Changed    []Change
	Added      []Change
	Removed    []Change
}

// Total returns the number of packages that differ.
func (r Result) Total() int {
	return len(r.Upgraded) + len(r.Downgraded) + len(r.Changed) + len(r.Added) + len(r.Removed)
}

// Empty reports whether the two images carry identical packages.
func (r Result) Empty() bool {
	return r.Total() == 0
}

// Diff categorizes the difference between the running and staged images.
// Every returned slice is sorted by package name, so the same pair of images
// always produces the same list.
func Diff(from, to Packages) Result {
	var result Result

	for name, fromVersion := range from {
		toVersion, present := to[name]
		if !present {
			result.Removed = append(result.Removed, Change{Name: name, From: fromVersion})
			continue
		}
		if toVersion == fromVersion {
			continue
		}
		change := Change{Name: name, From: fromVersion, To: toVersion}
		switch CompareVersions(fromVersion, toVersion) {
		case -1:
			result.Upgraded = append(result.Upgraded, change)
		case 1:
			result.Downgraded = append(result.Downgraded, change)
		default:
			result.Changed = append(result.Changed, change)
		}
	}

	for name, toVersion := range to {
		if _, present := from[name]; !present {
			result.Added = append(result.Added, Change{Name: name, To: toVersion})
		}
	}

	for _, changes := range [][]Change{result.Upgraded, result.Downgraded, result.Changed, result.Added, result.Removed} {
		sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	}
	return result
}

// CompareVersions orders two version strings, returning -1 when a sorts
// before b, 1 when it sorts after, and 0 when they are equal or when the
// comparison cannot be made.
//
// It parses each version as an RPM [epoch:]version[-release] (EVR) and applies
// RPM-compatible epoch, version, and release ordering (including tilde and caret
// semantics). It deliberately does not try to order two commit hashes or
// unsupported EVRs — those return 0 and land in Result.Changed, which is honest
// about not knowing rather than guessing a direction.
func CompareVersions(a, b string) int {
	if a == b {
		return 0
	}
	// A hash is not a version, and one hash is not "newer" than another.
	if isHash(a) || isHash(b) {
		return 0
	}

	evrA, okA := parseEVR(a)
	evrB, okB := parseEVR(b)
	if !okA || !okB {
		return 0
	}

	// Compare Epoch: implicit "0" if omitted or empty
	epochA := evrA.epoch
	if epochA == "" {
		epochA = "0"
	}
	epochB := evrB.epoch
	if epochB == "" {
		epochB = "0"
	}
	if rc := rpmvercmp(epochA, epochB); rc != 0 {
		return rc
	}

	// Compare Version
	if rc := rpmvercmp(evrA.version, evrB.version); rc != 0 {
		return rc
	}

	// Compare Release
	return compareValues(evrA.release, evrB.release)
}

type evr struct {
	epoch   string
	version string
	release string
}

func parseEVR(s string) (evr, bool) {
	if s == "" {
		return evr{}, false
	}

	// Must be ASCII and contain no whitespace or control characters.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c > '~' {
			return evr{}, false
		}
	}

	var parsed evr
	rest := s

	colonIdx := strings.IndexByte(s, ':')
	if colonIdx != -1 {
		// Only one epoch delimiter allowed.
		if strings.IndexByte(s[colonIdx+1:], ':') != -1 {
			return evr{}, false
		}
		epochPart := s[:colonIdx]
		// Epoch must be numeric digits (or empty, which RPM treats as 0).
		for i := 0; i < len(epochPart); i++ {
			if !isDigit(epochPart[i]) {
				return evr{}, false
			}
		}
		parsed.epoch = epochPart
		rest = s[colonIdx+1:]
	}

	if rest == "" {
		return evr{}, false
	}

	dashIdx := strings.LastIndexByte(rest, '-')
	if dashIdx != -1 {
		parsed.version = rest[:dashIdx]
		parsed.release = rest[dashIdx+1:]
		if parsed.version == "" || parsed.release == "" || !hasAlnum(parsed.version) || !hasAlnum(parsed.release) {
			return evr{}, false
		}
	} else {
		parsed.version = rest
		if !hasAlnum(parsed.version) {
			return evr{}, false
		}
	}

	return parsed, true
}

func compareValues(s1, s2 string) int {
	if s1 == "" && s2 == "" {
		return 0
	}
	if s1 != "" && s2 == "" {
		return 1
	}
	if s1 == "" && s2 != "" {
		return -1
	}
	return rpmvercmp(s1, s2)
}

// rpmvercmp implements RPM's rpmvercmp algorithm for comparing two version
// or release strings.
// Returns -1 when a < b, 1 when a > b, and 0 when a == b.
func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}

	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for i < len(a) && !isAlnum(a[i]) && a[i] != '~' && a[i] != '^' {
			i++
		}
		for j < len(b) && !isAlnum(b[j]) && b[j] != '~' && b[j] != '^' {
			j++
		}

		// Handle tilde separator: sorts before everything else.
		if (i < len(a) && a[i] == '~') || (j < len(b) && b[j] == '~') {
			if i >= len(a) || a[i] != '~' {
				return 1
			}
			if j >= len(b) || b[j] != '~' {
				return -1
			}
			i++
			j++
			continue
		}

		// Handle caret separator: sorts after base version (empty segment),
		// but before any other character.
		if (i < len(a) && a[i] == '^') || (j < len(b) && b[j] == '^') {
			if i >= len(a) {
				return -1
			}
			if j >= len(b) {
				return 1
			}
			if a[i] != '^' {
				return 1
			}
			if b[j] != '^' {
				return -1
			}
			i++
			j++
			continue
		}

		// If we ran to the end of either string, break.
		if i >= len(a) || j >= len(b) {
			break
		}

		isNum := false
		startI, startJ := i, j
		if isDigit(a[i]) {
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			isNum = true
		} else {
			for i < len(a) && isAlpha(a[i]) {
				i++
			}
			for j < len(b) && isAlpha(b[j]) {
				j++
			}
			isNum = false
		}

		segA := a[startI:i]
		segB := b[startJ:j]

		// If two version segments are different types (one numeric, one alpha):
		// numeric segments are always newer than alpha segments.
		if len(segB) == 0 {
			if isNum {
				return 1
			}
			return -1
		}

		if isNum {
			// Strip leading zeros.
			trimmedA := strings.TrimLeft(segA, "0")
			trimmedB := strings.TrimLeft(segB, "0")

			// Whichever number has more digits wins.
			if len(trimmedA) > len(trimmedB) {
				return 1
			}
			if len(trimmedB) > len(trimmedA) {
				return -1
			}

			if trimmedA > trimmedB {
				return 1
			}
			if trimmedA < trimmedB {
				return -1
			}
		} else {
			if segA > segB {
				return 1
			}
			if segA < segB {
				return -1
			}
		}
	}

	if i >= len(a) && j >= len(b) {
		return 0
	}
	if i >= len(a) {
		return -1
	}
	return 1
}

func isAlnum(b byte) bool {
	return isDigit(b) || isAlpha(b)
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func hasAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		if isAlnum(s[i]) {
			return true
		}
	}
	return false
}

// isHash reports whether a version is a git commit or content hash rather
// than a version number.
func isHash(version string) bool {
	if len(version) < 32 {
		return false
	}
	for _, r := range version {
		if !unicode.Is(unicode.Hex_Digit, r) {
			return false
		}
	}
	return true
}
