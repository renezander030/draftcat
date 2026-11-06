// Package httpaction is the http_request pipeline action: one outbound HTTP
// request, released by an approval step, carrying a credential that draftcat
// reads from an environment variable. The agent that drafted the payload never
// holds the credential and never sends the request itself.
//
// The package parses and checks a step's vars (shared by the engine and
// `draftcat validate`), renders the body from the run's data, describes the
// request for the operator, and sends it exactly once.
package httpaction

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults for the optional vars.
const (
	DefaultMethod           = http.MethodPost
	DefaultTimeout          = 30 * time.Second
	DefaultMaxResponseBytes = 1 << 20
	DefaultBodyKey          = "ai_output"
	DefaultAuthHeader       = "Authorization"
	DefaultAuthScheme       = "Bearer"

	// excerptBytes bounds the response text quoted in a failure.
	excerptBytes = 300
)

// KnownVars lists every var the action reads.
var KnownVars = []string{
	"url", "method", "body", "body_key", "headers",
	"auth_env", "auth_scheme", "auth_header",
	"timeout", "expect_status", "max_response_bytes",
}

// keptResponseHeaders are the response headers copied into the run's data.
// Cookies and anything else a receiver may consider private stay out.
var keptResponseHeaders = []string{
	"Content-Type", "Content-Location", "Location", "ETag", "Last-Modified",
	"Retry-After", "Request-Id", "X-Request-Id",
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

var (
	headerNameRE = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	templateRE   = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)
)

// Spec is a parsed http_request step.
type Spec struct {
	URL     *url.URL
	Method  string
	Headers [][2]string // static headers from vars.headers, in config order
	// Body is a template filled from the run's data; empty means BodyKey.
	Body string
	// BodyKey names the data value sent as the body; empty with an empty
	// Body means the request has no body.
	BodyKey          string
	AuthEnv          string
	AuthHeader       string
	AuthScheme       string
	Timeout          time.Duration
	MaxResponseBytes int64
	expect           []statusRange
	expectText       string
}

// FieldError is one problem with one var.
type FieldError struct {
	Var     string
	Message string
}

func (e FieldError) Error() string { return "vars." + e.Var + ": " + e.Message }

type statusRange struct{ lo, hi int }

// Parse reads a step's vars. Every problem is returned, so the validator can
// report them all; the engine refuses the step on the first.
func Parse(vars map[string]string) (Spec, []FieldError) {
	var errs []FieldError
	bad := func(v, format string, args ...interface{}) {
		errs = append(errs, FieldError{Var: v, Message: fmt.Sprintf(format, args...)})
	}
	s := Spec{
		Method:           DefaultMethod,
		Timeout:          DefaultTimeout,
		MaxResponseBytes: DefaultMaxResponseBytes,
		expect:           []statusRange{{200, 299}},
		expectText:       "2xx",
	}

	raw := strings.TrimSpace(vars["url"])
	switch u, err := url.Parse(raw); {
	case raw == "":
		bad("url", "required")
	case err != nil:
		bad("url", "does not parse: %v", err)
	case u.Scheme != "http" && u.Scheme != "https":
		bad("url", "must be an absolute http or https URL (got %q)", raw)
	case u.Host == "" || u.Hostname() == "":
		bad("url", "has no host (got %q)", raw)
	case u.User != nil:
		bad("url", "carries credentials; put the credential in an environment variable and name it in auth_env")
	default:
		s.URL = u
	}

	if m := strings.TrimSpace(vars["method"]); m != "" {
		s.Method = strings.ToUpper(m)
		if !allowedMethods[s.Method] {
			bad("method", "must be GET, POST, PUT, PATCH or DELETE (got %q)", m)
		}
	}

	s.AuthEnv = strings.TrimSpace(vars["auth_env"])
	if s.AuthEnv != "" {
		if !envNameRE.MatchString(s.AuthEnv) {
			bad("auth_env", "%q is not an environment variable name", s.AuthEnv)
		}
		s.AuthHeader = strings.TrimSpace(vars["auth_header"])
		s.AuthScheme = strings.TrimSpace(vars["auth_scheme"])
		if s.AuthHeader == "" {
			s.AuthHeader = DefaultAuthHeader
			if s.AuthScheme == "" {
				s.AuthScheme = DefaultAuthScheme
			}
		}
		if !headerNameRE.MatchString(s.AuthHeader) {
			bad("auth_header", "%q is not a header name; the credential header cannot be formed", s.AuthHeader)
		} else if reservedHeader(s.AuthHeader) != "" {
			bad("auth_header", "%q %s", s.AuthHeader, reservedHeader(s.AuthHeader))
		}
		if s.AuthScheme != "" && !headerNameRE.MatchString(s.AuthScheme) {
			bad("auth_scheme", "%q is not a single word such as Bearer; the credential header cannot be formed", s.AuthScheme)
		}
	}

	for i, line := range strings.Split(vars["headers"], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case !ok || !headerNameRE.MatchString(name):
			bad("headers", "line %d is not \"Name: value\" (got %q)", i+1, line)
		case strings.EqualFold(name, DefaultAuthHeader), strings.EqualFold(name, "Proxy-Authorization"),
			s.AuthEnv != "" && strings.EqualFold(name, s.AuthHeader):
			bad("headers", "%s carries a credential; name the environment variable in auth_env instead", name)
		case reservedHeader(name) != "":
			bad("headers", "%s %s", name, reservedHeader(name))
		default:
			s.Headers = append(s.Headers, [2]string{http.CanonicalHeaderKey(name), value})
		}
	}

	s.Body = vars["body"]
	s.BodyKey = strings.TrimSpace(vars["body_key"])
	switch {
	case s.Body != "" && s.BodyKey != "":
		bad("body", "set either body or body_key, not both")
	case s.Method == http.MethodGet && (s.Body != "" || s.BodyKey != ""):
		bad("body", "a GET request carries no body")
	case s.Body == "" && s.BodyKey == "" && s.Method != http.MethodGet && s.Method != http.MethodDelete:
		s.BodyKey = DefaultBodyKey
	}

	if v := strings.TrimSpace(vars["timeout"]); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			bad("timeout", "must be a positive duration such as 30s (got %q)", v)
		} else {
			s.Timeout = d
		}
	}

	if v := strings.TrimSpace(vars["expect_status"]); v != "" {
		ranges, err := parseExpect(v)
		if err != nil {
			bad("expect_status", "%v", err)
		} else {
			s.expect, s.expectText = ranges, v
		}
	}

	if v := strings.TrimSpace(vars["max_response_bytes"]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			bad("max_response_bytes", "must be a positive byte count (got %q)", v)
		} else {
			s.MaxResponseBytes = n
		}
	}
	return s, errs
}

// reservedHeader explains why a header may not be set from config, or "".
func reservedHeader(name string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Idempotency-Key":
		return "is set by draftcat for every request"
	case "Host", "Content-Length", "Transfer-Encoding", "Connection":
		return "is set by the HTTP client"
	}
	return ""
}

func parseExpect(v string) ([]statusRange, error) {
	var out []statusRange
	for _, part := range strings.Split(v, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if len(p) == 3 && p[1:] == "xx" && p[0] >= '1' && p[0] <= '5' {
			lo := int(p[0]-'0') * 100
			out = append(out, statusRange{lo, lo + 99})
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 100 || n > 599 {
			return nil, fmt.Errorf("%q is not a status code (200) or class (2xx)", strings.TrimSpace(part))
		}
		out = append(out, statusRange{n, n})
	}
	return out, nil
}

// Expected reports whether status counts as success.
func (s Spec) Expected(status int) bool {
	for _, r := range s.expect {
		if status >= r.lo && status <= r.hi {
			return true
		}
	}
	return false
}

// PlainRemote reports a plain http:// URL to a host other than loopback: the
// payload, and any credential, would cross the network unencrypted.
func (s Spec) PlainRemote() bool {
	if s.URL == nil || s.URL.Scheme != "http" {
		return false
	}
	host := s.URL.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

// contentType is the body's media type: a static Content-Type header, or JSON.
func (s Spec) contentType() string {
	for _, h := range s.Headers {
		if h[0] == "Content-Type" {
			return h[1]
		}
	}
	return "application/json"
}

func (s Spec) jsonBody() bool {
	ct := strings.ToLower(s.contentType())
	return strings.Contains(ct, "json")
}

// IdempotencyKey is the Idempotency-Key header for one step of one run. A
// receiver that has seen the key can drop a repeat of the same request.
func IdempotencyKey(runID, step string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + step))
	return hex.EncodeToString(sum[:16])
}

// Body renders the request body from the run's data. A JSON body is checked
// to be valid JSON before anything is sent.
func Body(s Spec, data map[string]interface{}) ([]byte, error) {
	jsonCT := s.jsonBody()
	switch {
	case s.Body != "":
		var missing []string
		var encErr error
		out := templateRE.ReplaceAllStringFunc(s.Body, func(m string) string {
			key := templateRE.FindStringSubmatch(m)[1]
			v, ok := data[key]
			if !ok {
				missing = append(missing, key)
				return m
			}
			if str, isStr := v.(string); isStr && !jsonCT {
				return str
			}
			b, err := marshal(v, false)
			if err != nil {
				encErr = fmt.Errorf("body: {{%s}}: %w", key, err)
			}
			return string(b)
		})
		if len(missing) > 0 {
			return nil, fmt.Errorf("body: no run data for {{%s}}", strings.Join(missing, "}}, {{"))
		}
		if encErr != nil {
			return nil, encErr
		}
		if jsonCT && !json.Valid([]byte(out)) {
			return nil, errors.New("body: the filled template is not valid JSON")
		}
		return []byte(out), nil
	case s.BodyKey != "":
		v, ok := data[s.BodyKey]
		if !ok {
			return nil, fmt.Errorf("body: run data has no %q (set vars.body or vars.body_key)", s.BodyKey)
		}
		if str, isStr := v.(string); isStr {
			trimmed := strings.TrimSpace(str)
			// A model's raw text that is a JSON object or array is sent as
			// that document, not as a quoted string.
			if !jsonCT || ((strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed))) {
				return []byte(str), nil
			}
		}
		return marshal(v, true)
	}
	return nil, nil
}

func marshal(v interface{}, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Describe is the request as the operator reviews it: method, URL, every
// header draftcat sends (the credential named, never shown) and the body.
func Describe(s Spec, body []byte, idempotencyKey string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", s.Method, s.URL.String())
	if body != nil {
		fmt.Fprintf(&b, "Content-Type: %s\n", s.contentType())
	}
	for _, h := range s.Headers {
		if h[0] == "Content-Type" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", h[0], h[1])
	}
	if s.AuthEnv != "" {
		value := "<credential from " + s.AuthEnv + ">"
		if s.AuthScheme != "" {
			value = s.AuthScheme + " " + value
		}
		fmt.Fprintf(&b, "%s: %s\n", s.AuthHeader, value)
	}
	fmt.Fprintf(&b, "Idempotency-Key: %s\n", idempotencyKey)
	if body != nil {
		b.WriteString("\n")
		b.Write(body)
	}
	return b.String()
}

// Digest binds an approval to one request: the same spec, body and key give
// the same digest, and any change to them gives another.
func Digest(s Spec, body []byte, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(Describe(s, body, idempotencyKey)))
	return hex.EncodeToString(sum[:])
}

// Response is what a sent request returned.
type Response struct {
	Status    int
	Headers   map[string]string
	Text      string      // the body as text, at most MaxResponseBytes
	JSON      interface{} // the body parsed, when it is complete JSON
	Truncated bool
}

// Data is the response as later steps read it from the run's data.
func (r Response) Data(idempotencyKey string) map[string]interface{} {
	var body interface{} = r.Text
	if r.JSON != nil {
		body = r.JSON
	}
	return map[string]interface{}{
		"status":          r.Status,
		"headers":         r.Headers,
		"body":            body,
		"truncated":       r.Truncated,
		"idempotency_key": idempotencyKey,
	}
}

// client never follows a redirect: a custom credential header would travel
// to whatever host the redirect names. A 3xx is returned as the response.
var client = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Do sends the request once. It never retries: a resend could repeat the
// side effect the operator approved once. credential is the value of
// s.AuthEnv; it is never part of a returned error.
func Do(ctx context.Context, s Spec, body []byte, idempotencyKey, credential string) (Response, error) {
	scrub := func(text string) string {
		if credential == "" {
			return text
		}
		return strings.ReplaceAll(text, credential, "[REDACTED:"+s.AuthEnv+"]")
	}
	if s.URL == nil {
		return Response{}, errors.New("no url")
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, s.Method, s.URL.String(), reader)
	if err != nil {
		return Response{}, errors.New(scrub(err.Error()))
	}
	if body != nil {
		req.Header.Set("Content-Type", s.contentType())
	}
	for _, h := range s.Headers {
		req.Header.Add(h[0], h[1])
	}
	if s.AuthEnv != "" {
		value := credential
		if s.AuthScheme != "" {
			value = s.AuthScheme + " " + credential
		}
		req.Header.Set(s.AuthHeader, value)
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)

	target := s.Method + " " + s.URL.Redacted()
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Response{}, fmt.Errorf("%s: no response before the deadline (step timeout %s); the receiver may or may not have acted on it", target, s.Timeout)
		}
		return Response{}, fmt.Errorf("%s: %s", target, scrub(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, s.MaxResponseBytes+1))
	out := Response{Status: resp.StatusCode, Headers: map[string]string{}}
	if int64(len(raw)) > s.MaxResponseBytes {
		raw, out.Truncated = raw[:s.MaxResponseBytes], true
	}
	out.Text = scrub(string(raw))
	for _, name := range keptResponseHeaders {
		if v := resp.Header.Get(name); v != "" {
			out.Headers[name] = scrub(v)
		}
	}

	if !s.Expected(resp.StatusCode) {
		excerpt := out.Text
		if len(excerpt) > excerptBytes {
			excerpt = excerpt[:excerptBytes] + "…"
		}
		excerpt = strings.Join(strings.Fields(excerpt), " ")
		return out, fmt.Errorf("%s returned HTTP %d, expected %s: %s", target, resp.StatusCode, s.expectText, excerpt)
	}
	if readErr != nil {
		return out, fmt.Errorf("%s returned HTTP %d but its body could not be read: %s", target, resp.StatusCode, scrub(readErr.Error()))
	}
	if !out.Truncated && json.Valid(raw) {
		dec := json.NewDecoder(bytes.NewReader([]byte(out.Text)))
		dec.UseNumber() // ids stay exact
		var v interface{}
		if dec.Decode(&v) == nil {
			out.JSON = v
		}
	}
	return out, nil
}

// UnknownVars returns the keys of vars that the action does not read, sorted.
func UnknownVars(vars map[string]string) []string {
	known := map[string]bool{}
	for _, k := range KnownVars {
		known[k] = true
	}
	var out []string
	for k := range vars {
		if !known[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
