package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// What changed in a tool definition.
//
// A pin by hash can only say that the definition changed. A definition_file
// holds the approved definition itself, so the gate names what changed: the
// top-level fields (description, inputSchema, annotations, ...) and, below
// them, each path added, removed or changed, down to a property's type, an
// enum value or a required argument. The summary lands in an approval prompt
// and a denial reason, so it is bounded: a capped number of entries, values
// only when they are short scalars, and an excerpt rather than the whole of a
// changed text.

const (
	// maxDefinitionChanges caps the entries a summary names; the rest are
	// counted.
	maxDefinitionChanges = 8
	// maxShownValue is the longest scalar shown in full, in runes.
	maxShownValue = 24
	// maxExcerpt is how much of a changed long text is shown, in runes.
	maxExcerpt = 32
	// maxShownMembers caps the members one array entry names.
	maxShownMembers = 4
	// maxKey is the longest object key a path shows, in runes.
	maxKey = 40
	// maxEntry is the longest single entry, in runes; a deep path loses its
	// middle.
	maxEntry = 160
)

// plainName is a key or member shown without quotes.
var plainName = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$-]*$`)

// definitionChanges lists what differs from the pinned definition to the one
// sent, one entry per changed path, in path order. Equal definitions have
// none. Values compare as canonical JSON, the form the pin hashes.
func definitionChanges(pinned, sent map[string]interface{}) []string {
	var out []string
	diffObjects("", pinned, sent, &out)
	for i, e := range out {
		out[i] = clipMiddle(e, maxEntry)
	}
	return out
}

// summarizeDefinitionChanges is the one line a prompt or a reason shows: the
// first maxDefinitionChanges entries and how many more there are.
func summarizeDefinitionChanges(changes []string) string {
	if len(changes) == 0 {
		return "definition changed"
	}
	shown := changes
	if len(shown) > maxDefinitionChanges {
		shown = shown[:maxDefinitionChanges]
	}
	s := "definition changed: " + strings.Join(shown, "; ")
	if more := len(changes) - len(shown); more > 0 {
		s += fmt.Sprintf("; and %d more", more)
	}
	return s
}

func diffObjects(path string, old, cur map[string]interface{}, out *[]string) {
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range cur {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		p := joinPath(path, k)
		o, inOld := old[k]
		c, inCur := cur[k]
		switch {
		case !inOld:
			entry := p + " added"
			if list, ok := c.([]interface{}); ok && allScalars(list) {
				entry += ": " + showList(list)
			} else if isShortScalar(c) {
				entry += ": " + showValue(c)
			}
			*out = append(*out, entry)
		case !inCur:
			*out = append(*out, p+" removed")
		default:
			diffValues(p, o, c, out)
		}
	}
}

// diffValues descends into objects and arrays and compares everything else
// as canonical JSON, so each value is encoded once however deep it sits.
func diffValues(path string, old, cur interface{}, out *[]string) {
	switch o := old.(type) {
	case map[string]interface{}:
		if c, ok := cur.(map[string]interface{}); ok {
			diffObjects(path, o, c, out)
			return
		}
	case []interface{}:
		if c, ok := cur.([]interface{}); ok {
			diffArrays(path, o, c, out)
			return
		}
	}
	if canonicalJSON(old) == canonicalJSON(cur) {
		return
	}
	o, oldText := old.(string)
	c, curText := cur.(string)
	if oldText && curText && (utf8.RuneCountInString(o) > maxShownValue || utf8.RuneCountInString(c) > maxShownValue) {
		*out = append(*out, path+" "+textChange(o, c))
		return
	}
	*out = append(*out, path+" "+showValue(old)+" -> "+showValue(cur))
}

// diffArrays names the members added and removed when both arrays hold only
// scalars (required, enum, a list of types), and otherwise compares item by
// item when the length is the same.
func diffArrays(path string, old, cur []interface{}, out *[]string) {
	if allScalars(old) && allScalars(cur) {
		if sameScalars(old, cur) {
			return
		}
		added, removed := memberChanges(old, cur)
		switch {
		case len(added)+len(removed) > 0:
			*out = append(*out, path+" "+showMembers(added, removed))
		case len(old) == len(cur):
			*out = append(*out, path+" reordered")
		default:
			*out = append(*out, fmt.Sprintf("%s %d -> %d items", path, len(old), len(cur)))
		}
		return
	}
	if len(old) != len(cur) {
		*out = append(*out, fmt.Sprintf("%s %d -> %d items", path, len(old), len(cur)))
		return
	}
	for i := range old {
		diffValues(fmt.Sprintf("%s[%d]", path, i), old[i], cur[i], out)
	}
}

func sameScalars(old, cur []interface{}) bool {
	if len(old) != len(cur) {
		return false
	}
	for i := range old {
		if canonicalJSON(old[i]) != canonicalJSON(cur[i]) {
			return false
		}
	}
	return true
}

func memberChanges(old, cur []interface{}) (added, removed []string) {
	set := func(list []interface{}) map[string]bool {
		s := map[string]bool{}
		for _, v := range list {
			s[canonicalJSON(v)] = true
		}
		return s
	}
	missing := func(list []interface{}, other map[string]bool) []string {
		var names []string
		seen := map[string]bool{}
		for _, v := range list {
			k := canonicalJSON(v)
			if !other[k] && !seen[k] {
				seen[k] = true
				names = append(names, showMember(v))
			}
		}
		return names
	}
	return missing(cur, set(old)), missing(old, set(cur))
}

func showMembers(added, removed []string) string {
	parts := make([]string, 0, len(added)+len(removed))
	for _, a := range added {
		parts = append(parts, "+"+a)
	}
	for _, r := range removed {
		parts = append(parts, "-"+r)
	}
	if more := len(parts) - maxShownMembers; more > 0 {
		parts = append(parts[:maxShownMembers], fmt.Sprintf("and %d more", more))
	}
	return strings.Join(parts, " ")
}

// showList renders a list of scalars, capped like showMembers.
func showList(list []interface{}) string {
	parts := make([]string, 0, min(len(list), maxShownMembers+1))
	for i, v := range list {
		if i == maxShownMembers {
			parts = append(parts, fmt.Sprintf("and %d more", len(list)-i))
			break
		}
		parts = append(parts, showMember(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// textChange says where a long text changed and shows a short excerpt of the
// new text from there, never the whole of it.
func textChange(old, cur string) string {
	o, c := []rune(old), []rune(cur)
	i := 0
	for i < len(o) && i < len(c) && o[i] == c[i] {
		i++
	}
	if i == len(c) {
		return fmt.Sprintf("cut to %d chars (was %d)", len(c), len(o))
	}
	end := min(i+maxExcerpt, len(c))
	excerpt := strconv.Quote(string(c[i:end]))
	if end < len(c) {
		excerpt += "…"
	}
	return fmt.Sprintf("changed at char %d: %s", i+1, excerpt)
}

// showValue renders a value for a summary: a short scalar as JSON, anything
// else by its kind.
func showValue(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case string:
		if utf8.RuneCountInString(x) > maxShownValue {
			return "text"
		}
		return strconv.Quote(x)
	}
	s := canonicalJSON(v)
	if utf8.RuneCountInString(s) > maxShownValue {
		return "value"
	}
	return s
}

// showMember renders an array member: a plain name bare, anything else as a
// value.
func showMember(v interface{}) string {
	if s, ok := v.(string); ok && plainName.MatchString(s) && len(s) <= maxShownValue {
		return s
	}
	return showValue(v)
}

func isShortScalar(v interface{}) bool {
	switch x := v.(type) {
	case map[string]interface{}, []interface{}:
		return false
	case string:
		return utf8.RuneCountInString(x) <= maxShownValue
	}
	return utf8.RuneCountInString(canonicalJSON(v)) <= maxShownValue
}

func allScalars(list []interface{}) bool {
	for _, v := range list {
		switch v.(type) {
		case map[string]interface{}, []interface{}:
			return false
		}
	}
	return true
}

// joinPath adds a key to a dotted path; a key that is not a plain name, or is
// long, is quoted (and clipped) in brackets, so no key can break the line.
func joinPath(path, key string) string {
	if !plainName.MatchString(key) || utf8.RuneCountInString(key) > maxKey {
		if r := []rune(key); len(r) > maxKey {
			key = string(r[:maxKey]) + "…"
		}
		return path + "[" + strconv.Quote(key) + "]"
	}
	if path == "" {
		return key
	}
	return path + "." + key
}

func clipMiddle(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	head := limit / 3
	return string(r[:head]) + "…" + string(r[len(r)-(limit-head-1):])
}

func canonicalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%T:%v", v, v)
	}
	return string(b)
}
