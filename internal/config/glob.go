package config

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// MatchGlob reports whether value matches a tool-gate glob. A pattern without
// a ** segment is path.Match exactly: * and ? never cross a /, [...] is a
// class. A segment that is exactly ** matches whole segments at any depth:
// zero or more in front of or between other segments (src/**/*.go matches
// src/main.go), one or more at the end (src/** is everything inside src/, not
// src itself). Every other segment of such a pattern is a path.Match pattern
// for one segment, so ** elsewhere in a segment is two *s, as before.
//
// A pattern with a ** segment never matches a value with a .. segment
// (between / or \), so a rule for a directory tree cannot be walked out of
// with src/../secrets. The value is not cleaned: . and empty segments are
// matched as written. A malformed pattern returns path.ErrBadPattern.
func MatchGlob(pattern, value string) (bool, error) {
	if !hasDoubleStar(pattern) {
		return path.Match(pattern, value)
	}
	segs := strings.Split(pattern, "/")
	for _, s := range segs {
		if _, err := path.Match(s, ""); err != nil {
			return false, err
		}
	}
	if hasParentSegment(value) {
		return false, nil
	}
	vals := strings.Split(value, "/")
	// at[j]: the pattern segments so far match vals[:j].
	at := make([]bool, len(vals)+1)
	at[0] = true
	for i, s := range segs {
		next := make([]bool, len(vals)+1)
		if s != "**" {
			for j, v := range vals {
				if at[j] {
					next[j+1], _ = path.Match(s, v)
				}
			}
		} else if from := slices.Index(at, true); from >= 0 {
			if i == len(segs)-1 {
				from++ // at the end: one segment or more
			}
			for k := from; k <= len(vals); k++ {
				next[k] = true
			}
		}
		at = next
	}
	return at[len(vals)], nil
}

// CheckGlob reports what draftcat validate refuses in a tool-gate glob: a
// pattern MatchGlob cannot read, or a ** pattern with a .. segment of its own,
// which could never match. nil means every value is matched against it as
// MatchGlob describes.
func CheckGlob(pattern string) error {
	if _, err := MatchGlob(pattern, ""); err != nil {
		return fmt.Errorf("bad pattern %q", pattern)
	}
	if hasDoubleStar(pattern) && hasParentSegment(pattern) {
		return fmt.Errorf("pattern %q has a .. segment, and a ** glob never matches a path with one", pattern)
	}
	return nil
}

// hasDoubleStar reports whether a glob has a ** segment, the only kind that
// matches across a /.
func hasDoubleStar(pattern string) bool {
	return slices.Contains(strings.Split(pattern, "/"), "**")
}

// hasParentSegment reports whether a path has a .. segment, split on / or \.
func hasParentSegment(p string) bool {
	return slices.Contains(strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }), "..")
}
