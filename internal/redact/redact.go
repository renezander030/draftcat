// Package redact keeps the engine's own credentials out of everything it
// writes for people and machines to read: the process log, operator
// notifications, persisted run errors and exported spans.
//
// The engine knows its secrets exactly — each one is read from a named
// environment variable — so redaction is a literal replace of registered
// values, not a guess at what a credential looks like. A value is registered
// once at startup and every output path runs through String or Writer.
package redact

import (
	"io"
	"sort"
	"strings"
	"sync"
)

// MinLen is the shortest value that is registered. Shorter strings are too
// likely to occur in ordinary text, and a replace would mangle it while
// protecting nothing worth protecting.
const MinLen = 8

type entry struct {
	value string
	label string
}

var (
	mu      sync.RWMutex
	entries []entry
)

// Register records a secret value under a label (usually the environment
// variable it came from). It is replaced by "[REDACTED:<label>]" wherever it
// appears. Values shorter than MinLen and duplicates are ignored.
func Register(label, value string) {
	value = strings.TrimSpace(value)
	if len(value) < MinLen {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range entries {
		if e.value == value {
			return
		}
	}
	entries = append(entries, entry{value: value, label: label})
	// Longest first, so a secret that contains another is replaced whole.
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].value) > len(entries[j].value) })
}

// Reset forgets every registered value. Test-only.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	entries = nil
}

// Count reports how many values are registered.
func Count() int {
	mu.RLock()
	defer mu.RUnlock()
	return len(entries)
}

// String returns s with every registered value replaced.
func String(s string) string {
	mu.RLock()
	defer mu.RUnlock()
	for _, e := range entries {
		if strings.Contains(s, e.value) {
			s = strings.ReplaceAll(s, e.value, "[REDACTED:"+e.label+"]")
		}
	}
	return s
}

// Bytes is String for byte slices.
func Bytes(b []byte) []byte {
	if Count() == 0 {
		return b
	}
	return []byte(String(string(b)))
}

// Writer wraps w so every Write is redacted first. It is meant for line
// writers such as the standard logger, which hand over one complete line per
// Write; a secret split across two Writes is not reassembled.
func Writer(w io.Writer) io.Writer { return writer{w} }

type writer struct{ w io.Writer }

func (r writer) Write(p []byte) (int, error) {
	if _, err := r.w.Write(Bytes(p)); err != nil {
		return 0, err
	}
	// Report the caller's length: the redacted line may differ in size, and a
	// short count would make the logger treat a successful write as failed.
	return len(p), nil
}
