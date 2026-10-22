package main

// Named gate callers: each agent harness can hold its own bearer token. The
// token decides who is calling, so the agent name on an action is a fact the
// gate established rather than a field the caller filled in, one harness can
// be limited to the tools it needs, and one token can be withdrawn without
// touching the others.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/renezander030/draftcat/internal/config"
)

type gateCallerKey struct{}

func withGateCaller(ctx context.Context, c *config.GateCaller) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, gateCallerKey{}, c)
}

// gateCallerFrom returns the caller a request authenticated as, or nil for the
// shared webhook token.
func gateCallerFrom(ctx context.Context) *config.GateCaller {
	c, _ := ctx.Value(gateCallerKey{}).(*config.GateCaller)
	return c
}

type gateCallerToken struct {
	caller config.GateCaller
	token  []byte
}

// gateAuthenticator maps a bearer token to the shared webhook identity or to
// one named caller.
type gateAuthenticator struct {
	shared        []byte
	requireCaller bool
	callers       []gateCallerToken
}

func newGateAuthenticator(shared []byte, gate config.ToolGateConfig) *gateAuthenticator {
	a := &gateAuthenticator{shared: shared, requireCaller: gate.RequireCaller}
	for _, c := range gate.Callers {
		if tok := os.Getenv(c.TokenEnv); c.TokenEnv != "" && tok != "" {
			a.callers = append(a.callers, gateCallerToken{caller: c, token: []byte(tok)})
		}
	}
	return a
}

// identify checks an Authorization header. It returns the caller (nil for the
// shared token), the key the request's signature is checked with, and whether
// the token is accepted. Every configured token is compared, in constant time,
// so the answer's timing does not reveal which one matched.
func (a *gateAuthenticator) identify(header string) (*config.GateCaller, []byte, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return nil, nil, false
	}
	got := []byte(strings.TrimPrefix(header, prefix))
	var match *config.GateCaller
	var key []byte
	for i := range a.callers {
		if subtle.ConstantTimeCompare(got, a.callers[i].token) == 1 {
			match, key = &a.callers[i].caller, a.callers[i].token
		}
	}
	shared := len(a.shared) > 0 && subtle.ConstantTimeCompare(got, a.shared) == 1
	switch {
	case match != nil:
		return match, key, true
	case shared && !a.requireCaller:
		return nil, a.shared, true
	default:
		return nil, nil, false
	}
}

// middleware authenticates every gate request before next sees it: the bearer
// token decides the caller, a POST body is read once (at most maxBody bytes)
// and its signature, when present or required, is checked with that caller's
// own key. next receives the caller on the request context.
func (a *gateAuthenticator) middleware(maxBody, maxSkew int64, requireSignature bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			caller, key, ok := a.identify(r.Header.Get("Authorization"))
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodPost {
				body, ok := readRequestBody(w, r, maxBody)
				if !ok {
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				sigHeader := r.Header.Get(webhookSigHeader)
				if sigHeader != "" || requireSignature {
					if err := verifyWebhookSignature(sigHeader, body, key, maxSkew, time.Now()); err != nil {
						log.Printf("[tool-gate][security] signature rejected: %v", err)
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(withGateCaller(r.Context(), caller)))
		})
	}
}

// callerDenial is the verdict for a caller asking about a tool it does not
// list.
func callerDenial(c *config.GateCaller, tool string) toolPolicyVerdict {
	return toolPolicyVerdict{
		Decision: "deny", DecidedBy: "allowlist", Rule: "caller.tools", Class: "unlisted",
		Reason: "caller " + c.Name + " may not call " + tool + " — not in its tools",
	}
}

// actionAgent returns the agent an action was asked under, and whether the
// action exists.
func (g *toolGate) actionAgent(id string) (string, bool) {
	if state != nil {
		a, err := state.ToolAction(id)
		if err != nil {
			return "", false
		}
		return a.Agent, true
	}
	g.mu.Lock()
	tk := g.tickets[id]
	g.mu.Unlock()
	if tk == nil {
		return "", false
	}
	return tk.Agent, true
}
