//go:build voice

package voice

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"time"
)

type Server struct {
	cfg     Config
	store   *Store
	lookups *LookupRunner
	token   string
	srv     *http.Server
	gate    CallGate
}

// CallGate holds calls to the operator's daily call caps. Check answers
// whether a new call may start; Admit counts one or refuses it; End records
// how long it ran, up to endedAt. An error wrapping ErrCallRefused is a
// refusal, any other error means the gate could not decide or record.
type CallGate interface {
	Check(ctx context.Context) error
	Admit(ctx context.Context, sessionID string) error
	End(ctx context.Context, sessionID string, endedAt time.Time) error
}

// ErrCallRefused marks a call refused by a daily cap.
var ErrCallRefused = errors.New("call refused")

// SetCallGate attaches the call caps. Without a gate every call is accepted
// and nothing is counted.
func (s *Server) SetCallGate(g CallGate) { s.gate = g }

func NewServer(cfg Config, db *sql.DB) (*Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	token := os.Getenv(cfg.Auth.TokenEnv)
	if cfg.Auth.Method == "bearer" && token == "" {
		return nil, errors.New("voice: bearer auth requires env " + cfg.Auth.TokenEnv)
	}

	store, err := NewStore(db)
	if err != nil {
		return nil, err
	}

	s := &Server{
		cfg:     cfg,
		store:   store,
		lookups: NewLookupRunner(cfg.PreCall),
		token:   token,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /voice/session_start", s.auth(s.handleSessionStart))
	mux.HandleFunc("POST /voice/event", s.auth(s.handleEvent))
	mux.HandleFunc("POST /voice/session_end", s.auth(s.handleSessionEnd))
	mux.HandleFunc("POST /voice/handoff", s.auth(s.handleHandoff))
	mux.HandleFunc("POST /voice/learning", s.auth(s.handleLearning))
	mux.HandleFunc("POST /voice/pre_call_context", s.auth(s.handlePreCallContext))

	s.srv = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

func (s *Server) Start() error {
	log.Printf("voice: HTTP server listening on %s", s.cfg.ListenAddr)
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// Handler is the server's routes, for serving them on another listener or
// driving them in a test.
func (s *Server) Handler() http.Handler { return s.srv.Handler }

func (s *Server) Store() *Store          { return s.store }
func (s *Server) Lookups() *LookupRunner { return s.lookups }

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	if s.cfg.Auth.Method != "bearer" {
		return h
	}
	want := []byte("Bearer " + s.token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}
