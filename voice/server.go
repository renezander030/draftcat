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
}

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
