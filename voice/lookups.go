//go:build voice

package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type LookupFunc func(ctx context.Context, callerPhone string) (map[string]any, error)

type PreCallResult struct {
	Workflow    string          `json:"workflow"`
	ContextVars json.RawMessage `json:"context_vars,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type LookupRunner struct {
	cfg      PreCallConfig
	cache    *lookupCache
	backends map[string]LookupFunc
	http     *http.Client
}

func NewLookupRunner(cfg PreCallConfig) *LookupRunner {
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = 300 * time.Millisecond
	}
	return &LookupRunner{
		cfg:      cfg,
		cache:    newLookupCache(5 * time.Minute),
		backends: map[string]LookupFunc{},
		http:     &http.Client{Timeout: timeout * 2},
	}
}

// Register attaches a named backend (e.g. "ghl") to the runner. The main
// package wires its connectors in via this hook so the voice package stays
// independent of draftyard's internal connectors.
func (l *LookupRunner) Register(name string, fn LookupFunc) {
	l.backends[name] = fn
}

func (l *LookupRunner) Run(ctx context.Context, callerPhone string) PreCallResult {
	if v, ok := l.cache.get(callerPhone); ok {
		return v
	}

	contextVars := map[string]any{}
	var warnings []string

	for _, spec := range l.cfg.Lookups {
		switch spec.Source {
		case "custom_http":
			data, err := l.runCustomHTTP(ctx, spec, callerPhone)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("custom_http (%s): %v", spec.URL, err))
				continue
			}
			mergeMap(contextVars, data)
		default:
			fn, ok := l.backends[spec.Source]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("lookup source %q has no backend registered", spec.Source))
				continue
			}
			data, err := fn(ctx, callerPhone)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s lookup: %v", spec.Source, err))
				continue
			}
			mergeMap(contextVars, data)
		}
	}

	out := PreCallResult{
		Workflow: l.applyRouting(contextVars),
		Warnings: warnings,
	}
	if len(contextVars) > 0 {
		if buf, err := json.Marshal(contextVars); err == nil {
			out.ContextVars = buf
		}
	}
	l.cache.put(callerPhone, out)
	return out
}

func (l *LookupRunner) runCustomHTTP(ctx context.Context, spec LookupSpec, callerPhone string) (map[string]any, error) {
	if spec.URL == "" {
		return nil, errors.New("url required")
	}
	body, _ := json.Marshal(map[string]string{"caller_phone": callerPhone})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if spec.Header != "" {
		k, v, found := strings.Cut(spec.Header, "=")
		if found {
			req.Header.Set(strings.TrimSpace(k), resolveEnvRef(strings.TrimSpace(v)))
		}
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (l *LookupRunner) applyRouting(vars map[string]any) string {
	for _, r := range l.cfg.RoutingRules {
		if r.If == "" || r.Default != "" {
			continue
		}
		if evalCondition(r.If, vars) {
			return r.Workflow
		}
	}
	for _, r := range l.cfg.RoutingRules {
		if r.Default != "" {
			return r.Default
		}
	}
	return ""
}

func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			continue
		}
		dst[k] = v
	}
}

// evalCondition supports a minimal "<field> == '<value>'" expression. More
// complex routing should be expressed as additional rules rather than richer
// syntax inside one rule.
func evalCondition(cond string, vars map[string]any) bool {
	left, right, found := strings.Cut(cond, "==")
	if !found {
		return false
	}
	field := strings.TrimSpace(left)
	want := strings.Trim(strings.TrimSpace(right), `'"`)
	got, ok := vars[field]
	if !ok {
		return false
	}
	return fmt.Sprint(got) == want
}

var envRefRe = regexp.MustCompile(`\{\{env\.([A-Z_][A-Z0-9_]*)\}\}`)

func resolveEnvRef(s string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		name := envRefRe.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
}

type lookupCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]cacheEntry
}

type cacheEntry struct {
	val PreCallResult
	exp time.Time
}

func newLookupCache(ttl time.Duration) *lookupCache {
	return &lookupCache{ttl: ttl, data: make(map[string]cacheEntry)}
}

func (c *lookupCache) get(k string) (PreCallResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[k]
	if !ok || time.Now().After(e.exp) {
		return PreCallResult{}, false
	}
	return e.val, true
}

func (c *lookupCache) put(k string, v PreCallResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = cacheEntry{val: v, exp: time.Now().Add(c.ttl)}
}
